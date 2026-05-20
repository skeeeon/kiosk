package main

import (
	"fmt"
	"log"
	"os"

	"github.com/pocketbase/pocketbase"
	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/plugins/migratecmd"

	"github.com/skeeeon/kiosk/internal/cart"
	"github.com/skeeeon/kiosk/internal/config"
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
		e.Router.POST("/api/kiosk/items/import", h.CSVImport)

		// Serve the Vue SPA from pb_public. indexFallback=true means unknown
		// paths return index.html so client-side routes (/admin/*) resolve.
		// PocketBase's own /api/* and /_/* routes win on specificity.
		e.Router.GET("/{path...}", apis.Static(os.DirFS("./pb_public"), true))

		return e.Next()
	})

	log.Printf("starting kiosk %s at %s on %s:%d",
		cfg.Kiosk.Code, cfg.Kiosk.LocationCode, cfg.Server.Bind, cfg.Server.Port)

	if len(os.Args) == 1 {
		os.Args = append(os.Args, "serve",
			fmt.Sprintf("--http=%s:%d", cfg.Server.Bind, cfg.Server.Port))
	}

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
