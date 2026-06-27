// Command kiosk-controller is the fleet's central aggregator. It embeds
// PocketBase (admin UI at /_/, REST API at /api/collections/*) and runs a
// JetStream durable consumer that projects per-kiosk transaction events
// into its own ledger. v1's scope ends there — phase 2 adds catalog push,
// drift detection, and command channels.
//
// The binary uses the same Config struct and migration registry as
// cmd/kiosk. The distinction is enforced by the KIOSK_ROLE=controller env
// var, set in main() before any of the role-guarded migrations register
// themselves and before config validation looks for kiosk.code.
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
	"github.com/skeeeon/kiosk/internal/config"
	"github.com/skeeeon/kiosk/internal/controller"
	"github.com/skeeeon/kiosk/internal/events"
	"github.com/skeeeon/kiosk/internal/notifications"
	"github.com/skeeeon/kiosk/internal/scheduler"
	"github.com/skeeeon/kiosk/internal/ui"

	// Kiosk migrations register via init(); we just need them in the
	// build graph. The controller-only migrations live in a sibling
	// package the kiosk binary doesn't import — that's the gating
	// mechanism that keeps them off plain kiosks.
	_ "github.com/skeeeon/kiosk/migrations"
	_ "github.com/skeeeon/kiosk/migrations/controller"
)

// sendLogRetentionDays bounds the notification_send_log + notification_dedupe
// tables on the controller. Mirrors the kiosk-side window in cmd/kiosk/main.go;
// configurable later if a fleet wants longer audit retention.
const sendLogRetentionDays = 90

func main() {
	// Signal to the config validator that we're the controller (relaxes
	// the kiosk.code requirement). Migrations themselves no longer key
	// off this env var — controller-only migrations live in
	// migrations/controller, which only this binary imports.
	if err := os.Setenv("KIOSK_ROLE", "controller"); err != nil {
		log.Fatalf("set KIOSK_ROLE: %v", err)
	}

	cfg, err := config.Load(configPath())
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	// Install the configured NATS subject prefix before stream/consumer
	// provisioning reads it via the builders in internal/events. Empty
	// falls back to events.DefaultSubjectPrefix; the kiosk side must agree.
	events.SetSubjectPrefix(cfg.NATS.SubjectPrefix)

	// Use a distinct data dir so a kiosk and controller can co-exist in the
	// same checkout during development without stomping each other's SQLite
	// files. Operators in production typically run them from different hosts
	// or working directories anyway.
	app := pocketbase.NewWithConfig(pocketbase.Config{
		DefaultDataDir: "pb_data_controller",
	})

	migratecmd.MustRegister(app, app.RootCmd, migratecmd.Config{
		Automigrate: true,
	})

	authfix.EnforceEmailVisibility(app)

	controller.RegisterSeedCommand(app, cfg)

	// Centralized notifier for managed kiosks. Reads templates from the
	// controller's notification_templates collection (seeded automatically
	// via the kiosk-side migrations the controller transitively imports)
	// and sends via the controller's configured SMTP. Kiosks publish
	// receipt.transaction and alert.lowstock events on the JetStream stream;
	// the aggregator dispatches them here.
	notifier := notifications.New(app)

	h := controller.New(app, cfg, notifier)

	// Scheduled reports own the fleet's digest cadence in managed mode. The
	// controller has the projected open_checkouts table + transaction ledger
	// and the central notification_templates config, so the scheduler runs
	// here exclusively — kiosks running in managed mode skip the scheduler
	// entirely (see cmd/kiosk/main.go). BindRecordHooks reacts to SPA edits;
	// RegisterEnabled reattaches enabled rows at boot inside OnServe.
	scheduler.BindRecordHooks(app, notifier.SendTo)

	// Drain in-flight notification goroutines on shutdown before PB tears the
	// DB down (see the Notifier docs). Bounded best-effort; registered
	// unconditionally so it also runs when NATS is disabled.
	app.OnTerminate().BindFunc(func(e *core.TerminateEvent) error {
		notifier.WaitInFlight(2 * time.Second)
		return e.Next()
	})

	// All NATS-dependent setup goes inside OnServe so non-serve subcommands
	// (--help, seed-catalog, migrate, etc.) don't attempt to connect to a
	// broker and fail when none is reachable. The seed subcommand brings up
	// its own NATS + publisher hooks before running.
	// Daily retention pass on the controller's notification_send_log and
	// notification_dedupe tables. Same 90-day window as the kiosk side; PB's
	// Cron is process-local, so if the controller is down at fire time the
	// next eligible tick handles the backlog.
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
		// Reattach enabled scheduled_reports rows to app.Cron(). The hooks
		// installed above keep the cron table in sync with mutations; this
		// covers the boot path.
		scheduler.RegisterEnabled(app, notifier.SendTo)

		// NATS + catalog publisher + aggregator are brought up first so the
		// catalog integrity/reconcile handlers below can close over the
		// publisher. The catch-all SPA route is registered last so that
		// specific /api/kiosk/* routes win matching.
		if !cfg.NATS.Enabled {
			return fmt.Errorf("nats.enabled must be true for the controller — set nats.url and enable")
		}
		pub, err := events.Connect(cfg.NATS, "kiosk-controller")
		if err != nil {
			return fmt.Errorf("nats connect: %w", err)
		}
		events.SetPublisher(pub)
		js, err := events.JetStream(pub)
		if err != nil {
			return fmt.Errorf("jetstream: %w", err)
		}

		aggCtx, aggCancel := context.WithCancel(context.Background())

		cp, err := controller.NewCatalogPublisher(aggCtx, app, js,
			cfg.Controller.CatalogItemsBucket,
			cfg.Controller.CatalogUsersBucket,
			cfg.Controller.CatalogGroupsBucket)
		if err != nil {
			aggCancel()
			return fmt.Errorf("catalog publisher: %w", err)
		}

		agg := controller.NewAggregator(app, js, cfg.NATS.StreamName)
		agg.SetNotifier(notifier)
		if err := agg.Start(aggCtx); err != nil {
			aggCancel()
			return fmt.Errorf("start aggregator: %w", err)
		}

		// Heartbeat registry: in-memory only (no JetStream). Constructed
		// after the aggregator so its first-beat auto-register callback can
		// point at agg.TouchKiosk — this is how kiosks that have never
		// completed a transaction still show up in the kiosks collection.
		nc, err := events.Conn(pub)
		if err != nil {
			aggCancel()
			return fmt.Errorf("nats conn for heartbeats: %w", err)
		}
		hbRegistry := controller.NewHeartbeatRegistry(agg.TouchKiosk)
		hbSub, err := hbRegistry.Subscribe(nc)
		if err != nil {
			aggCancel()
			return fmt.Errorf("subscribe heartbeats: %w", err)
		}

		// Sighting ingest: plain core-NATS fleet-wide subscriber for the lossy
		// `sighting` family (location/sightings L3). Provisions last_observed_state,
		// resolves via instance_epc_index, upserts instance_location, mirrors down.
		sightingIngest := controller.NewSightingIngest(aggCtx, app, js)
		sightingSub, err := sightingIngest.Subscribe(nc)
		if err != nil {
			aggCancel()
			return fmt.Errorf("subscribe sightings: %w", err)
		}

		// Override the standalone maintenance digest with the controller's
		// fleet-wide snapshot fan-out. The standalone runner reads the local
		// item_instances table, which is empty on the controller (instances
		// live on the kiosks); this closure captures the NATS conn + heartbeat
		// registry the scheduler's runner signature can't carry. Safe to do
		// after RegisterEnabled — runOnce resolves the runner at fire time.
		scheduler.RegisterRunner("maintenance", h.MaintenanceDigestRunner(nc, hbRegistry))

		// Override the standalone open-checkouts digest with the controller's
		// NATS-first gather (online kiosks answer live; offline kiosks fall back
		// to the projected ledger) and stamp provenance so an offline kiosk is
		// flagged, never silently dropped. The closure captures nc + the
		// heartbeat registry, same as the maintenance override.
		scheduler.RegisterRunner("open_checkouts", h.OpenCheckoutsDigestRunner(nc, hbRegistry))

		app.OnTerminate().BindFunc(func(te *core.TerminateEvent) error {
			agg.Stop()
			if hbSub != nil {
				_ = hbSub.Unsubscribe()
			}
			if sightingSub != nil {
				_ = sightingSub.Unsubscribe()
			}
			aggCancel()
			if p := events.CurrentPublisher(); p != nil {
				p.Close()
			}
			return te.Next()
		})

		// Custom HTTP routes for the SPA's identity, branding, exports, and
		// catalog reconciliation. PB's REST API at /api/collections/* still
		// handles CRUD.
		e.Router.GET("/health", func(re *core.RequestEvent) error {
			return re.JSON(200, map[string]string{"status": "ok"})
		})
		e.Router.GET("/api/kiosk/identity", h.Identity)
		e.Router.GET("/branding/logo", h.Logo)
		e.Router.GET("/branding/custom.css", h.CustomCSS)
		e.Router.GET("/api/kiosk/items.csv", h.ItemsExportCSV)
		e.Router.GET("/api/kiosk/transactions.csv", h.TransactionsExportCSV)
		e.Router.GET("/api/kiosk/catalog/integrity", h.CatalogIntegrity(cp))
		e.Router.POST("/api/kiosk/catalog/reconcile", h.CatalogReconcile(cp))
		e.Router.GET("/api/kiosk/reports/open-checkouts", h.ReportOpenCheckouts(nc, hbRegistry))
		e.Router.GET("/api/kiosk/reports/open-checkouts.csv", h.ReportOpenCheckoutsCSV(nc, hbRegistry))
		e.Router.GET("/api/kiosk/reports/group-activity.csv", h.ReportGroupActivityCSV)
		e.Router.GET("/api/kiosk/reports/instance-lifecycle.csv", h.ReportLifecycleAuditCSV)
		e.Router.GET("/api/kiosk/reports/notifications.csv", h.ReportNotificationsCSV)

		// CSV import + downloadable templates. Items lands at the kiosk-side
		// URL the SPA already uses so the same form works on either binary;
		// users and groups are controller-only since standalone kiosks have
		// admin CRUD UI for the smaller scale they operate at.
		e.Router.POST("/api/kiosk/items/import", h.CSVImportItems)
		e.Router.GET("/api/kiosk/items/import/template", h.CSVImportTemplateItems)
		e.Router.POST("/api/kiosk/users/import", h.CSVImportUsers)
		e.Router.GET("/api/kiosk/users/import/template", h.CSVImportTemplateUsers)
		e.Router.POST("/api/kiosk/groups/import", h.CSVImportGroups)
		e.Router.GET("/api/kiosk/groups/import/template", h.CSVImportTemplateGroups)

		// Centralized notifications CRUD. Managed kiosks' admin SPA hits
		// these via /api/controller/notifications/* instead of the kiosk's
		// local /api/kiosk/notifications/* — the read-only banner on the
		// kiosk side now reflects reality.
		e.Router.GET("/api/controller/notifications", h.ListNotificationTemplates)
		e.Router.PATCH("/api/controller/notifications/{event_type}", h.UpdateNotificationTemplate)
		e.Router.GET("/api/controller/notifications/{event_type}/defaults", h.GetNotificationTemplateDefaults)

		// Fleet liveness + remote admin endpoints. The heartbeats endpoint is
		// the SPA's source of truth for the online/stale/offline badge; the
		// inventory endpoints proxy controller→kiosk commands over NATS.
		e.Router.GET("/api/controller/kiosks/heartbeats", h.HeartbeatsEndpoint(hbRegistry))
		e.Router.GET("/api/controller/kiosks/{code}/inventory", h.InventorySnapshot(nc, hbRegistry))
		e.Router.GET("/api/controller/kiosks/{code}/metrics", h.Metrics(nc, hbRegistry))
		e.Router.GET("/api/controller/kiosks/{code}/config", h.Config(nc, hbRegistry))
		e.Router.POST("/api/controller/kiosks/{code}/inventory/adjust", h.InventoryAdjust(nc, hbRegistry))
		e.Router.POST("/api/controller/kiosks/{code}/checkouts/{source_line_id}/close", h.CheckoutClose(nc, hbRegistry))

		// Remote item-instance management. Mirrors the inventory family of
		// commands; idempotency is on the kiosk side via instance_audit's
		// unique-when-non-empty command_id index.
		e.Router.GET("/api/controller/kiosks/{code}/instances", h.InstanceSnapshot(nc, hbRegistry))
		e.Router.POST("/api/controller/kiosks/{code}/instances", h.InstanceCreate(nc, hbRegistry))
		e.Router.PATCH("/api/controller/kiosks/{code}/instances/{instance_code}", h.InstanceEdit(nc, hbRegistry))
		e.Router.POST("/api/controller/kiosks/{code}/instances/{instance_code}/status", h.InstanceSetStatus(nc, hbRegistry))

		// Maintenance commands. Both run on the target kiosk against its
		// own ledger; the controller never reaches into the kiosk's DB.
		e.Router.POST("/api/controller/kiosks/{code}/integrity/rebuild", h.IntegrityRebuild(nc, hbRegistry))
		e.Router.POST("/api/controller/kiosks/{code}/ledger/republish", h.LedgerRepublish(nc, hbRegistry))

		// Timeclock. Remote punches run at the target kiosk (kiosks are the
		// only punch writers); the fleet reports read the controller's own
		// projected time_punches.
		e.Router.POST("/api/controller/kiosks/{code}/timeclock/punch", h.TimeclockPunch(nc, hbRegistry))
		e.Router.POST("/api/controller/kiosks/{code}/timeclock/republish", h.TimeclockRepublish(nc, hbRegistry))
		e.Router.GET("/api/controller/timeclock/now", h.TimeclockNow)
		e.Router.GET("/api/controller/timeclock/history", h.TimeclockHistory)
		e.Router.GET("/api/controller/reports/timeclock.csv", h.ReportTimeclockCSV)

		// Fleet-wide reports. Low-stock fans out inventory.snapshot to
		// every online kiosk in parallel and joins with the controller's
		// projected ledger to compute available quantities.
		e.Router.GET("/api/controller/reports/low-stock", h.ReportLowStock(nc, hbRegistry))
		e.Router.GET("/api/controller/reports/low-stock.csv", h.ReportLowStockCSV(nc, hbRegistry))
		e.Router.GET("/api/controller/reports/adjustment-audit.csv", h.ReportAdjustmentAuditCSV)

		// Serve the same embedded Vue SPA the kiosk uses. The SPA detects
		// role at boot via /api/kiosk/identity and gates its UI accordingly.
		e.Router.GET("/{path...}", apis.Static(ui.FS(), true))

		return e.Next()
	})

	log.Printf("starting kiosk-controller on %s:%d (nats=%s)",
		cfg.Server.Bind, cfg.Server.Port, cfg.NATS.URL)

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
	return "controller.yaml"
}
