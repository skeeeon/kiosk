package controller

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"

	"github.com/pocketbase/pocketbase"
	"github.com/pocketbase/pocketbase/core"
	"github.com/spf13/cobra"

	"github.com/skeeeon/kiosk/internal/config"
	"github.com/skeeeon/kiosk/internal/csvimport"
	"github.com/skeeeon/kiosk/internal/events"
)

// RegisterSeedCommand wires the `seed-catalog` subcommand onto the
// controller's root command. The subcommand brings up its own NATS
// connection and catalog publisher hooks before doing its writes, so each
// record saved fans out to the JetStream KV buckets as it goes.
//
// All three CSV kinds the HTTP importer supports are also available here.
// The row-level work delegates to internal/csvimport so the CLI and HTTP
// paths can't drift in their validation/upsert behavior.
//
// Usage:
//
//	./kiosk-controller seed-catalog --items=items.csv --users=users.csv --groups=groups.csv
func RegisterSeedCommand(app *pocketbase.PocketBase, cfg *config.Config) {
	cmd := &cobra.Command{
		Use:   "seed-catalog",
		Short: "Bulk-import items, users, and/or groups from CSV into the controller catalog",
		Long: "Imports items, users, and/or groups from CSV files. Existing records " +
			"are matched by `code` and updated in place; new records are inserted. " +
			"Same row format as the HTTP importer (POST /api/kiosk/<kind>/import).",
		RunE: func(cmd *cobra.Command, _ []string) error {
			itemsPath, _ := cmd.Flags().GetString("items")
			usersPath, _ := cmd.Flags().GetString("users")
			groupsPath, _ := cmd.Flags().GetString("groups")
			noPublish, _ := cmd.Flags().GetBool("no-publish")
			if itemsPath == "" && usersPath == "" && groupsPath == "" {
				return errors.New("at least one of --items, --users, or --groups is required")
			}

			// Apply migrations explicitly — migratecmd's Automigrate only
			// hooks OnServe, and we're running a one-shot subcommand.
			if err := app.Bootstrap(); err != nil {
				return fmt.Errorf("bootstrap: %w", err)
			}
			runner := core.NewMigrationsRunner(app, core.AppMigrations)
			if _, err := runner.Up(); err != nil {
				return fmt.Errorf("apply migrations: %w", err)
			}

			// Wire the catalog publisher so the upserts below also flow to
			// JetStream KV. --no-publish skips this (useful for first-time
			// seeding when the broker isn't reachable yet; the publisher
			// hooks bound at the next normal startup will re-emit on edit).
			if !noPublish {
				pub, err := events.Connect(cfg.NATS)
				if err != nil {
					return fmt.Errorf("nats connect: %w", err)
				}
				defer pub.Close()
				js, err := events.JetStream(pub)
				if err != nil {
					return fmt.Errorf("jetstream: %w", err)
				}
				if _, err := NewCatalogPublisher(context.Background(), app, js,
					cfg.Controller.CatalogItemsBucket,
					cfg.Controller.CatalogUsersBucket,
					cfg.Controller.CatalogGroupsBucket); err != nil {
					return fmt.Errorf("catalog publisher: %w", err)
				}
			}

			// Groups go first so user rows that reference a group code
			// without a pre-existing row land on the just-imported metadata
			// rather than auto-creating a minimal row.
			if groupsPath != "" {
				if err := runSeedFile(app, groupsPath, csvimport.KindGroups); err != nil {
					return fmt.Errorf("seed groups: %w", err)
				}
			}
			if itemsPath != "" {
				if err := runSeedFile(app, itemsPath, csvimport.KindItems); err != nil {
					return fmt.Errorf("seed items: %w", err)
				}
			}
			if usersPath != "" {
				if err := runSeedFile(app, usersPath, csvimport.KindUsers); err != nil {
					return fmt.Errorf("seed users: %w", err)
				}
			}
			return nil
		},
	}
	cmd.Flags().String("items", "", "path to items CSV (columns: code,name,type,tracking_mode,unit,category,active,notes)")
	cmd.Flags().String("users", "", "path to users CSV (columns: code,name,email,role,group,active)")
	cmd.Flags().String("groups", "", "path to groups CSV (columns: code,name,contact_email,contact_phone,notes,active)")
	cmd.Flags().Bool("no-publish", false, "import locally only; skip JetStream KV fan-out")
	app.RootCmd.AddCommand(cmd)
}

// runSeedFile opens a CSV and pumps it through csvimport.Run, then logs a
// summary line. Per-row errors are surfaced as log entries — same outcome
// the HTTP importer reports in JSON, just emitted to the operator's
// terminal so they can scroll back through failures.
func runSeedFile(app core.App, path string, kind csvimport.Kind) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	result, err := csvimport.Run(app, kind, f, false)
	if err != nil {
		return err
	}
	for _, e := range result.Errors {
		log.Printf("seed-catalog %s row %d: %s (%s)", kind, e.Row, e.Message, e.Code)
	}
	log.Printf("seed-catalog %s: %d inserted, %d updated, %d errors",
		kind, result.RowsInserted, result.RowsUpdated, len(result.Errors))
	return nil
}
