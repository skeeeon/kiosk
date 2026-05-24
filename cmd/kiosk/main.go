package main

import (
	"context"
	"encoding/json"
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
	"github.com/skeeeon/kiosk/internal/commands"
	"github.com/skeeeon/kiosk/internal/config"
	"github.com/skeeeon/kiosk/internal/events"
	"github.com/skeeeon/kiosk/internal/handlers"
	"github.com/skeeeon/kiosk/internal/heartbeat"
	"github.com/skeeeon/kiosk/internal/instances"
	"github.com/skeeeon/kiosk/internal/kioskctx"
	"github.com/skeeeon/kiosk/internal/notifications"
	"github.com/skeeeon/kiosk/internal/scheduler"

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

	// Command bus + heartbeat: best-effort, NATS-only. Kiosks without NATS
	// boot and serve local checkouts normally; only remote admin from the
	// controller and the fleet-liveness indicator depend on this wiring.
	var commandSub interface{ Unsubscribe() error }
	heartbeatCtx, heartbeatCancel := context.WithCancel(context.Background())
	if pub != nil && cfg.NATS.Enabled {
		nc, err := events.Conn(pub)
		switch {
		case err != nil:
			log.Printf("commands/heartbeat: nats connection unavailable: %v", err)
		case cfg.Kiosk.Code == "":
			// Defense in depth — config validation already enforces this,
			// but the dispatcher won't subscribe without a code.
			log.Printf("commands/heartbeat: kiosk.code is empty — skipping")
		default:
			disp := commands.NewDispatcher(app, cfg.Kiosk.Code)
			app.OnServe().BindFunc(func(e *core.ServeEvent) error {
				sub, err := disp.Register(nc)
				if err != nil {
					log.Printf("commands: subscribe failed — %v", err)
				} else {
					commandSub = sub
				}
				return e.Next()
			})
			app.OnServe().BindFunc(func(e *core.ServeEvent) error {
				// Empty version string is fine — telemetry only. ldflags-injected
				// version can be added later without touching this signature.
				heartbeat.Start(heartbeatCtx, nc, cfg.Kiosk.Code, "")
				return e.Next()
			})
		}
	}

	app.OnTerminate().BindFunc(func(e *core.TerminateEvent) error {
		if catalogWatcher != nil {
			catalogWatcher.Stop()
		}
		watcherCancel()
		heartbeatCancel()
		if commandSub != nil {
			_ = commandSub.Unsubscribe()
		}
		if p := events.CurrentPublisher(); p != nil {
			p.Close()
		}
		return e.Next()
	})

	carts := cart.NewStore(cfg.Session.IdleTimeout.AsDuration())
	notifier := notifications.New(app)
	h := handlers.New(app, cfg, carts, notifier)

	// PB record hooks on item_instances: create / decommission (active flip)
	// / delete write an instance_audit row + publish an instance.lifecycle
	// event. Cosmetic edits don't audit. Only the kiosk binary mutates
	// instances (the controller SPA hides the instances panel), so this
	// registration lives here exclusively.
	instances.New().Register(app)

	// Scheduled reports register their cron jobs at boot and react to
	// record-hook changes thereafter — adding/editing/deleting a row in
	// the SPA reflects in app.Cron() without a restart.
	//
	// In managed mode the scheduler computes the digest locally (it needs
	// open_checkouts state, which only the kiosk has) but publishes a
	// notifications.DigestEnvelope over NATS so the controller does the
	// actual SMTP send and writes its own send_log row. In standalone
	// mode the local Notifier owns the full send path as before.
	var send scheduler.Sender = notifier.SendTo
	if cfg.Controller.Enabled {
		kioskCode := cfg.Kiosk.Code
		send = func(eventType string, data any, recipients notifications.Recipients) error {
			subject, err := digestSubjectFor(eventType, kioskCode)
			if err != nil {
				return err
			}
			payload, err := json.Marshal(data)
			if err != nil {
				return fmt.Errorf("managed-mode scheduler: marshal %q payload: %w", eventType, err)
			}
			events.Publish(subject, notifications.DigestEnvelope{
				EventType:  eventType,
				Context:    payload,
				Recipients: recipients,
			})
			// events.Publish is fire-and-forget — slogs on failure, no
			// error return. Treat a publish as "accepted" for the schedule
			// row's last_status; the actual send outcome lives on the
			// controller's notification_send_log.
			return nil
		}
	}
	scheduler.BindRecordHooks(app, send)
	app.OnServe().BindFunc(func(e *core.ServeEvent) error {
		scheduler.RegisterEnabled(app, send)
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
		e.Router.GET("/branding/custom.css", h.CustomCSS)
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
		e.Router.POST("/api/kiosk/checkouts/by-line/{transaction_line_id}/close", h.AdminCloseCheckout)
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

// digestSubjectFor maps a digest event type to its kiosk-scoped NATS
// subject. Adding a new digest means adding both an event-type constant
// in internal/notifications and a case here.
func digestSubjectFor(eventType, kioskCode string) (string, error) {
	switch eventType {
	case notifications.EventTypeOpenChecksDigest:
		return events.OpenChecksDigestSubject(kioskCode), nil
	case notifications.EventTypeDailyActivity:
		return events.DailyActivityDigestSubject(kioskCode), nil
	}
	return "", fmt.Errorf("managed-mode scheduler: no NATS subject for event %q", eventType)
}
