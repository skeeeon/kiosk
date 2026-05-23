package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/pocketbase/pocketbase"
	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/plugins/migratecmd"

	"github.com/skeeeon/kiosk/internal/authfix"
	"github.com/skeeeon/kiosk/internal/cart"
	"github.com/skeeeon/kiosk/internal/catalog"
	"github.com/skeeeon/kiosk/internal/config"
	"github.com/skeeeon/kiosk/internal/events"
	"github.com/skeeeon/kiosk/internal/handlers"
	"github.com/skeeeon/kiosk/internal/kioskctx"
	"github.com/skeeeon/kiosk/internal/notifications"

	// Register schema migrations via init() side effects.
	_ "github.com/skeeeon/kiosk/migrations"
)

// sendLogRetentionDays bounds the notification_send_log table. 90 days is
// enough lookback to debug "did that alert fire last quarter?" without the
// table growing unbounded on a busy kiosk. Configurability is intentionally
// deferred until someone asks.
const sendLogRetentionDays = 90

func main() {
	cfg, err := config.Load(configPath())
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	kioskctx.Set(kioskctx.Identity{
		KioskCode:    cfg.Kiosk.Code,
		LocationCode: cfg.Kiosk.LocationCode,
	})

	// Install the configured NATS subject prefix before any code path that
	// might publish (or log) an event runs. Empty falls back to the default.
	events.SetSubjectPrefix(cfg.NATS.SubjectPrefix)

	app := pocketbase.New()

	// Apply registered Go migrations automatically on startup.
	migratecmd.MustRegister(app, app.RootCmd, migratecmd.Config{
		Automigrate: true,
	})

	authfix.EnforceEmailVisibility(app)

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
				cfg.Controller.CatalogUsersBucket,
				cfg.Controller.CatalogGroupsBucket,
				cfg.Kiosk.Code)
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
	notifier := notifications.New(app)
	h := handlers.New(app, cfg, carts, notifier)

	// Scheduled reports register their cron jobs at boot and react to
	// record-hook changes thereafter — adding/editing/deleting a row in
	// the SPA reflects in app.Cron() without a restart.
	bindScheduledReportsHooks(app, notifier)
	app.OnServe().BindFunc(func(e *core.ServeEvent) error {
		registerScheduledReports(app, notifier)
		return e.Next()
	})

	// Daily retention pass on the notifications send log + dedupe table.
	// Runs at 03:15 local time — well outside the kiosk's busy windows. PB's
	// Cron is a process-local scheduler; if the kiosk is down at fire time,
	// the next live tick handles the backlog on its next eligible slot.
	app.Cron().Add("notifications_retention", "15 3 * * *", func() {
		cutoff := time.Now().UTC().AddDate(0, 0, -sendLogRetentionDays).Format("2006-01-02 15:04:05.000Z")
		if deleted, err := notifier.PruneSendLog(cutoff); err != nil {
			log.Printf("send log prune: %v", err)
		} else if deleted > 0 {
			log.Printf("send log prune: removed %d rows older than %d days", deleted, sendLogRetentionDays)
		}
		if deleted, err := notifier.PruneDedupe(cutoff); err != nil {
			log.Printf("dedupe prune: %v", err)
		} else if deleted > 0 {
			log.Printf("dedupe prune: removed %d rows older than %d days", deleted, sendLogRetentionDays)
		}
	})

	app.OnServe().BindFunc(func(e *core.ServeEvent) error {
		e.Router.GET("/health", func(re *core.RequestEvent) error {
			return re.JSON(200, map[string]string{"status": "ok"})
		})
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
		e.Router.GET("/api/kiosk/reports/open-checkouts", h.ReportOpenCheckouts)
		e.Router.POST("/api/kiosk/ledger/republish", h.RepublishLedger)
		e.Router.POST("/api/kiosk/items/import", h.CSVImport)
		e.Router.GET("/api/kiosk/items.csv", h.ItemsExportCSV)
		e.Router.POST("/api/kiosk/items/{id}/adjust", h.AdjustItemStock)
		e.Router.GET("/api/kiosk/transactions.csv", h.TransactionsExportCSV)
		e.Router.GET("/api/kiosk/notifications", h.ListNotificationTemplates)
		e.Router.PATCH("/api/kiosk/notifications/{event_type}", h.UpdateNotificationTemplate)
		e.Router.GET("/api/kiosk/notifications/{event_type}/defaults", h.GetNotificationTemplateDefaults)

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
