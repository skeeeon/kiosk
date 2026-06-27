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
	"github.com/skeeeon/kiosk/internal/commands"
	"github.com/skeeeon/kiosk/internal/config"
	"github.com/skeeeon/kiosk/internal/events"
	"github.com/skeeeon/kiosk/internal/handlers"
	"github.com/skeeeon/kiosk/internal/heartbeat"
	"github.com/skeeeon/kiosk/internal/instances"
	"github.com/skeeeon/kiosk/internal/kioskctx"
	"github.com/skeeeon/kiosk/internal/notifications"
	"github.com/skeeeon/kiosk/internal/rfid"
	"github.com/skeeeon/kiosk/internal/scheduler"
	"github.com/skeeeon/kiosk/internal/sightings"
	"github.com/skeeeon/kiosk/internal/timeclock"
	"github.com/skeeeon/kiosk/internal/ui"

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
	pub, err := events.Connect(cfg.NATS, "kiosk-"+cfg.Kiosk.Code)
	if err != nil {
		log.Printf("nats: continuing without event publishing — %v", err)
	}
	events.SetPublisher(pub)

	// Managed mode: subscribe to the controller's catalog KV buckets and
	// project changes into local items/users. Watcher is best-effort —
	// failures here log but don't block kiosk startup, because the kiosk
	// can still serve checkouts against whatever catalog state it has.
	var (
		catalogWatcher  *catalog.Watcher
		punchFleet      *timeclock.Fleet
		punchWatcher    *timeclock.Watcher
		checkoutFleet   *timeclock.CheckoutFleet
		checkoutWatcher *timeclock.CheckoutWatcher
		mirrorWatcher   *sightings.MirrorWatcher
	)
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
			// Fleet-wide clocked-in state replica (managed + timeclock only).
			// Best-effort like the catalog watcher: a failure (e.g. the
			// controller hasn't provisioned the bucket yet) degrades the
			// merge rule to local-only punch state.
			if cfg.Timeclock.Enabled {
				punchFleet = timeclock.NewFleet()
				punchWatcher = timeclock.NewWatcher(js, punchFleet)
				app.OnServe().BindFunc(func(e *core.ServeEvent) error {
					if err := punchWatcher.Start(watcherCtx); err != nil {
						log.Printf("timeclock watcher: %v — kiosk will continue with local-only punch state", err)
						punchWatcher = nil
					}
					return e.Next()
				})
				// Fleet-wide open-checkout replica feeding the cross-kiosk
				// clock-out gate. Same best-effort posture: a failure degrades
				// the gate to local-only open checkouts.
				checkoutFleet = timeclock.NewCheckoutFleet()
				checkoutWatcher = timeclock.NewCheckoutWatcher(js, checkoutFleet)
				app.OnServe().BindFunc(func(e *core.ServeEvent) error {
					if err := checkoutWatcher.Start(watcherCtx); err != nil {
						log.Printf("timeclock checkout watcher: %v — kiosk will continue with local-only open-checkout gate", err)
						checkoutWatcher = nil
					}
					return e.Next()
				})
			}

			// Fleet last-observed mirror (location/sightings L3): hydrates this
			// node's OWN slice of the controller's last_observed_state bucket into
			// its item_instances.last_observed_* columns, so a unit owned here but
			// seen by another site's gateway shows its true last-seen locally.
			// Best-effort, same posture as the catalog watcher.
			mirrorWatcher = sightings.NewMirrorWatcher(app, js, cfg.Kiosk.Code, "")
			app.OnServe().BindFunc(func(e *core.ServeEvent) error {
				if err := mirrorWatcher.Start(watcherCtx); err != nil {
					log.Printf("sighting mirror watcher: %v — kiosk will continue with local-gateway data only", err)
					mirrorWatcher = nil
				}
				return e.Next()
			})
		}
	}

	// Command bus + heartbeat: best-effort, NATS-only. Kiosks without NATS
	// boot and serve local checkouts normally; only remote admin from the
	// controller and the fleet-liveness indicator depend on this wiring.
	var (
		commandSub interface{ Unsubscribe() error }
		disp       *commands.Dispatcher // captured here so we can set KioskHandlers below
	)
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
			disp = commands.NewDispatcher(app, cfg.Kiosk.Code)
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

	var rfidReaders []rfid.Reader

	app.OnTerminate().BindFunc(func(e *core.TerminateEvent) error {
		if catalogWatcher != nil {
			catalogWatcher.Stop()
		}
		if mirrorWatcher != nil {
			mirrorWatcher.Stop()
		}
		if punchWatcher != nil {
			punchWatcher.Stop()
		}
		if checkoutWatcher != nil {
			checkoutWatcher.Stop()
		}
		watcherCancel()
		heartbeatCancel()
		if commandSub != nil {
			_ = commandSub.Unsubscribe()
		}
		for _, r := range rfidReaders {
			_ = r.Close()
		}
		if p := events.CurrentPublisher(); p != nil {
			p.Close()
		}
		return e.Next()
	})

	carts := cart.NewStore(cfg.Session.IdleTimeout.AsDuration())
	notifier := notifications.New(app)
	// Drain in-flight notification goroutines on shutdown before PB tears the
	// DB down — a deliver() waking after the DB closes would panic inside
	// FindCollectionByNameOrId (see the Notifier docs). Bounded best-effort.
	app.OnTerminate().BindFunc(func(e *core.TerminateEvent) error {
		notifier.WaitInFlight(2 * time.Second)
		return e.Next()
	})
	h := handlers.New(app, cfg, carts, notifier)
	// nil on standalone kiosks / when timeclock is off — the timeclock
	// merge rule is nil-safe and degrades to local-only punch state.
	h.PunchFleet = punchFleet
	// nil on standalone / when timeclock is off — the clock-out gate is
	// nil-safe and degrades to local-only open checkouts.
	h.CheckoutFleet = checkoutFleet

	// Phase-4 enclosure_diff commands (cart.start, read.trigger) reach
	// into the cart store and SSE broker via KioskHandlers. Set it
	// here, after h exists; safe to do unconditionally (no command
	// has fired yet — subscription happens via OnServe below) and
	// nil-safe for builds without NATS.
	if disp != nil {
		disp.KioskHandlers = h
	}

	// RFID reader. The supervisor inside rfid.Connect dials in the
	// background and retries with backoff, so we wire h.RFID up
	// unconditionally and let ReadFor return ErrNotConnected during
	// any gaps — the SPA's RFID button then hits 503 gracefully
	// instead of blocking boot.
	if cfg.RFID.Enabled {
		h.Readers = make(map[string]*handlers.ReaderHandle, len(cfg.RFID.Readers))
		for id, rc := range cfg.RFID.Readers {
			hd := &handlers.ReaderHandle{ID: id, Mode: rc.Mode, EnclosureID: rc.EnclosureID, Zone: rc.Zone}
			if r, err := rfid.New(rc); err != nil {
				log.Printf("rfid: reader %q: %v — continuing without it", id, err)
			} else {
				hd.Reader = r
				rfidReaders = append(rfidReaders, r)
			}
			h.Readers[id] = hd
		}
		// Connect each reader's supervisor on serve — it dials in the
		// background and retries with backoff, so ReadFor returns
		// ErrNotConnected (503 to the SPA) during any gap rather than
		// blocking boot.
		app.OnServe().BindFunc(func(e *core.ServeEvent) error {
			for id, hd := range h.Readers {
				if hd.Reader != nil {
					_ = hd.Reader.Connect()
					log.Printf("rfid: supervisor started for reader %q (%s mode)", id, hd.Mode)
				}
			}
			return e.Next()
		})
	}

	// Standalone sighting ingest: a node with NATS but no controller subscribes
	// to its OWN sighting subject and resolves each raw sighting locally via the
	// scan resolver, stamping advisory last-observed (docs/location-sightings-
	// plan.md, L2). Gated on !Controller.Enabled — in managed mode the controller
	// is the sole sighting subscriber and mirrors last-observed back via KV (L3).
	var sightingSub interface{ Unsubscribe() error }
	if pub != nil && cfg.NATS.Enabled && !cfg.Controller.Enabled && cfg.Kiosk.Code != "" {
		if nc, err := events.Conn(pub); err != nil {
			log.Printf("sightings: nats connection unavailable: %v", err)
		} else {
			app.OnServe().BindFunc(func(e *core.ServeEvent) error {
				sub, err := sightings.Subscribe(nc, app, cfg.Kiosk.Code, h.LookupInstanceIDByEPC)
				if err != nil {
					log.Printf("sightings: subscribe failed — %v", err)
				} else {
					sightingSub = sub
				}
				return e.Next()
			})
			app.OnTerminate().BindFunc(func(e *core.TerminateEvent) error {
				if sightingSub != nil {
					_ = sightingSub.Unsubscribe()
				}
				return e.Next()
			})
		}
	}

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
	// Standalone deployments only. In managed mode the controller owns
	// the schedule rows, the cron, and the SMTP send — it has the
	// fleet-wide projected ledger and the central template config, so
	// the previous "kiosk computes, NATS-ships to controller for send"
	// detour goes away. The kiosk's SPA hides the view in managed mode
	// (see AdminScheduledReportsView's role gate).
	if !cfg.Controller.Enabled {
		scheduler.BindRecordHooks(app, notifier.SendTo)
		app.OnServe().BindFunc(func(e *core.ServeEvent) error {
			scheduler.RegisterEnabled(app, notifier.SendTo)
			return e.Next()
		})
	}

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
		e.Router.GET("/api/kiosk/cart/foreman-return/options", h.CartForemanReturnOptions)
		e.Router.POST("/api/kiosk/cart/foreman-return", h.CartForemanReturn)
		e.Router.PATCH("/api/kiosk/cart/lines/{id}", h.CartUpdateLine)
		e.Router.DELETE("/api/kiosk/cart/lines/{id}", h.CartDeleteLine)
		e.Router.GET("/api/kiosk/cart", h.CartGet)
		e.Router.GET("/api/kiosk/cart/events", h.CartEventsStream)
		e.Router.POST("/api/kiosk/cart/cancel", h.CartCancel)
		e.Router.POST("/api/kiosk/cart/commit", h.CartCommit)
		e.Router.POST("/api/kiosk/cart/rfid-scan", h.RFIDScan)
		e.Router.POST("/api/kiosk/cart/read-trigger", h.ReadTrigger)
		e.Router.GET("/api/kiosk/integrity", h.Integrity)
		e.Router.POST("/api/kiosk/integrity/rebuild", h.RebuildOpenCheckouts)
		e.Router.GET("/api/kiosk/reconciliation", h.Reconciliation)
		e.Router.GET("/api/kiosk/metrics", h.Metrics)
		e.Router.GET("/api/kiosk/reports/open-checkouts", h.ReportOpenCheckouts)
		e.Router.GET("/api/kiosk/reports/open-checkouts.csv", h.ReportOpenCheckoutsCSV)
		e.Router.GET("/api/kiosk/reports/low-stock.csv", h.ReportLowStockCSV)
		e.Router.GET("/api/kiosk/reports/group-activity.csv", h.ReportGroupActivityCSV)
		e.Router.GET("/api/kiosk/reports/instance-lifecycle.csv", h.ReportLifecycleAuditCSV)
		e.Router.GET("/api/kiosk/reports/notifications.csv", h.ReportNotificationsCSV)
		e.Router.POST("/api/kiosk/ledger/republish", h.RepublishLedger)
		// Timeclock — every handler self-gates on timeclock.enabled (404
		// when off), so unconditional registration keeps this list flat.
		e.Router.GET("/api/kiosk/timeclock/status", h.TimeclockStatus)
		e.Router.POST("/api/kiosk/timeclock/punch", h.TimeclockPunch)
		e.Router.GET("/api/kiosk/timeclock/foreman/options", h.TimeclockForemanOptions)
		e.Router.POST("/api/kiosk/timeclock/admin-punch", h.TimeclockAdminPunch)
		e.Router.GET("/api/kiosk/timeclock/now", h.TimeclockNow)
		e.Router.GET("/api/kiosk/timeclock/history", h.TimeclockHistory)
		e.Router.POST("/api/kiosk/timeclock/republish", h.TimeclockRepublish)
		e.Router.GET("/api/kiosk/reports/timeclock.csv", h.ReportTimeclockCSV)
		e.Router.POST("/api/kiosk/items/import", h.CSVImport)
		e.Router.GET("/api/kiosk/items/import/template", h.CSVImportTemplate)
		e.Router.POST("/api/kiosk/users/import", h.UsersCSVImport)
		e.Router.GET("/api/kiosk/users/import/template", h.UsersCSVImportTemplate)
		e.Router.POST("/api/kiosk/groups/import", h.GroupsCSVImport)
		e.Router.GET("/api/kiosk/groups/import/template", h.GroupsCSVImportTemplate)
		e.Router.GET("/api/kiosk/items.csv", h.ItemsExportCSV)
		e.Router.POST("/api/kiosk/items/{id}/adjust", h.AdjustItemStock)
		e.Router.POST("/api/kiosk/checkouts/by-line/{transaction_line_id}/close", h.AdminCloseCheckout)
		e.Router.GET("/api/kiosk/transactions.csv", h.TransactionsExportCSV)
		e.Router.GET("/api/kiosk/notifications", h.ListNotificationTemplates)
		e.Router.PATCH("/api/kiosk/notifications/{event_type}", h.UpdateNotificationTemplate)
		e.Router.GET("/api/kiosk/notifications/{event_type}/defaults", h.GetNotificationTemplateDefaults)

		// Serve the embedded Vue SPA. indexFallback=true means unknown
		// paths return index.html so client-side routes (/admin/*) resolve.
		// PocketBase's own /api/* and /_/* routes win on specificity.
		e.Router.GET("/{path...}", apis.Static(ui.FS(), true))

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
