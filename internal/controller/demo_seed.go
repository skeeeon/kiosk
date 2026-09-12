// The `demo-seed` subcommand: stand up the Northwind Traders demo estate
// from the controller. See docs/demo-plan.md for the whole story; this file
// is the controller half of it.
//
// Shape is copied from RegisterSeedCommand in seed.go — bootstrap,
// migrate, bring the catalog publisher up BEFORE any writes so each save
// fans out to KV as it goes. What it adds beyond seed-catalog is the four
// things a fleet stand-up needs that a CSV import doesn't: it ensures the
// JetStream stream exists, it gets a *nats.Conn for request/reply, it
// waits for the catalogue to actually land at each kiosk, and it drains
// before closing.
package controller

import (
	"context"
	"errors"
	"fmt"
	"log"

	"github.com/nats-io/nats.go"
	"github.com/pocketbase/pocketbase"
	"github.com/pocketbase/pocketbase/core"
	"github.com/spf13/cobra"

	"github.com/skeeeon/kiosk/internal/config"
	"github.com/skeeeon/kiosk/internal/demoseed"
	"github.com/skeeeon/kiosk/internal/events"
)

// RegisterDemoSeedCommand wires `demo-seed` onto the controller's root
// command.
//
// Usage:
//
//	./kiosk-controller demo-seed --confirm            # catalogue + fleet + KV fan-out
//	./kiosk-controller demo-seed --remote --confirm   # ...and kiosk-local state over the bus
//
// The two-step shape is the runbook's, and the reason is ordering: the
// second form needs the kiosks running, and the kiosks need the catalogue
// before they can resolve an item_code.
func RegisterDemoSeedCommand(app *pocketbase.PocketBase, cfg *config.Config) {
	cmd := &cobra.Command{
		Use:   "demo-seed",
		Short: "Seed the Northwind Traders demo estate (DEMO TOOLING — writes working credentials)",
		Long: "Writes the Northwind demo catalogue — crews, workers, SKUs, the three kiosks\n" +
			"and their per-kiosk membership — into this controller, fanning each save out to\n" +
			"the JetStream KV buckets as it goes. Idempotent: a second run creates nothing.\n\n" +
			"With --remote it then provisions each kiosk's own state (quantities and\n" +
			"serialized units) over the NATS command bus, which requires the kiosks to be\n" +
			"running and to have received the catalogue first.\n\n" +
			"This is demo tooling. It creates a working admin login with a well-known\n" +
			"password and must never be run against a customer installation.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			confirm, _ := cmd.Flags().GetBool("confirm")
			noPublish, _ := cmd.Flags().GetBool("no-publish")
			remote, _ := cmd.Flags().GetBool("remote")
			if !confirm {
				return errors.New("refusing to seed without --confirm: this writes a working admin login with a well-known password")
			}
			if remote && noPublish {
				return errors.New("--remote needs the broker; it cannot be combined with --no-publish")
			}
			return runDemoSeed(app, cfg, noPublish, remote)
		},
	}
	cmd.Flags().Bool("confirm", false, "required: acknowledge that this writes demo credentials")
	cmd.Flags().Bool("remote", false, "also provision each kiosk's local state over the NATS command bus (kiosks must be running)")
	cmd.Flags().Bool("no-publish", false, "seed locally only; skip the JetStream stream, KV buckets, and fan-out")
	app.RootCmd.AddCommand(cmd)
}

func runDemoSeed(app *pocketbase.PocketBase, cfg *config.Config, noPublish, remote bool) error {
	// Apply migrations explicitly — migratecmd's Automigrate only hooks
	// OnServe, and this is a one-shot subcommand.
	if err := app.Bootstrap(); err != nil {
		return fmt.Errorf("bootstrap: %w", err)
	}
	if _, err := core.NewMigrationsRunner(app, core.AppMigrations).Up(); err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}

	var nc *nats.Conn
	if !noPublish {
		conn, closeFn, err := openBroker(context.Background(), app, cfg)
		if err != nil {
			return err
		}
		defer closeFn()
		nc = conn
	}

	catalogue, err := demoseed.SeedCatalog(app, nil, log.Printf)
	if err != nil {
		return err
	}
	fleet, err := demoseed.SeedFleet(app, log.Printf)
	if err != nil {
		return err
	}
	log.Printf("demo-seed: catalogue — %d created, %d already present",
		catalogue.Created+fleet.Created, catalogue.Existing+fleet.Existing)

	if !remote {
		if noPublish {
			log.Printf("demo-seed: --no-publish — nothing reached KV; re-run without it to fan the catalogue out")
		}
		log.Printf("demo-seed: next, start the kiosks, then re-run with --remote to provision their local state")
		return nil
	}

	adminID, err := demoseed.DemoAdminID(app)
	if err != nil {
		return err
	}
	applied, err := demoseed.ApplyRemote(nc, adminID, log.Printf)
	if err != nil {
		return err
	}
	log.Printf("demo-seed: kiosk-local state — %d applied, %d already present",
		applied.Applied, applied.Existing)
	log.Printf("demo-seed: admin login %s / %s", demoseed.DemoAdminEmail, demoseed.DemoAdminPassword)
	return nil
}

// openBroker connects, provisions the stream and the catalog KV buckets,
// and binds the publisher's record hooks — all before the caller writes
// anything, because the hooks are what turn each app.Save into a KV
// fan-out.
//
// The EnsureStream call is the non-obvious one. The seeder cannot run
// while the controller serves (it writes the controller's own database),
// so under the runbook's ordering nothing has created the stream yet — and
// every instance.lifecycle / inventory.adjust event the seeded estate
// emits would be published into a void. Unlike the ledger, those audits
// have no republish command and cannot be backfilled afterwards.
func openBroker(ctx context.Context, app core.App, cfg *config.Config) (*nats.Conn, func(), error) {
	pub, err := events.Connect(cfg.NATS, "kiosk-controller-demo-seed")
	if err != nil {
		return nil, nil, fmt.Errorf("nats connect: %w", err)
	}
	if pub == nil {
		return nil, nil, errors.New("nats.enabled is false — demo-seed needs a broker; use --no-publish for a local-only catalogue")
	}
	closeFn := func() { pub.Close() }

	// events.Connect deliberately succeeds against an unreachable broker —
	// it returns a buffering connection and dials in the background,
	// because a kiosk's local ledger is authoritative and NATS must never
	// block it from starting. A one-shot seeder has the opposite posture:
	// it has nothing to buffer for, everything it does next needs a live
	// round trip, and without this check the first failure surfaces as
	// "ensure stream: nats: connection closed", which sends an operator
	// looking at JetStream instead of at the broker.
	conn, err := events.Conn(pub)
	if err != nil {
		closeFn()
		return nil, nil, fmt.Errorf("nats conn: %w", err)
	}
	if !conn.IsConnected() {
		closeFn()
		return nil, nil, fmt.Errorf("not connected to %s — start the broker (or use --no-publish for a local-only catalogue)", cfg.NATS.URL)
	}

	js, err := events.JetStream(pub)
	if err != nil {
		closeFn()
		return nil, nil, fmt.Errorf("jetstream: %w", err)
	}
	if _, err := EnsureStream(ctx, js, cfg.NATS.StreamName); err != nil {
		closeFn()
		return nil, nil, fmt.Errorf("ensure stream: %w", err)
	}
	if _, err := NewCatalogPublisher(ctx, app, js,
		cfg.Controller.CatalogItemsBucket,
		cfg.Controller.CatalogUsersBucket,
		cfg.Controller.CatalogGroupsBucket); err != nil {
		closeFn()
		return nil, nil, fmt.Errorf("catalog publisher: %w", err)
	}
	return conn, closeFn, nil
}
