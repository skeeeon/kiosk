package handlers

import (
	"net/http"
	"strings"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"

	"github.com/skeeeon/kiosk/internal/scan"
)

// ItemsList returns active items for the in-cart browse picker. It's
// anonymous on the same trust-boundary argument as the rest of /api/kiosk/*:
// the kiosk box is bound to localhost and behind a physically-secured screen.
//
// Filters: q (substring match against code, name, category, or notes),
// type ("tool" | "consumable"). Results are sorted by name and returned in
// a single payload — the kiosk catalog is membership-bounded via kiosk_items
// so pagination would be premature here.
//
// OpenCount is populated via a batched GROUP BY on open_checkouts so the SPA
// can render accurate per-item availability (qty_on_hand − open_count for
// tools; qty_on_hand for consumables, where qty_on_hand already decrements
// on consume).
func (h *Handlers) ItemsList(re *core.RequestEvent) error {
	q := strings.TrimSpace(re.Request.URL.Query().Get("q"))
	typeFilter := re.Request.URL.Query().Get("type")

	filter := "active = true"
	params := dbx.Params{}
	if q != "" {
		filter += " && (name ~ {:q} || code ~ {:q} || category ~ {:q} || notes ~ {:q})"
		params["q"] = q
	}
	if typeFilter == "tool" || typeFilter == "consumable" {
		filter += " && type = {:type}"
		params["type"] = typeFilter
	}

	rows, err := h.App.FindRecordsByFilter("items", filter, "name", 0, 0, params)
	if err != nil {
		return err
	}

	// Batched open-checkout counts. open_checkouts is bounded by what's
	// currently out (small), so an ungated GROUP BY beats building an
	// IN (?, ?, …) clause for the page.
	type openRow struct {
		Item  string `db:"item"`
		Count int    `db:"c"`
	}
	var openRows []openRow
	if err := h.App.DB().NewQuery("SELECT item, COUNT(*) c FROM open_checkouts GROUP BY item").All(&openRows); err != nil {
		return err
	}
	counts := make(map[string]int, len(openRows))
	for _, r := range openRows {
		counts[r.Item] = r.Count
	}

	items := make([]*scan.Item, 0, len(rows))
	for _, r := range rows {
		it := itemFromRecord(r)
		it.OpenCount = counts[r.Id]
		items = append(items, it)
	}

	return re.JSON(http.StatusOK, map[string]any{
		"items": items,
	})
}
