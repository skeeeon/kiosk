package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/pocketbase/pocketbase"
	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/plugins/migratecmd"

	"github.com/skeeeon/kiosk/internal/cart"
	"github.com/skeeeon/kiosk/internal/catalog"
	"github.com/skeeeon/kiosk/internal/config"
	"github.com/skeeeon/kiosk/internal/events"
	"github.com/skeeeon/kiosk/internal/handlers"
	"github.com/skeeeon/kiosk/internal/kioskctx"

	// Register schema migrations via init() side effects.
	_ "github.com/skeeeon/kiosk/migrations"
)

func main() {
	cfg, err := config.Load(configPath())
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	kioskctx.Set(kioskctx.Identity{
		KioskCode:    cfg.Kiosk.Code,
		LocationCode: cfg.Kiosk.LocationCode,
	})

	app := pocketbase.New()

	// Apply registered Go migrations automatically on startup.
	migratecmd.MustRegister(app, app.RootCmd, migratecmd.Config{
		Automigrate: true,
	})

	// NATS is best-effort: a misconfigured or unreachable endpoint must not
	// block the kiosk from starting (the local ledger is authoritative).
	// Connect itself doesn't fail on unreachable servers — it returns a
	// buffering connection. Errors here are structural (empty URL, bad
	// creds file, etc.); log and proceed without a publisher.
	pub, err := events.Connect(cfg.NATS)
	if err != nil {
		log.Printf("nats: continuing without event publishing — %v", err)
	}
	events.SetPublisher(pub)

	// Managed mode: subscribe to the controller's catalog KV buckets and
	// project changes into local items/users. Watcher is best-effort —
	// failures here log but don't block kiosk startup, because the kiosk
	// can still serve checkouts against whatever catalog state it has.
	var catalogWatcher *catalog.Watcher
	watcherCtx, watcherCancel := context.WithCancel(context.Background())
	if cfg.Controller.Enabled {
		if pub == nil {
			log.Printf("controller.enabled=true but nats is not connected — catalog will not sync")
		} else if js, err := events.JetStream(pub); err != nil {
			log.Printf("controller.enabled=true but jetstream unavailable: %v", err)
		} else {
			catalogWatcher = catalog.NewWatcher(app, js,
				cfg.Controller.CatalogItemsBucket,
				cfg.Controller.CatalogUsersBucket)
			app.OnServe().BindFunc(func(e *core.ServeEvent) error {
				if err := catalogWatcher.Start(watcherCtx); err != nil {
					log.Printf("catalog watcher: %v — kiosk will continue without sync", err)
					catalogWatcher = nil
				}
				return e.Next()
			})
		}
	}

	app.OnTerminate().BindFunc(func(e *core.TerminateEvent) error {
		if catalogWatcher != nil {
			catalogWatcher.Stop()
		}
		watcherCancel()
		if p := events.CurrentPublisher(); p != nil {
			p.Close()
		}
		return e.Next()
	})

	carts := cart.NewStore(cfg.Session.IdleTimeout.AsDuration())
	h := handlers.New(app, cfg, carts)

	app.OnServe().BindFunc(func(e *core.ServeEvent) error {
		e.Router.GET("/api/kiosk/identity", h.Identity)
		e.Router.POST("/api/kiosk/scan", h.Scan)
		e.Router.GET("/api/kiosk/items", h.ItemsList)
		e.Router.GET("/branding/logo", h.Logo)
		e.Router.POST("/api/kiosk/cart/start", h.CartStart)
		e.Router.POST("/api/kiosk/cart/add", h.CartAdd)
		e.Router.PATCH("/api/kiosk/cart/lines/{id}", h.CartUpdateLine)
		e.Router.DELETE("/api/kiosk/cart/lines/{id}", h.CartDeleteLine)
		e.Router.POST("/api/kiosk/cart/cancel", h.CartCancel)
		e.Router.POST("/api/kiosk/cart/commit", h.CartCommit)
		e.Router.GET("/api/kiosk/integrity", h.Integrity)
		e.Router.POST("/api/kiosk/integrity/rebuild", h.RebuildOpenCheckouts)
		e.Router.POST("/api/kiosk/items/import", h.CSVImport)
		e.Router.GET("/api/kiosk/items.csv", h.ItemsExportCSV)
		e.Router.POST("/api/kiosk/items/{id}/adjust", h.AdjustItemStock)
		e.Router.GET("/api/kiosk/transactions.csv", h.TransactionsExportCSV)

		// Serve the Vue SPA from pb_public. indexFallback=true means unknown
		// paths return index.html so client-side routes (/admin/*) resolve.
		// PocketBase's own /api/* and /_/* routes win on specificity.
		e.Router.GET("/{path...}", apis.Static(os.DirFS("./pb_public"), true))

		return e.Next()
	})

	log.Printf("starting kiosk %s at %s on %s:%d",
		cfg.Kiosk.Code, cfg.Kiosk.LocationCode, cfg.Server.Bind, cfg.Server.Port)

	os.Args = config.EnsureServeBind(os.Args,
		fmt.Sprintf("%s:%d", cfg.Server.Bind, cfg.Server.Port))

	if err := app.Start(); err != nil {
		log.Fatal(err)
	}
}

func configPath() string {
	if p := os.Getenv("KIOSK_CONFIG"); p != "" {
		return p
	}
	return "kiosk.yaml"
}
