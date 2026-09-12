// Package demoseed holds the Northwind Traders demo estate: one fixture
// table plus the two appliers that install it.
//
// See docs/demo-plan.md for the reasoning. The two things worth knowing
// before reading further:
//
//	Seed flows down, truth flows up. The catalogue (groups, users, items,
//	membership) is written on the CONTROLLER and reaches kiosks through the
//	JetStream KV buckets. The ledger is never seeded — kiosks write it and
//	it flows back up through the event stream.
//
//	The remote applier is the local applier with NATS in the middle.
//	Kiosk-local state (quantities, reorder thresholds, serialized units)
//	does not cross the catalogue wire by design, so it is installed either
//	by calling the kiosk's own mutation functions in-process (ApplyLocal)
//	or by sending the same values as commands over the bus (ApplyRemote).
//	One fixture, one set of mutation functions, two transports.
//
// This is demo tooling. It never runs on a customer install, and the
// password it writes is deliberately weak and well-known.
package demoseed

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
)

// DemoAdminEmail / DemoAdminPassword is the well-known admin the seeder
// creates in the `admins` collection on whichever binary it runs against.
// The bootstrap admin's password is printed once and then lost, which is
// exactly wrong for a demo that gets re-stood-up weekly.
const (
	DemoAdminEmail    = "admin@northwind.example"
	DemoAdminPassword = "northwind-demo"
	DemoAdminName     = "Northwind Demo Admin"
)

// Group is one crew. Groups exist so a foreman can return tools on behalf
// of their own crew and no one else's — the gate commit.Commit enforces.
type Group struct {
	Code, Name, ContactEmail string
}

// User is one worker. Code is the badge value the checkout flow scans and
// the `user_code` the demo rules post. Codes, names and emails are shared
// verbatim with the access-control demo's Northwind cardholders: Elena
// badges through the freezer door there and checks out a pallet jack here.
type User struct {
	Code, Name, Email, Role, Group string
}

// Item is one SKU. Quantity-on-hand and reorder threshold are NOT here —
// they are per-node physical state and live on the Node below, because the
// same SKU at two kiosks holds its own count.
type Item struct {
	Code, Name, Type, TrackingMode, Unit, Category, Notes string

	// RequiresMaintenance routes every serialized return of this SKU
	// straight to the maintenance bench. On a digital torque wrench that
	// is a real policy (it needs recalibrating), and it is the cheapest
	// way to get the maintenance queue populated without simulating an
	// operator sending anything there.
	RequiresMaintenance bool
}

// Stock is one quantity-tracked SKU's physical state at one node.
// Serialized SKUs never appear here: their quantity_on_hand is a
// materialized view of the non-retired instance count, and
// PerformStockAdjustment rejects them outright.
type Stock struct {
	ItemCode         string
	Quantity         int
	ReorderThreshold int
}

// Unit is one serialized physical unit at one node. EnclosureID names the
// RFID cabinet it lives in and MUST match an enclosure_id in that node's
// kiosk.yaml — there is no enclosures collection, the YAML is the registry
// (see config.RFIDConfig.EnclosureIDs). A unit assigned to a cabinet no
// reader reads silently drops out of that cabinet's enclosure_diff set.
type Unit struct {
	ItemCode, Code, Serial, EnclosureID string
}

// EPC is the unit's RFID tag value, derived from its code so a re-seed
// produces the same tag and any tag files written for a mock reader stay
// valid. Lowercase because migration 1792 lowercases
// item_instances.rfid_epc, and the column is unique-when-non-empty.
func (u Unit) EPC() string {
	sum := sha256.Sum256([]byte("northwind-demo-epc/" + u.Code))
	return "e2801160" + hex.EncodeToString(sum[:8])
}

// Node is one kiosk or the virtual timeclock terminal. Membership in
// `kiosk_items` is DERIVED from Stock + Units rather than listed
// separately, so a fixture can't declare a SKU it doesn't stock.
type Node struct {
	Code, LocationCode, Notes string

	// Port is documentation only — it records which demo/*.yaml binds
	// where, so the rules files and this table can be read against each
	// other.
	Port int

	// Timeclock marks the virtual terminal: cmd/timeclock, no catalogue,
	// no kiosk_items rows. It gets a `kiosks` row anyway so the fleet list
	// is complete before it has ever spoken.
	Timeclock bool

	Stock []Stock
	Units []Unit
}

// ItemCodes returns the distinct SKUs this node stocks, sorted. This is
// the kiosk_items membership list — and creating those rows, not creating
// the items, is what makes catalogue appear at a kiosk.
func (n Node) ItemCodes() []string {
	seen := map[string]bool{}
	for _, s := range n.Stock {
		seen[s.ItemCode] = true
	}
	for _, u := range n.Units {
		seen[u.ItemCode] = true
	}
	out := make([]string, 0, len(seen))
	for c := range seen {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// EnclosureIDs returns the distinct cabinets this node's units live in,
// sorted. The demo kiosk.yaml for this node must declare an
// enclosure_diff reader for each.
func (n Node) EnclosureIDs() []string {
	seen := map[string]bool{}
	var out []string
	for _, u := range n.Units {
		if u.EnclosureID != "" && !seen[u.EnclosureID] {
			seen[u.EnclosureID] = true
			out = append(out, u.EnclosureID)
		}
	}
	sort.Strings(out)
	return out
}

// NodeByCode looks a node up by kiosk code. The error names every code it
// did know about, because the usual cause is running the seeder against a
// config whose kiosk.code isn't part of the demo estate.
func NodeByCode(code string) (Node, error) {
	for _, n := range Nodes {
		if n.Code == code {
			return n, nil
		}
	}
	codes := make([]string, 0, len(Nodes))
	for _, n := range Nodes {
		codes = append(codes, n.Code)
	}
	sort.Strings(codes)
	return Node{}, fmt.Errorf("no demo node with kiosk code %q (known: %s)",
		code, strings.Join(codes, ", "))
}

// ItemByCode looks a SKU up in the fixture.
func ItemByCode(code string) (Item, bool) {
	for _, it := range Items {
		if it.Code == code {
			return it, true
		}
	}
	return Item{}, false
}

// ---- the fixture ----

// Groups are the crews. nw-warehouse and nw-dock each have a foreman so
// the on-behalf-of return dialog has somewhere to point.
var Groups = []Group{
	{Code: "nw-warehouse", Name: "Warehouse", ContactEmail: "warehouse@northwind.example"},
	{Code: "nw-dock", Name: "Dock & Receiving", ContactEmail: "dock@northwind.example"},
	{Code: "nw-office", Name: "Office", ContactEmail: "office@northwind.example"},
	{Code: "nw-maint", Name: "Maintenance", ContactEmail: "maintenance@northwind.example"},
}

// Users. Codes, names and emails match access-control/internal/demoseed's
// Northwind cardholders exactly — one company, two apps. Two foremen, three
// workers; five people against a ten-ish SKU pool is what makes the
// checkout loop settle (see "The catalogue is sized for the loop to close"
// in docs/demo-plan.md).
var Users = []User{
	{Code: "nw-elena", Name: "Elena Sokolova", Email: "elena@northwind.example", Role: "foreman", Group: "nw-warehouse"},
	{Code: "nw-marco", Name: "Marco Ferreira", Email: "marco@northwind.example", Role: "worker", Group: "nw-warehouse"},
	{Code: "nw-dana", Name: "Dana Whitfield", Email: "dana@northwind.example", Role: "foreman", Group: "nw-dock"},
	{Code: "nw-owen", Name: "Owen Pryce", Email: "owen@northwind.example", Role: "worker", Group: "nw-dock"},
	{Code: "nw-raj", Name: "Raj Malhotra", Email: "raj@northwind.example", Role: "worker", Group: "nw-maint"},
}

// Items. Categories are not a stored rule — they exist so the "bulk add by
// category" action on the controller's KioskItemsPanel has something to
// demonstrate.
var Items = []Item{
	// hand-tools — quantity-tracked. The bread and butter of a crib: no
	// serial, no tag, N of them on a shelf.
	{Code: "HT-1010", Name: "Crescent Wrench 10in", Type: "tool", TrackingMode: "quantity", Unit: "each", Category: "hand-tools"},
	{Code: "HT-1020", Name: "Pipe Wrench 18in", Type: "tool", TrackingMode: "quantity", Unit: "each", Category: "hand-tools"},
	{Code: "HT-1030", Name: "Socket Set 3/8in Drive", Type: "tool", TrackingMode: "quantity", Unit: "each", Category: "hand-tools"},
	{Code: "HT-1040", Name: "Bolt Cutters 24in", Type: "tool", TrackingMode: "quantity", Unit: "each", Category: "hand-tools"},
	{Code: "HT-1050", Name: "Pry Bar 36in", Type: "tool", TrackingMode: "quantity", Unit: "each", Category: "hand-tools"},

	// power-tools — serialized, tagged, and the population of the RFID
	// cabinets. These are the units enclosure_diff notices leaving.
	{Code: "PT-2010", Name: "Impact Driver 18V", Type: "tool", TrackingMode: "serialized", Unit: "each", Category: "power-tools"},
	{Code: "PT-2020", Name: "Digital Torque Wrench", Type: "tool", TrackingMode: "serialized", Unit: "each", Category: "power-tools",
		RequiresMaintenance: true,
		Notes:               "Returns route to the bench for recalibration."},
	{Code: "PT-2030", Name: "Reciprocating Saw", Type: "tool", TrackingMode: "serialized", Unit: "each", Category: "power-tools"},
	{Code: "PT-2040", Name: "Rotary Hammer", Type: "tool", TrackingMode: "serialized", Unit: "each", Category: "power-tools"},
	{Code: "PT-2050", Name: "Heat Gun", Type: "tool", TrackingMode: "serialized", Unit: "each", Category: "power-tools"},

	// materials-handling — quantity-tracked tools. Elena's pallet jack.
	{Code: "MH-5010", Name: "Pallet Jack", Type: "tool", TrackingMode: "quantity", Unit: "each", Category: "materials-handling"},
	{Code: "MH-5020", Name: "Hand Truck", Type: "tool", TrackingMode: "quantity", Unit: "each", Category: "materials-handling"},
	{Code: "MH-5030", Name: "Load Bar", Type: "tool", TrackingMode: "quantity", Unit: "each", Category: "materials-handling"},

	// ppe — a deliberate mix. Jackets and hard hats come back (tool);
	// gloves and glasses do not (consumable). Same category, opposite
	// ledger behaviour, which is the distinction worth showing.
	{Code: "PP-3010", Name: "Freezer Jacket", Type: "tool", TrackingMode: "quantity", Unit: "each", Category: "ppe"},
	{Code: "PP-3020", Name: "Cut-Resistant Gloves L", Type: "consumable", TrackingMode: "quantity", Unit: "pair", Category: "ppe"},
	{Code: "PP-3030", Name: "Safety Glasses", Type: "consumable", TrackingMode: "quantity", Unit: "each", Category: "ppe"},
	{Code: "PP-3040", Name: "Hard Hat", Type: "tool", TrackingMode: "quantity", Unit: "each", Category: "ppe"},

	// consumables — exempt from the pool arithmetic entirely, because
	// `consume` never writes an open_checkouts row. They can be as
	// numerous as the demo likes.
	{Code: "CS-4010", Name: "Utility Blades 100ct", Type: "consumable", TrackingMode: "quantity", Unit: "box", Category: "consumables"},
	{Code: "CS-4020", Name: "Stretch Wrap Roll", Type: "consumable", TrackingMode: "quantity", Unit: "roll", Category: "consumables"},
	{Code: "CS-4030", Name: "Packing Tape", Type: "consumable", TrackingMode: "quantity", Unit: "roll", Category: "consumables"},
	{Code: "CS-4040", Name: "AA Batteries 24pk", Type: "consumable", TrackingMode: "quantity", Unit: "pack", Category: "consumables"},
	{Code: "CS-4050", Name: "Nitrile Gloves 100ct", Type: "consumable", TrackingMode: "quantity", Unit: "box", Category: "consumables"},
}

// Nodes. Three kiosks and a virtual timeclock terminal, chosen to cover
// every distinction the product makes — and no more. Sites come from the
// platform's own seed (KC-DC1, KC-OFFICE, SGF-XD2).
//
// Membership is the differentiator: the same SKU at two kiosks shares a
// code and holds its own quantity and its own units locally. That is the
// whole point of kiosk_items, and it is invisible unless the fixture
// deliberately makes the sites differ.
var Nodes = []Node{
	{
		// The flagship. Twelve tool SKUs against five workers settles at
		// roughly six held each — a Tools Out report that looks like a
		// working crib and stays that size overnight.
		Code:         "KC-DC1-CRIB",
		LocationCode: "KC-DC1",
		Notes:        "Main tool crib. Counter scanner plus two enclosure_diff cabinets (A, B).",
		Port:         8101,
		Stock: []Stock{
			{ItemCode: "HT-1010", Quantity: 12, ReorderThreshold: 4},
			{ItemCode: "HT-1020", Quantity: 6, ReorderThreshold: 2},
			{ItemCode: "HT-1030", Quantity: 9, ReorderThreshold: 3},
			{ItemCode: "HT-1040", Quantity: 4, ReorderThreshold: 2},
			{ItemCode: "HT-1050", Quantity: 7, ReorderThreshold: 2},
			{ItemCode: "MH-5010", Quantity: 5, ReorderThreshold: 2},
			{ItemCode: "MH-5020", Quantity: 8, ReorderThreshold: 3},
			// Deliberately seeded at its threshold so the low-stock alert
			// has something to find on the first digest without anyone
			// adjusting stock by hand.
			{ItemCode: "CS-4010", Quantity: 3, ReorderThreshold: 3},
			{ItemCode: "CS-4040", Quantity: 24, ReorderThreshold: 6},
		},
		Units: []Unit{
			{ItemCode: "PT-2010", Code: "PT-2010-0001", Serial: "IMP-88104-A", EnclosureID: "A"},
			{ItemCode: "PT-2010", Code: "PT-2010-0002", Serial: "IMP-88104-B", EnclosureID: "A"},
			{ItemCode: "PT-2020", Code: "PT-2020-0001", Serial: "TQD-20117-A", EnclosureID: "A"},
			{ItemCode: "PT-2020", Code: "PT-2020-0002", Serial: "TQD-20117-B", EnclosureID: "A"},
			{ItemCode: "PT-2030", Code: "PT-2030-0001", Serial: "RSW-43390", EnclosureID: "A"},
			{ItemCode: "PT-2040", Code: "PT-2040-0001", Serial: "RHM-55021-A", EnclosureID: "B"},
			{ItemCode: "PT-2040", Code: "PT-2040-0002", Serial: "RHM-55021-B", EnclosureID: "B"},
			{ItemCode: "PT-2050", Code: "PT-2050-0001", Serial: "HGN-71640", EnclosureID: "B"},
		},
	},
	{
		// What most installs actually are: a badge, a barcode scanner, and
		// a shelf of consumables. Same site as the crib, so the per-kiosk
		// catalogue difference is visible without changing buildings.
		Code:         "KC-DC1-DOCK",
		LocationCode: "KC-DC1",
		Notes:        "Dock consumables and PPE. Badge and barcode only — no RFID.",
		Port:         8102,
		Stock: []Stock{
			{ItemCode: "PP-3010", Quantity: 14, ReorderThreshold: 4},
			{ItemCode: "PP-3020", Quantity: 40, ReorderThreshold: 12},
			{ItemCode: "PP-3030", Quantity: 30, ReorderThreshold: 10},
			{ItemCode: "PP-3040", Quantity: 11, ReorderThreshold: 4},
			{ItemCode: "MH-5020", Quantity: 4, ReorderThreshold: 2},
			{ItemCode: "MH-5030", Quantity: 16, ReorderThreshold: 6},
			{ItemCode: "CS-4010", Quantity: 8, ReorderThreshold: 3},
			{ItemCode: "CS-4020", Quantity: 22, ReorderThreshold: 8},
			{ItemCode: "CS-4030", Quantity: 18, ReorderThreshold: 8},
			{ItemCode: "CS-4040", Quantity: 12, ReorderThreshold: 6},
			{ItemCode: "CS-4050", Quantity: 9, ReorderThreshold: 4},
		},
	},
	{
		// The second site. This is what makes the fleet real, gives the
		// controller something to aggregate across, and makes the
		// cross-kiosk clock-out gate demonstrable: a tool out at
		// Springfield blocking a clock-out in Kansas City.
		Code:         "SGF-XD2-CRIB",
		LocationCode: "SGF-XD2",
		Notes:        "Cross-dock crib. Counter scanner plus one enclosure_diff cabinet (A). A deliberately thinner slice of the KC catalogue.",
		Port:         8103,
		Stock: []Stock{
			{ItemCode: "HT-1010", Quantity: 5, ReorderThreshold: 2},
			{ItemCode: "HT-1030", Quantity: 3, ReorderThreshold: 2},
			{ItemCode: "HT-1040", Quantity: 2, ReorderThreshold: 1},
			{ItemCode: "MH-5020", Quantity: 3, ReorderThreshold: 1},
			{ItemCode: "CS-4030", Quantity: 10, ReorderThreshold: 4},
			{ItemCode: "CS-4040", Quantity: 6, ReorderThreshold: 3},
		},
		Units: []Unit{
			{ItemCode: "PT-2010", Code: "PT-2010-1001", Serial: "IMP-91455-A", EnclosureID: "A"},
			{ItemCode: "PT-2010", Code: "PT-2010-1002", Serial: "IMP-91455-B", EnclosureID: "A"},
			{ItemCode: "PT-2020", Code: "PT-2020-1001", Serial: "TQD-30882", EnclosureID: "A"},
		},
	},
	{
		// KC-OFFICE deliberately has no kiosk. Office staff punch from
		// their phones against the virtual terminal, which is exactly how
		// that binary is meant to be deployed. It gets a kiosks row so the
		// fleet list is complete, and no kiosk_items rows at all.
		Code:         "KC-OFFICE-TC",
		LocationCode: "KC-OFFICE",
		Notes:        "Virtual timeclock terminal (cmd/timeclock). Authenticated self-service punches from a phone; no checkout surface.",
		Port:         8092,
		Timeclock:    true,
	},
}
