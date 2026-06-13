// Command kiosk-timeclock is the per-user-authenticated self-service time
// clock terminal — workers clock in/out from their phones, no badge scanner
// or dedicated hardware. It is a kiosk in every architectural sense (it has a
// kiosk_code, writes the time_punches ledger via the same funnel, and — in
// managed mode — rides punch_state KV to the fleet), but its trust boundary is
// the authenticated `users` session rather than the box on a trusted LAN.
//
// That inversion is enforced BY CONSTRUCTION: this binary registers ONLY the
// authed /api/self/timeclock/* surface (plus identity/branding and the admin
// users-import used for standalone provisioning). None of the anonymous
// /api/kiosk/* checkout/cart/inventory routes are wired here, so a
// misconfiguration can't expose them — they don't exist in this process.
//
// It supports the SAME three operating modes as cmd/kiosk, degrading exactly
// the same way:
//   - standalone:        local punch ledger only; workers provisioned locally
//                        (admin SPA / superuser / CSV); clocked-in state local.
//   - standalone + NATS: also publishes timeclock.punch events for an external
//                        consumer; still local workers + local state.
//   - controller-managed: workers (with email, for auth) sync from the
//                        catalog_users watcher and clocked-in state merges
//                        fleet-wide via the punch_state replica.
// Only timeclock.virtual=true (which implies timeclock.enabled) is required.
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
	"github.com/skeeeon/kiosk/internal/cart"
	"github.com/skeeeon/kiosk/internal/catalog"
	"github.com/skeeeon/kiosk/internal/config"
	"github.com/skeeeon/kiosk/internal/events"
	"github.com/skeeeon/kiosk/internal/handlers"
	"github.com/skeeeon/kiosk/internal/heartbeat"
	"github.com/skeeeon/kiosk/internal/kioskctx"
	"github.com/skeeeon/kiosk/internal/notifications"
	"github.com/skeeeon/kiosk/internal/timeclock"
	"github.com/skeeeon/kiosk/internal/ui"

	// Kiosk-side schema (incl. the time_punches ledger) registers via init().
	// The timeclock-only migration package — imported by this binary alone —
	// turns the users collection into a real worker-auth surface; regular
	// kiosks and the controller never import it, so they never enable login.
	_ "github.com/skeeeon/kiosk/migrations"
	_ "github.com/skeeeon/kiosk/migrations/timeclock"
)

func main() {
	cfg, err := config.Load(configPath())
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	// This binary is meaningless without the flag (it gates the SPA into
	// worker-login mode and is what registers the self-service surface), so
	// refuse to run mislabeled — a virtual=false config belongs on cmd/kiosk.
	if !cfg.Timeclock.Virtual {
		log.Fatal("cmd/timeclock requires timeclock.virtual=true (use cmd/kiosk for a standard or physical timeclock-only kiosk)")
	}

	kioskctx.Set(kioskctx.Identity{
		KioskCode:    cfg.Kiosk.Code,
		LocationCode: cfg.Kiosk.LocationCode,
	})

	events.SetSubjectPrefix(cfg.NATS.SubjectPrefix)

	// Distinct data dir so a kiosk / controller / timeclock terminal can
	// co-exist in one checkout during development without stomping SQLite.
	app := pocketbase.NewWithConfig(pocketbase.Config{
		DefaultDataDir: "pb_data_timeclock",
	})

	migratecmd.MustRegister(app, app.RootCmd, migratecmd.Config{
		Automigrate: true,
	})

	authfix.EnforceEmailVisibility(app)

	// Match-only OAuth2 guard: a worker must already exist (provisioned by the
	// controller in managed mode, or locally in standalone) with a matching
	// email before SSO will mint a session. Without this, PocketBase would
	// happily CREATE a users record for any IdP account that completes the
	// OAuth2 dance — i.e. anyone with a Google/Microsoft login could punch a
	// clock. IsNewRecord is true exactly when no existing record matched, so
	// rejecting it closes the self-provisioning hole. Harmless when OAuth2
	// isn't configured (the hook only fires on OAuth2 auth requests).
	app.OnRecordAuthWithOAuth2Request("users").BindFunc(func(e *core.RecordAuthWithOAuth2RequestEvent) error {
		if e.IsNewRecord {
			return e.ForbiddenError("this account is not provisioned for time clock access", nil)
		}
		return e.Next()
	})

	// NATS is best-effort, exactly like cmd/kiosk: a standalone terminal runs
	// fine without it. Connect returns (nil, nil) when disabled and a buffering
	// connection on an unreachable broker; a non-nil error is structural.
	pub, err := events.Connect(cfg.NATS, "kiosk-timeclock-"+cfg.Kiosk.Code)
	if err != nil {
		log.Printf("nats: continuing without event publishing — %v", err)
	}
	events.SetPublisher(pub)

	var (
		catalogWatcher *catalog.Watcher
		punchWatcher   *timeclock.Watcher
		punchFleet     *timeclock.Fleet
	)
	watcherCtx, watcherCancel := context.WithCancel(context.Background())
	heartbeatCtx, heartbeatCancel := context.WithCancel(context.Background())

	// Managed mode: sync workers (with email, for auth) from the catalog and
	// hydrate the fleet-wide clocked-in replica. Best-effort — a failure here
	// degrades to local-only, same as cmd/kiosk.
	if cfg.Controller.Enabled {
		if pub == nil {
			log.Printf("controller.enabled=true but nats is not connected — workers will not sync and clocked-in state is local-only")
		} else if js, jerr := events.JetStream(pub); jerr != nil {
			log.Printf("controller.enabled=true but jetstream unavailable: %v", jerr)
		} else {
			catalogWatcher = catalog.NewWatcher(app, js,
				cfg.Controller.CatalogItemsBucket,
				cfg.Controller.CatalogUsersBucket,
				cfg.Controller.CatalogGroupsBucket,
				cfg.Kiosk.Code)
			punchFleet = timeclock.NewFleet()
			punchWatcher = timeclock.NewWatcher(js, punchFleet)
			app.OnServe().BindFunc(func(e *core.ServeEvent) error {
				if err := catalogWatcher.Start(watcherCtx); err != nil {
					log.Printf("catalog watcher: %v — workers may not sync until it recovers", err)
					catalogWatcher = nil
				}
				if err := punchWatcher.Start(watcherCtx); err != nil {
					log.Printf("punch watcher: %v — clocked-in state degrades to local-only", err)
					punchWatcher = nil
				}
				return e.Next()
			})
		}
	}

	// Heartbeat so the controller shows the terminal online. NATS-only.
	if pub != nil && cfg.NATS.Enabled {
		if nc, cerr := events.Conn(pub); cerr != nil {
			log.Printf("heartbeat: nats connection unavailable: %v", cerr)
		} else {
			app.OnServe().BindFunc(func(e *core.ServeEvent) error {
				heartbeat.Start(heartbeatCtx, nc, cfg.Kiosk.Code, "")
				return e.Next()
			})
		}
	}

	carts := cart.NewStore(cfg.Session.IdleTimeout.AsDuration())
	notifier := notifications.New(app)
	h := handlers.New(app, cfg, carts, notifier)
	// nil in the unmanaged modes — the timeclock merge rule is nil-safe and
	// degrades to local-only clocked-in state.
	h.PunchFleet = punchFleet

	app.OnTerminate().BindFunc(func(e *core.TerminateEvent) error {
		if catalogWatcher != nil {
			catalogWatcher.Stop()
		}
		if punchWatcher != nil {
			punchWatcher.Stop()
		}
		watcherCancel()
		heartbeatCancel()
		if p := events.CurrentPublisher(); p != nil {
			p.Close()
		}
		return e.Next()
	})

	app.OnServe().BindFunc(func(e *core.ServeEvent) error {
		// The entire public surface. PocketBase's own /api/collections/* (auth
		// endpoints) and /_/ (superuser) are still served by PB itself — gate
		// those at the reverse proxy / firewall as documented.
		e.Router.GET("/health", func(re *core.RequestEvent) error {
			return re.JSON(200, map[string]string{"status": "ok"})
		})
		e.Router.GET("/api/kiosk/identity", h.Identity)
		e.Router.GET("/branding/logo", h.Logo)
		e.Router.GET("/branding/custom.css", h.CustomCSS)
		e.Router.GET("/api/self/timeclock/status", h.SelfTimeclockStatus)
		e.Router.POST("/api/self/timeclock/punch", h.SelfTimeclockPunch)
		e.Router.GET("/api/self/timeclock/history", h.SelfTimeclockHistory)

		// Standalone provisioning aid: bulk-import workers from CSV. Admin-gated
		// (requireAdmin) like everywhere else; the admin SPA's user CRUD already
		// works against PB's built-in /api/collections/users. In managed mode
		// workers come from the catalog instead and the SPA hides mutation.
		e.Router.POST("/api/kiosk/users/import", h.UsersCSVImport)
		e.Router.GET("/api/kiosk/users/import/template", h.UsersCSVImportTemplate)

		// Embedded Vue SPA; it boots, reads /api/kiosk/identity, and switches
		// into the worker-login + self-punch experience on timeclock_virtual.
		e.Router.GET("/{path...}", apis.Static(ui.FS(), true))
		return e.Next()
	})

	log.Printf("starting kiosk-timeclock %s at %s on %s:%d (nats=%v, managed=%v)",
		cfg.Kiosk.Code, cfg.Kiosk.LocationCode, cfg.Server.Bind, cfg.Server.Port,
		cfg.NATS.Enabled, cfg.Controller.Enabled)

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
	return "timeclock.yaml"
}
