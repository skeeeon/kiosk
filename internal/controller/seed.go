package controller

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase"
	"github.com/pocketbase/pocketbase/core"
	"github.com/spf13/cobra"

	"github.com/skeeeon/kiosk/internal/config"
	"github.com/skeeeon/kiosk/internal/events"
)

// RegisterSeedCommand wires the `seed-catalog` subcommand onto the
// controller's root command. The subcommand brings up its own NATS
// connection and catalog publisher hooks before doing its writes, so each
// record saved fans out to the JetStream KV buckets as it goes.
//
// Usage:
//
//	./kiosk-controller seed-catalog --items=items.csv --users=users.csv
func RegisterSeedCommand(app *pocketbase.PocketBase, cfg *config.Config) {
	cmd := &cobra.Command{
		Use:   "seed-catalog",
		Short: "Bulk-import items and/or users from CSV into the controller catalog",
		Long: "Imports items and/or users from CSV files. Existing records are " +
			"matched by `code` and updated in place; new records are inserted. " +
			"The same CSV format used by /api/kiosk/csv/import on the kiosks works here.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			itemsPath, _ := cmd.Flags().GetString("items")
			usersPath, _ := cmd.Flags().GetString("users")
			noPublish, _ := cmd.Flags().GetBool("no-publish")
			if itemsPath == "" && usersPath == "" {
				return errors.New("at least one of --items or --users is required")
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
				if _, err := NewCatalogPublisher(context.Background(), app, js); err != nil {
					return fmt.Errorf("catalog publisher: %w", err)
				}
			}

			if itemsPath != "" {
				if err := seedItems(app, itemsPath); err != nil {
					return fmt.Errorf("seed items: %w", err)
				}
			}
			if usersPath != "" {
				if err := seedUsers(app, usersPath); err != nil {
					return fmt.Errorf("seed users: %w", err)
				}
			}
			return nil
		},
	}
	cmd.Flags().String("items", "", "path to items CSV (columns: code,name,type,tracking_mode,...)")
	cmd.Flags().String("users", "", "path to users CSV (columns: code,name,email,role,active)")
	cmd.Flags().Bool("no-publish", false, "import locally only; skip JetStream KV fan-out")
	app.RootCmd.AddCommand(cmd)
}

func seedItems(app core.App, path string) error {
	rows, err := readCSV(path)
	if err != nil {
		return err
	}
	if len(rows) < 2 {
		return fmt.Errorf("%s: needs a header row and at least one data row", path)
	}
	headers := normalizeHeaders(rows[0])
	if _, ok := headers["code"]; !ok {
		return fmt.Errorf("%s: missing 'code' column", path)
	}

	col, err := app.FindCollectionByNameOrId("items")
	if err != nil {
		return fmt.Errorf("find items collection: %w", err)
	}

	var inserted, updated, skipped int
	for i, row := range rows[1:] {
		code := strings.TrimSpace(csvCol(headers, row, "code"))
		name := strings.TrimSpace(csvCol(headers, row, "name"))
		typ := strings.TrimSpace(csvCol(headers, row, "type"))
		tracking := strings.TrimSpace(csvCol(headers, row, "tracking_mode"))
		if tracking == "" {
			tracking = "quantity"
		}
		if code == "" || name == "" || (typ != "tool" && typ != "consumable") {
			log.Printf("seed-catalog items row %d skipped: invalid required fields", i+2)
			skipped++
			continue
		}

		existing, err := app.FindFirstRecordByFilter("items",
			"code = {:c}", dbx.Params{"c": code})
		if err != nil && !isNotFound(err) {
			return fmt.Errorf("lookup item %s: %w", code, err)
		}
		var rec *core.Record
		if existing != nil {
			rec = existing
		} else {
			rec = core.NewRecord(col)
		}
		rec.Set("code", code)
		rec.Set("name", name)
		rec.Set("type", typ)
		rec.Set("tracking_mode", tracking)
		rec.Set("unit", csvCol(headers, row, "unit"))
		rec.Set("serial", csvCol(headers, row, "serial"))
		rec.Set("category", csvCol(headers, row, "category"))
		rec.Set("rfid_epc", csvCol(headers, row, "rfid_epc"))
		rec.Set("notes", csvCol(headers, row, "notes"))
		rec.Set("active", parseCSVBool(csvCol(headers, row, "active"), true))

		if err := app.Save(rec); err != nil {
			return fmt.Errorf("save item %s: %w", code, err)
		}
		if existing != nil {
			updated++
		} else {
			inserted++
		}
	}
	log.Printf("seed-catalog items: %d inserted, %d updated, %d skipped", inserted, updated, skipped)
	return nil
}

func seedUsers(app core.App, path string) error {
	rows, err := readCSV(path)
	if err != nil {
		return err
	}
	if len(rows) < 2 {
		return fmt.Errorf("%s: needs a header row and at least one data row", path)
	}
	headers := normalizeHeaders(rows[0])
	if _, ok := headers["code"]; !ok {
		return fmt.Errorf("%s: missing 'code' column", path)
	}

	col, err := app.FindCollectionByNameOrId("users")
	if err != nil {
		return fmt.Errorf("find users collection: %w", err)
	}

	var inserted, updated, skipped int
	for i, row := range rows[1:] {
		code := strings.TrimSpace(csvCol(headers, row, "code"))
		name := strings.TrimSpace(csvCol(headers, row, "name"))
		role := strings.TrimSpace(csvCol(headers, row, "role"))
		if role == "" {
			role = "worker"
		}
		if code == "" || name == "" {
			log.Printf("seed-catalog users row %d skipped: missing code or name", i+2)
			skipped++
			continue
		}
		if role != "worker" && role != "foreman" {
			log.Printf("seed-catalog users row %d skipped: invalid role %q", i+2, role)
			skipped++
			continue
		}

		existing, err := app.FindFirstRecordByFilter("users",
			"code = {:c}", dbx.Params{"c": code})
		if err != nil && !isNotFound(err) {
			return fmt.Errorf("lookup user %s: %w", code, err)
		}
		var rec *core.Record
		if existing != nil {
			rec = existing
		} else {
			rec = core.NewRecord(col)
			// PB auth collections require a password; workers don't actually
			// log in so a strong random one is fine. The catalog projection
			// to kiosks doesn't carry passwords.
			pw, err := randomPassword(16)
			if err != nil {
				return fmt.Errorf("generate password for %s: %w", code, err)
			}
			rec.SetPassword(pw)
		}
		rec.Set("code", code)
		rec.Set("name", name)
		rec.Set("email", csvCol(headers, row, "email"))
		rec.Set("role", role)
		rec.Set("active", parseCSVBool(csvCol(headers, row, "active"), true))

		if err := app.Save(rec); err != nil {
			return fmt.Errorf("save user %s: %w", code, err)
		}
		if existing != nil {
			updated++
		} else {
			inserted++
		}
	}
	log.Printf("seed-catalog users: %d inserted, %d updated, %d skipped", inserted, updated, skipped)
	return nil
}

func readCSV(path string) ([][]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return csv.NewReader(f).ReadAll()
}

func normalizeHeaders(headers []string) map[string]int {
	out := make(map[string]int, len(headers))
	for i, h := range headers {
		out[strings.ToLower(strings.TrimSpace(h))] = i
	}
	return out
}

func csvCol(headers map[string]int, row []string, name string) string {
	i, ok := headers[name]
	if !ok || i >= len(row) {
		return ""
	}
	return strings.TrimSpace(row[i])
}

func parseCSVBool(s string, defaultVal bool) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "":
		return defaultVal
	case "true", "1", "yes", "y":
		return true
	default:
		return false
	}
}
