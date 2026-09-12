package demoseed

import (
	"errors"
	"fmt"
	"log"

	"github.com/pocketbase/pocketbase"
	"github.com/pocketbase/pocketbase/core"
	"github.com/spf13/cobra"
)

// RegisterKioskCommand wires `demo-seed` onto the kiosk binary's root
// command. This is the standalone demo's whole setup: one binary, one
// config, no controller, no NATS — the cheapest thing to put in front of
// someone, and the first demo most people will ever see.
//
// It seeds only this node's slice of the catalogue, because a standalone
// kiosk has no controller to receive membership from and stocking SKUs it
// doesn't carry would misrepresent what the product does.
//
// Two things about the kiosk binary worth knowing, both consequences of
// main() doing its wiring before app.Start():
//
//   - The item_instances record hooks are registered by the time this
//     runs, which is WANTED. Every unit the applier creates writes its own
//     instance_audit row, publishes instance.lifecycle, and recomputes
//     items.quantity_on_hand. That is why ApplyLocal never hand-writes an
//     audit row. The OnServe registrations (watchers, RFID, command bus)
//     stay inert.
//   - kiosk.yaml must exist, because main() fatals on a config error
//     before cobra ever dispatches a subcommand.
func RegisterKioskCommand(app *pocketbase.PocketBase, kioskCode string) {
	cmd := &cobra.Command{
		Use:   "demo-seed",
		Short: "Seed this kiosk with its slice of the Northwind demo estate (DEMO TOOLING)",
		Long: "Writes the Northwind demo catalogue this kiosk stocks — crews, workers and\n" +
			"its own SKUs — then applies its local state: reorder thresholds, quantities,\n" +
			"and the serialized units in its cabinets. Idempotent: a second run creates\n" +
			"nothing.\n\n" +
			"For the standalone demo. On a controller-managed kiosk the catalogue arrives\n" +
			"over JetStream KV instead, and `kiosk-controller demo-seed --remote` provisions\n" +
			"the local state over the command bus.\n\n" +
			"This is demo tooling. It creates a working admin login with a well-known\n" +
			"password and must never be run against a customer installation.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			confirm, _ := cmd.Flags().GetBool("confirm")
			if !confirm {
				return errors.New("refusing to seed without --confirm: this writes a working admin login with a well-known password")
			}
			return RunKioskSeed(app, kioskCode, log.Printf)
		},
	}
	cmd.Flags().Bool("confirm", false, "required: acknowledge that this writes demo credentials")
	app.RootCmd.AddCommand(cmd)
}

// RunKioskSeed is the subcommand's body, exported so a test can drive it
// without cobra.
func RunKioskSeed(app *pocketbase.PocketBase, kioskCode string, logf Logf) error {
	if logf == nil {
		logf = discard
	}
	node, err := NodeByCode(kioskCode)
	if err != nil {
		return err
	}
	if node.Timeclock {
		return fmt.Errorf("%s is the virtual timeclock terminal: its workers arrive with the catalogue, and it stocks nothing to seed", kioskCode)
	}

	// Apply migrations explicitly — migratecmd's Automigrate only hooks
	// OnServe, and this is a one-shot subcommand.
	if err := app.Bootstrap(); err != nil {
		return fmt.Errorf("bootstrap: %w", err)
	}
	if _, err := core.NewMigrationsRunner(app, core.AppMigrations).Up(); err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}

	catalogue, err := SeedCatalog(app, node.ItemCodes(), logf)
	if err != nil {
		return err
	}
	logf("demo-seed: catalogue — %d created, %d already present", catalogue.Created, catalogue.Existing)

	local, err := ApplyLocal(app, kioskCode, logf)
	if err != nil {
		return err
	}
	logf("demo-seed: local state — %d applied, %d already present", local.Applied, local.Existing)
	logf("demo-seed: admin login %s / %s", DemoAdminEmail, DemoAdminPassword)
	return nil
}
