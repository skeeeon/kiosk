package handlers

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"

	"github.com/skeeeon/kiosk/internal/scan"
)

const (
	itemsListDefaultPerPage = 50
	itemsListMaxPerPage     = 100
)

// ItemsList returns active items for the in-cart browse picker. It's
// anonymous on the same trust-boundary argument as the rest of /api/kiosk/*:
// the kiosk box is bound to localhost and behind a physically-secured screen.
//
// Filters: q (substring match against code or name), type ("tool" |
// "consumable"). Results are sorted by name and paginated; `has_more` is a
// cheap proxy (true when the page is full).
func (h *Handlers) ItemsList(re *core.RequestEvent) error {
	q := strings.TrimSpace(re.Request.URL.Query().Get("q"))
	typeFilter := re.Request.URL.Query().Get("type")

	perPage := itemsListDefaultPerPage
	if v := re.Request.URL.Query().Get("per_page"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= itemsListMaxPerPage {
			perPage = n
		}
	}
	page := 1
	if v := re.Request.URL.Query().Get("page"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 1 {
			page = n
		}
	}

	filter := "active = true"
	params := dbx.Params{}
	if q != "" {
		filter += " && (name ~ {:q} || code ~ {:q})"
		params["q"] = q
	}
	if typeFilter == "tool" || typeFilter == "consumable" {
		filter += " && type = {:type}"
		params["type"] = typeFilter
	}

	offset := (page - 1) * perPage
	rows, err := h.App.FindRecordsByFilter("items", filter, "name", perPage, offset, params)
	if err != nil {
		return err
	}

	items := make([]*scan.Item, 0, len(rows))
	for _, r := range rows {
		items = append(items, itemFromRecord(r))
	}

	return re.JSON(http.StatusOK, map[string]any{
		"items":    items,
		"page":     page,
		"per_page": perPage,
		"has_more": len(rows) == perPage,
	})
}
