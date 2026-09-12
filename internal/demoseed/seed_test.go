package demoseed_test

import (
	"strings"
	"testing"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase"
	"github.com/pocketbase/pocketbase/core"

	"github.com/skeeeon/kiosk/internal/demoseed"
	"github.com/skeeeon/kiosk/internal/instances"
	"github.com/skeeeon/kiosk/internal/kioskctx"

	// Register kiosk migrations via init() so the runner can apply them.
	_ "github.com/skeeeon/kiosk/migrations"
)

// setupApp boots a fresh PB app in a temp dir with the kiosk migrations
// applied. Copied from internal/commit/commit_test.go: migratecmd's
// Automigrate hooks OnServe (not OnBootstrap), so a test that never starts
// a server has to run the migrations itself.
//
// The item_instances hooks are registered too, because the kiosk binary
// has them bound by the time `demo-seed` runs and the local applier
// depends on that: the hooks write the audit row and recompute
// items.quantity_on_hand, which is why ApplyLocal does neither.
func setupApp(t *testing.T) *pocketbase.PocketBase {
	t.Helper()
	t.Setenv("KIOSK_QUIET_BOOTSTRAP", "1")

	app := pocketbase.NewWithConfig(pocketbase.Config{
		DefaultDataDir:  t.TempDir(),
		HideStartBanner: true,
	})
	if err := app.Bootstrap(); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if _, err := core.NewMigrationsRunner(app, core.AppMigrations).Up(); err != nil {
		t.Fatalf("migrations up: %v", err)
	}
	instances.New().Register(app)
	t.Cleanup(func() { _ = app.ResetBootstrapState() })
	return app
}

// TestFixtureIsConsistent is the cheap guard that catches a hand-edited
// table before anyone spends twenty minutes standing an estate up. No DB.
func TestFixtureIsConsistent(t *testing.T) {
	groupCodes := map[string]bool{}
	for _, g := range demoseed.Groups {
		if groupCodes[g.Code] {
			t.Errorf("duplicate group code %q", g.Code)
		}
		groupCodes[g.Code] = true
	}

	userCodes := map[string]bool{}
	foremenByGroup := map[string]int{}
	for _, u := range demoseed.Users {
		if userCodes[u.Code] {
			t.Errorf("duplicate user code %q", u.Code)
		}
		userCodes[u.Code] = true
		if !groupCodes[u.Group] {
			t.Errorf("user %s is in unknown group %q", u.Code, u.Group)
		}
		if u.Role != "worker" && u.Role != "foreman" {
			t.Errorf("user %s has role %q, want worker or foreman", u.Code, u.Role)
		}
		if u.Role == "foreman" {
			foremenByGroup[u.Group]++
		}
	}
	// The on-behalf-of return dialog needs a foreman with crew to point at.
	if len(foremenByGroup) == 0 {
		t.Error("no foreman in the fixture: the foreman-return demo has nobody to drive it")
	}

	itemCodes := map[string]string{} // code -> tracking_mode
	for _, it := range demoseed.Items {
		if _, dup := itemCodes[it.Code]; dup {
			t.Errorf("duplicate item code %q", it.Code)
		}
		if it.Type != "tool" && it.Type != "consumable" {
			t.Errorf("item %s has type %q, want tool or consumable", it.Code, it.Type)
		}
		if it.TrackingMode != "quantity" && it.TrackingMode != "serialized" {
			t.Errorf("item %s has tracking_mode %q", it.Code, it.TrackingMode)
		}
		itemCodes[it.Code] = it.TrackingMode
	}

	nodeCodes := map[string]bool{}
	unitCodes := map[string]bool{}
	epcs := map[string]string{}
	for _, n := range demoseed.Nodes {
		if nodeCodes[n.Code] {
			t.Errorf("duplicate node code %q", n.Code)
		}
		nodeCodes[n.Code] = true
		if n.LocationCode == "" {
			t.Errorf("node %s has no location_code", n.Code)
		}

		seenStock := map[string]bool{}
		for _, s := range n.Stock {
			mode, ok := itemCodes[s.ItemCode]
			if !ok {
				t.Errorf("node %s stocks unknown item %q", n.Code, s.ItemCode)
				continue
			}
			// A Stock entry on a serialized SKU is the mistake worth
			// catching here: quantity_on_hand is derived from the instance
			// count and PerformStockAdjustment rejects the adjustment.
			if mode == "serialized" {
				t.Errorf("node %s has a Stock entry for serialized item %s", n.Code, s.ItemCode)
			}
			if seenStock[s.ItemCode] {
				t.Errorf("node %s lists item %s twice in Stock", n.Code, s.ItemCode)
			}
			seenStock[s.ItemCode] = true
			if s.Quantity < 0 || s.ReorderThreshold < 0 {
				t.Errorf("node %s item %s has a negative quantity or threshold", n.Code, s.ItemCode)
			}
		}

		for _, u := range n.Units {
			mode, ok := itemCodes[u.ItemCode]
			if !ok {
				t.Errorf("node %s has a unit of unknown item %q", n.Code, u.ItemCode)
				continue
			}
			if mode != "serialized" {
				t.Errorf("node %s has unit %s of quantity-tracked item %s", n.Code, u.Code, u.ItemCode)
			}
			if unitCodes[u.Code] {
				t.Errorf("duplicate unit code %q", u.Code)
			}
			unitCodes[u.Code] = true
			// rfid_epc is unique-when-non-empty across the whole fleet's
			// worth of fixtures, and the demo runs several kiosks against
			// one broker — a collision would only surface at seed time.
			if prev, dup := epcs[u.EPC()]; dup {
				t.Errorf("unit %s and %s derive the same EPC", prev, u.Code)
			}
			epcs[u.EPC()] = u.Code
			if len(u.EPC()) != 24 {
				t.Errorf("unit %s EPC %q is %d chars, want 24", u.Code, u.EPC(), len(u.EPC()))
			}
			if strings.ToLower(u.EPC()) != u.EPC() {
				t.Errorf("unit %s EPC is not lowercase (migration 1792 lowercases the column)", u.Code)
			}
		}

		if n.Timeclock && (len(n.Stock) > 0 || len(n.Units) > 0) {
			t.Errorf("node %s is the virtual timeclock terminal and must stock nothing", n.Code)
		}
	}
}

// TestEPCIsStable pins the derivation. A re-seed has to produce the same
// tag values or any tag file written for a mock reader goes stale.
func TestEPCIsStable(t *testing.T) {
	u := demoseed.Unit{Code: "PT-2010-0001"}
	first := u.EPC()
	if second := (demoseed.Unit{Code: "PT-2010-0001"}).EPC(); first != second {
		t.Fatalf("EPC is not deterministic: %q then %q", first, second)
	}
	if other := (demoseed.Unit{Code: "PT-2010-0002"}).EPC(); other == first {
		t.Fatal("different unit codes derive the same EPC")
	}
}

// TestItemCodesDerivesMembership — membership is derived from Stock plus
// Units rather than listed separately, so a fixture can't declare a SKU it
// doesn't stock.
func TestItemCodesDerivesMembership(t *testing.T) {
	node, err := demoseed.NodeByCode("KC-DC1-CRIB")
	if err != nil {
		t.Fatal(err)
	}
	got := node.ItemCodes()
	want := len(node.Stock) + countDistinctUnitItems(node)
	if len(got) != want {
		t.Fatalf("ItemCodes() = %d codes, want %d (%d stock + %d distinct serialized SKUs)",
			len(got), want, len(node.Stock), countDistinctUnitItems(node))
	}
	// Sorted, so the membership rows are created in a stable order.
	for i := 1; i < len(got); i++ {
		if got[i-1] >= got[i] {
			t.Fatalf("ItemCodes() is not sorted: %v", got)
		}
	}
	if cabinets := node.EnclosureIDs(); len(cabinets) != 2 {
		t.Fatalf("KC-DC1-CRIB should have two cabinets, got %v", cabinets)
	}
}

func countDistinctUnitItems(n demoseed.Node) int {
	seen := map[string]bool{}
	for _, u := range n.Units {
		seen[u.ItemCode] = true
	}
	return len(seen)
}

// TestSeedCatalogIsIdempotent is the contract: a second full run creates
// nothing, so a run that died partway heals on the next one and a
// hand-edited demo survives a re-seed.
func TestSeedCatalogIsIdempotent(t *testing.T) {
	app := setupApp(t)

	first, err := demoseed.SeedCatalog(app, nil, nil)
	if err != nil {
		t.Fatalf("first seed: %v", err)
	}
	if first.Created == 0 {
		t.Fatal("first seed created nothing")
	}
	if first.Existing != 0 {
		t.Fatalf("first seed found %d existing rows in a fresh database", first.Existing)
	}

	second, err := demoseed.SeedCatalog(app, nil, nil)
	if err != nil {
		t.Fatalf("second seed: %v", err)
	}
	if second.Created != 0 {
		t.Fatalf("second seed created %d rows; it must create none", second.Created)
	}
	if second.Existing != first.Created {
		t.Fatalf("second seed found %d rows, want the %d the first created", second.Existing, first.Created)
	}

	assertCount(t, app, "groups", len(demoseed.Groups))
	assertCount(t, app, "users", len(demoseed.Users))
	assertCount(t, app, "items", len(demoseed.Items))
}

// TestSeedCatalogScopesToANode — the standalone kiosk gets only the SKUs
// it stocks. A standalone kiosk has no controller to receive membership
// from, so carrying the whole catalogue would misrepresent the product.
func TestSeedCatalogScopesToANode(t *testing.T) {
	app := setupApp(t)
	node, err := demoseed.NodeByCode("SGF-XD2-CRIB")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := demoseed.SeedCatalog(app, node.ItemCodes(), nil); err != nil {
		t.Fatalf("seed: %v", err)
	}
	assertCount(t, app, "items", len(node.ItemCodes()))
	if len(node.ItemCodes()) >= len(demoseed.Items) {
		t.Fatal("SGF-XD2-CRIB should stock a strict subset of the catalogue")
	}
}

// TestApplyLocalInstallsAndIsIdempotent covers the standalone demo's whole
// second half.
func TestApplyLocalInstallsAndIsIdempotent(t *testing.T) {
	kioskctx.Set(kioskctx.Identity{KioskCode: "KC-DC1-CRIB", LocationCode: "KC-DC1"})
	app := setupApp(t)
	node, err := demoseed.NodeByCode("KC-DC1-CRIB")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := demoseed.SeedCatalog(app, node.ItemCodes(), nil); err != nil {
		t.Fatalf("seed catalogue: %v", err)
	}

	first, err := demoseed.ApplyLocal(app, node.Code, nil)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if first.Applied == 0 {
		t.Fatal("apply did nothing")
	}

	// Quantities and thresholds landed on the quantity-tracked SKUs.
	for _, s := range node.Stock {
		rec := findItem(t, app, s.ItemCode)
		if got := rec.GetInt("quantity_on_hand"); got != s.Quantity {
			t.Errorf("%s quantity_on_hand = %d, want %d", s.ItemCode, got, s.Quantity)
		}
		if got := rec.GetInt("reorder_threshold"); got != s.ReorderThreshold {
			t.Errorf("%s reorder_threshold = %d, want %d", s.ItemCode, got, s.ReorderThreshold)
		}
	}

	// Units landed with their cabinet and their tag.
	for _, u := range node.Units {
		rec, err := app.FindFirstRecordByFilter("item_instances", "code = {:c}", dbx.Params{"c": u.Code})
		if err != nil {
			t.Fatalf("unit %s: %v", u.Code, err)
		}
		if got := rec.GetString("enclosure_id"); got != u.EnclosureID {
			t.Errorf("unit %s enclosure_id = %q, want %q", u.Code, got, u.EnclosureID)
		}
		if got := rec.GetString("rfid_epc"); got != u.EPC() {
			t.Errorf("unit %s rfid_epc = %q, want %q", u.Code, got, u.EPC())
		}
		if got := rec.GetString("status"); got != instances.StatusInService {
			t.Errorf("unit %s status = %q, want in_service", u.Code, got)
		}
	}

	// The item_instances hooks — not the applier — recompute the
	// serialized SKUs' quantity_on_hand from the non-retired unit count.
	perItem := map[string]int{}
	for _, u := range node.Units {
		perItem[u.ItemCode]++
	}
	for code, want := range perItem {
		if got := findItem(t, app, code).GetInt("quantity_on_hand"); got != want {
			t.Errorf("serialized %s quantity_on_hand = %d, want %d from the instance-count recompute", code, got, want)
		}
	}

	second, err := demoseed.ApplyLocal(app, node.Code, nil)
	if err != nil {
		t.Fatalf("re-apply: %v", err)
	}
	if second.Applied != 0 {
		t.Fatalf("re-apply performed %d mutations; it must perform none", second.Applied)
	}
	// No zero-delta stock_adjustments rows on the re-run.
	assertCount(t, app, "stock_adjustments", len(node.Stock))
}

// TestApplyLocalRejectsTheTimeclockTerminal — KC-OFFICE deliberately has
// no kiosk, and pointing the seeder at the terminal is a config mistake
// worth naming.
func TestApplyLocalRejectsTheTimeclockTerminal(t *testing.T) {
	app := setupApp(t)
	_, err := demoseed.ApplyLocal(app, "KC-OFFICE-TC", nil)
	if err == nil {
		t.Fatal("expected an error for the virtual timeclock terminal")
	}
	if !strings.Contains(err.Error(), "timeclock") {
		t.Fatalf("error should say why: %v", err)
	}
}

// TestNodeByCodeNamesWhatItKnew — the usual cause of this failure is
// running the seeder against a config whose kiosk.code isn't in the
// estate, and "not found" alone sends the operator to the wrong place.
func TestNodeByCodeNamesWhatItKnew(t *testing.T) {
	_, err := demoseed.NodeByCode("NOT-A-KIOSK")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "KC-DC1-CRIB") {
		t.Fatalf("error should list the known codes: %v", err)
	}
}

func findItem(t *testing.T, app core.App, code string) *core.Record {
	t.Helper()
	rec, err := app.FindFirstRecordByFilter("items", "code = {:c}", dbx.Params{"c": code})
	if err != nil {
		t.Fatalf("find item %s: %v", code, err)
	}
	return rec
}

func assertCount(t *testing.T, app core.App, collection string, want int) {
	t.Helper()
	rows, err := app.FindRecordsByFilter(collection, "id != ''", "", 0, 0)
	if err != nil {
		t.Fatalf("count %s: %v", collection, err)
	}
	if len(rows) != want {
		t.Errorf("%s has %d rows, want %d", collection, len(rows), want)
	}
}
