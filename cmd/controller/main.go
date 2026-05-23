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

	"github.com/pocketbase/pocketbase"
	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/plugins/migratecmd"

	"github.com/skeeeon/kiosk/internal/authfix"
	"github.com/skeeeon/kiosk/internal/config"
	"github.com/skeeeon/kiosk/internal/controller"
	"github.com/skeeeon/kiosk/internal/events"

	"github.com/skeeeon/kiosk/migrations"
)

func main() {
	// Signal to the migration registry and config validator that we're the
	// controller. MUST happen before any import side-effects we care about
	// have a chance to read it.
	if err := os.Setenv("KIOSK_ROLE", "controller"); err != nil {
		log.Fatalf("set KIOSK_ROLE: %v", err)
	}

	// Register controller-only schema additions. Done explicitly (rather
	// than via init() side effects) so the kiosk binary, which imports the
	// same migrations package transitively, never registers them.
	migrations.RegisterControllerMigrations()

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

	h := controller.New(app, cfg)

	// All NATS-dependent setup goes inside OnServe so non-serve subcommands
	// (--help, seed-catalog, migrate, etc.) don't attempt to connect to a
	// broker and fail when none is reachable. The seed subcommand brings up
	// its own NATS + publisher hooks before running.
	app.OnServe().BindFunc(func(e *core.ServeEvent) error {
		// NATS + catalog publisher + aggregator are brought up first so the
		// catalog integrity/reconcile handlers below can close over the
		// publisher. The catch-all SPA route is registered last so that
		// specific /api/kiosk/* routes win matching.
		if !cfg.NATS.Enabled {
			return fmt.Errorf("nats.enabled must be true for the controller — set nats.url and enable")
		}
		pub, err := events.Connect(cfg.NATS)
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

		app.OnTerminate().BindFunc(func(te *core.TerminateEvent) error {
			agg.Stop()
			if hbSub != nil {
				_ = hbSub.Unsubscribe()
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
		e.Router.GET("/api/kiosk/items.csv", h.ItemsExportCSV)
		e.Router.GET("/api/kiosk/transactions.csv", h.TransactionsExportCSV)
		e.Router.GET("/api/kiosk/catalog/integrity", h.CatalogIntegrity(cp))
		e.Router.POST("/api/kiosk/catalog/reconcile", h.CatalogReconcile(cp))
		e.Router.GET("/api/kiosk/reports/open-checkouts", h.ReportOpenCheckouts)

		// Fleet liveness + remote admin endpoints. The heartbeats endpoint is
		// the SPA's source of truth for the online/stale/offline badge; the
		// inventory endpoints proxy controller→kiosk commands over NATS.
		e.Router.GET("/api/controller/kiosks/heartbeats", h.HeartbeatsEndpoint(hbRegistry))
		e.Router.GET("/api/controller/kiosks/{code}/inventory", h.InventorySnapshot(nc, hbRegistry))
		e.Router.POST("/api/controller/kiosks/{code}/inventory/adjust", h.InventoryAdjust(nc, hbRegistry))

		// Serve the same Vue SPA the kiosk uses. The SPA detects role at
		// boot via /api/kiosk/identity and gates its UI accordingly.
		e.Router.GET("/{path...}", apis.Static(os.DirFS("./pb_public"), true))

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
