package demoseed

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"

	"github.com/skeeeon/kiosk/internal/dberr"
)

// Logf is the seeder's progress sink. Both subcommands pass log.Printf;
// tests pass a no-op. Taking it as an argument rather than reaching for the
// global logger keeps the package testable without capturing stderr.
type Logf func(format string, args ...any)

// discard is the zero value for a nil Logf.
func discard(string, ...any) {}

// Result counts what one seeder call did.
//
// Created counts rows inserted. Existing counts rows found by natural key
// and left alone — field fills run only on create, so a demo you have
// hand-edited survives a re-seed. Applied counts mutations performed
// (stock adjustments, unit creates) as opposed to plain row writes.
//
// A second full run must report Created == 0 and Applied == 0. That is the
// idempotency contract, and seed_test.go asserts it.
type Result struct {
	Created  int
	Existing int
	Applied  int
}

func (r *Result) add(other Result) {
	r.Created += other.Created
	r.Existing += other.Existing
	r.Applied += other.Applied
}

// SeedCatalog writes the shared vocabulary: the demo admin, the crews, the
// workers and the SKUs. On the controller this runs with itemCodes == nil
// (the whole catalogue) and every save fans out to the JetStream KV
// buckets through the publisher hooks the caller bound first. On a
// standalone kiosk it runs scoped to that node's own slice, because a
// standalone kiosk has no controller to receive membership from and
// carrying SKUs it doesn't stock would misrepresent the product.
//
// Order matters: groups before users (so a user's group resolves to the
// seeded row rather than auto-creating a bare one), and items before the
// membership rows SeedFleet writes.
func SeedCatalog(app core.App, itemCodes []string, log Logf) (*Result, error) {
	if log == nil {
		log = discard
	}
	out := &Result{}

	if err := seedDemoAdmin(app, out, log); err != nil {
		return nil, err
	}

	groupIDs := map[string]string{}
	for _, g := range Groups {
		rec, created, err := findOrCreate(app, "groups", "code", g.Code, func(r *core.Record) error {
			r.Set("code", g.Code)
			r.Set("name", g.Name)
			r.Set("contact_email", g.ContactEmail)
			r.Set("active", true)
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("seed group %s: %w", g.Code, err)
		}
		groupIDs[g.Code] = rec.Id
		count(out, created)
		if created {
			log("demo-seed: group %s", g.Code)
		}
	}

	for _, u := range Users {
		gid, ok := groupIDs[u.Group]
		if !ok {
			return nil, fmt.Errorf("seed user %s: unknown group %q", u.Code, u.Group)
		}
		_, created, err := findOrCreate(app, "users", "code", u.Code, func(r *core.Record) error {
			// Workers don't log in at a kiosk — identity arrives by badge
			// scan. A random password satisfies PB's auth collection; the
			// virtual timeclock's workers reach their account through the
			// reset-by-email flow, same as a real install.
			pw, err := randomPassword()
			if err != nil {
				return err
			}
			r.SetPassword(pw)
			r.Set("code", u.Code)
			r.Set("name", u.Name)
			r.Set("email", u.Email)
			r.Set("role", u.Role)
			r.Set("group", gid)
			r.Set("active", true)
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("seed user %s: %w", u.Code, err)
		}
		count(out, created)
		if created {
			log("demo-seed: user %s (%s)", u.Code, u.Name)
		}
	}

	wanted := map[string]bool{}
	for _, c := range itemCodes {
		wanted[c] = true
	}
	for _, it := range Items {
		if itemCodes != nil && !wanted[it.Code] {
			continue
		}
		item := it // capture for the closure
		_, created, err := findOrCreate(app, "items", "code", it.Code, func(r *core.Record) error {
			r.Set("code", item.Code)
			r.Set("name", item.Name)
			r.Set("type", item.Type)
			r.Set("tracking_mode", item.TrackingMode)
			r.Set("unit", item.Unit)
			r.Set("category", item.Category)
			r.Set("notes", item.Notes)
			r.Set("active", true)
			r.Set("requires_maintenance_on_return", item.RequiresMaintenance)
			// quantity_on_hand and reorder_threshold are deliberately not
			// set here. They are per-node physical state; the appliers own
			// them. A controller-side value would be meaningless (which
			// site's shelf?) and, for a serialized SKU, would be
			// overwritten by the instance-count recompute anyway.
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("seed item %s: %w", it.Code, err)
		}
		count(out, created)
		if created {
			log("demo-seed: item %s (%s)", it.Code, it.Name)
		}
	}

	return out, nil
}

// SeedFleet registers the nodes and their catalogue membership. Controller
// only — `kiosks` and `kiosk_items` live in migrations/controller and the
// kiosk binary's database has never heard of them.
//
// Creating the kiosk_items rows is the load-bearing half. Creating an item
// publishes nothing: publishItemToMembers loops over KiosksForItem, and a
// freshly-created item has no members, so the loop body never executes.
// The membership row is what puts a SKU in a kiosk's KV slice.
func SeedFleet(app core.App, log Logf) (*Result, error) {
	if log == nil {
		log = discard
	}
	out := &Result{}

	for _, n := range Nodes {
		node := n
		kiosk, created, err := findOrCreate(app, "kiosks", "kiosk_code", n.Code, func(r *core.Record) error {
			r.Set("kiosk_code", node.Code)
			r.Set("location_code", node.LocationCode)
			r.Set("notes", node.Notes)
			// `status` is Required and has no schema default — set it
			// explicitly, the same way touchKiosk does on auto-register.
			// Pre-registering here is the cleanest of the three paths into
			// this table; the heartbeat and first-transaction paths then
			// converge on this row and do nothing.
			r.Set("status", "unknown")
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("seed kiosk %s: %w", n.Code, err)
		}
		count(out, created)
		if created {
			log("demo-seed: kiosk %s at %s", n.Code, n.LocationCode)
		}

		for _, code := range node.ItemCodes() {
			item, err := app.FindFirstRecordByFilter("items", "code = {:c}", dbx.Params{"c": code})
			if err != nil {
				return nil, fmt.Errorf("membership %s/%s: find item: %w", node.Code, code, err)
			}
			created, err := ensureMembership(app, kiosk.Id, item.Id)
			if err != nil {
				return nil, fmt.Errorf("membership %s/%s: %w", node.Code, code, err)
			}
			count(out, created)
			if created {
				log("demo-seed: %s stocks %s", node.Code, code)
			}
		}
	}

	return out, nil
}

// ensureMembership is the (kiosk, item) pair upsert. The pair is
// unique-indexed, so the find-first-then-create race resolves into a
// unique violation rather than a duplicate — and a seeder is single-writer
// anyway.
func ensureMembership(app core.App, kioskID, itemID string) (bool, error) {
	_, err := app.FindFirstRecordByFilter("kiosk_items",
		"kiosk = {:k} && item = {:i}", dbx.Params{"k": kioskID, "i": itemID})
	if err == nil {
		return false, nil
	}
	if !dberr.IsNotFound(err) {
		return false, err
	}
	col, err := app.FindCollectionByNameOrId("kiosk_items")
	if err != nil {
		return false, err
	}
	rec := core.NewRecord(col)
	rec.Set("kiosk", kioskID)
	rec.Set("item", itemID)
	if err := app.Save(rec); err != nil {
		return false, err
	}
	return true, nil
}

// seedDemoAdmin creates the well-known admin. The bootstrap admin's
// password is printed once and then gone, which is the wrong property for
// a demo that gets rebuilt every few weeks.
func seedDemoAdmin(app core.App, out *Result, log Logf) error {
	_, created, err := findOrCreate(app, "admins", "email", DemoAdminEmail, func(r *core.Record) error {
		r.Set("email", DemoAdminEmail)
		r.Set("name", DemoAdminName)
		r.Set("active", true)
		r.Set("verified", true)
		r.SetPassword(DemoAdminPassword)
		return nil
	})
	if err != nil {
		return fmt.Errorf("seed demo admin: %w", err)
	}
	count(out, created)
	if created {
		log("demo-seed: admin %s / %s", DemoAdminEmail, DemoAdminPassword)
	}
	return nil
}

// DemoAdminID returns the id of the seeded demo admin, falling back to any
// admins row. Both appliers need an actor id: the local one writes it to
// stock_adjustments.admin (a real FK), the remote one sends it as
// controller_admin_id so the kiosk's audit UI can render a name.
func DemoAdminID(app core.App) (string, error) {
	rec, err := app.FindFirstRecordByFilter("admins", "email = {:e}",
		dbx.Params{"e": DemoAdminEmail})
	if err == nil {
		return rec.Id, nil
	}
	if !dberr.IsNotFound(err) {
		return "", err
	}
	rows, err := app.FindRecordsByFilter("admins", "id != ''", "created", 1, 0)
	if err != nil {
		return "", err
	}
	if len(rows) == 0 {
		return "", fmt.Errorf("no admins record to attribute the seed to; run demo-seed against a migrated database")
	}
	return rows[0].Id, nil
}

// findOrCreate is the idempotency primitive: look a row up by its natural
// key, and only if it is missing build it with fill. Returns whether it
// created one.
func findOrCreate(app core.App, collection, field, value string,
	fill func(*core.Record) error) (*core.Record, bool, error) {

	existing, err := app.FindFirstRecordByFilter(collection,
		field+" = {:v}", dbx.Params{"v": value})
	if err == nil {
		return existing, false, nil
	}
	if !dberr.IsNotFound(err) {
		return nil, false, err
	}

	col, err := app.FindCollectionByNameOrId(collection)
	if err != nil {
		return nil, false, fmt.Errorf("find collection %s: %w", collection, err)
	}
	rec := core.NewRecord(col)
	if err := fill(rec); err != nil {
		return nil, false, err
	}
	if err := app.Save(rec); err != nil {
		return nil, false, err
	}
	return rec, true, nil
}

func count(out *Result, created bool) {
	if created {
		out.Created++
		return
	}
	out.Existing++
}

// randomPassword mirrors csvimport's: PB's auth collections require a
// non-empty password on create and kiosk workers never use one.
func randomPassword() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
