// Report-style CSV writers. Pure formatting — callers fetch/aggregate rows
// via the same code paths the JSON endpoints use, then hand the result to
// these writers. Keeping presentation isolated mirrors the items/transactions
// writers in csv.go and lets a future scheduled-export path reuse the same
// formatting without going through HTTP.
package exports

import (
	"encoding/csv"
	"fmt"
	"io"
	"math"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"

	"github.com/skeeeon/kiosk/internal/ledger"
)

// WriteOpenCheckoutsCSV streams hydrated open-checkout rows as CSV. The DTOs
// come from ledger.ReplayOpenRows + ledger.Hydrate on the kiosk binary, or
// from the projected `open_checkouts` table + Hydrate on the controller. The
// shape is identical because Hydrate normalizes both sources, so the writer
// doesn't need to branch.
func WriteOpenCheckoutsCSV(w io.Writer, rows []ledger.OpenCheckoutDTO) error {
	cw := csv.NewWriter(w)
	defer cw.Flush()

	if err := cw.Write([]string{
		"checked_out_at", "days_out", "kiosk_code",
		"item_code", "item_name", "item_type",
		"user_code", "user_name", "user_group",
		"serial", "transaction_line_id",
	}); err != nil {
		return err
	}

	now := time.Now()
	for _, r := range rows {
		var itemCode, itemName, itemType string
		if r.Expand.Item != nil {
			itemCode = r.Expand.Item.Code
			itemName = r.Expand.Item.Name
			itemType = r.Expand.Item.Type
		}
		var userCode, userName, userGroup string
		if r.Expand.User != nil {
			userCode = r.Expand.User.Code
			userName = r.Expand.User.Name
			userGroup = r.Expand.User.Group
		}
		days := math.Floor(now.Sub(r.CheckedOutAt).Hours() / 24)
		if err := writeRow(cw, []string{
			r.CheckedOutAt.Format(time.RFC3339),
			fmt.Sprintf("%.0f", days),
			r.KioskCode,
			itemCode, itemName, itemType,
			userCode, userName, userGroup,
			r.Serial,
			r.ID,
		}); err != nil {
			return err
		}
	}
	return nil
}

// LowStockRow is the unified row shape for the low-stock CSV. Kiosk-local
// callers populate KioskCode from kioskctx.Get(); the controller's fan-out
// populates it per row from the snapshot's originating kiosk. Both sides
// produce the same column layout so consumers don't branch.
type LowStockRow struct {
	KioskCode        string
	ItemCode         string
	ItemName         string
	TrackingMode     string
	QuantityOnHand   int
	Out              int
	Available        int
	ReorderThreshold int
}

// ComputeLowStockRows returns the items at or below their reorder threshold,
// computed exactly the way the low-stock CSV is built: active items with a
// positive threshold, where a tool's available = on_hand − currently-out drops
// to the threshold or below. Shared between the CSV report handler and the
// metrics snapshot so the low-stock count never drifts from the report.
//
// kioskCode stamps each row — kioskctx.Get().KioskCode on a kiosk binary; the
// controller's fan-out builds its rows per-snapshot elsewhere and doesn't call
// this.
func ComputeLowStockRows(app core.App, kioskCode string) ([]LowStockRow, error) {
	items, err := app.FindRecordsByFilter("items", "active = true", "code", 0, 0)
	if err != nil {
		return nil, fmt.Errorf("load items: %w", err)
	}
	opens, err := app.FindRecordsByFilter("open_checkouts", "", "", 0, 0)
	if err != nil {
		return nil, fmt.Errorf("load open_checkouts: %w", err)
	}
	openByItem := map[string]int{}
	for _, o := range opens {
		openByItem[o.GetString("item")]++
	}

	out := make([]LowStockRow, 0, len(items))
	for _, it := range items {
		threshold := it.GetInt("reorder_threshold")
		if threshold <= 0 {
			continue
		}
		onHand := it.GetInt("quantity_on_hand")
		outCount := 0
		available := onHand
		if it.GetString("type") == "tool" {
			outCount = openByItem[it.Id]
			available = onHand - outCount
			if available < 0 {
				available = 0
			}
		}
		if available > threshold {
			continue
		}
		out = append(out, LowStockRow{
			KioskCode:        kioskCode,
			ItemCode:         it.GetString("code"),
			ItemName:         it.GetString("name"),
			TrackingMode:     it.GetString("tracking_mode"),
			QuantityOnHand:   onHand,
			Out:              outCount,
			Available:        available,
			ReorderThreshold: threshold,
		})
	}
	return out, nil
}

// GroupActivityOptions narrows the group-activity export. From/To are
// YYYY-MM-DD; the writer expands them to inclusive day boundaries against
// completed_at. KioskCode filters fleet-wide projections on the controller;
// on a kiosk binary it's redundant (single-kiosk data) but harmless.
type GroupActivityOptions struct {
	From      string
	To        string
	KioskCode string
}

// WriteGroupActivityCSV rolls up completed transactions and their lines by
// user_group, then emits one CSV row per group. Mirrors the SPA's
// loadGroupActivity aggregation so the export matches the on-screen table.
func WriteGroupActivityCSV(app core.App, w io.Writer, opts GroupActivityOptions) error {
	txFilter, txParams := buildGroupActivityTxFilter(opts)
	txs, err := app.FindRecordsByFilter("transactions", txFilter, "-completed_at", 0, 0, txParams)
	if err != nil {
		return fmt.Errorf("find transactions: %w", err)
	}

	cw := csv.NewWriter(w)
	defer cw.Flush()

	if err := cw.Write([]string{
		"group_code", "group_name", "contact_email",
		"transactions", "checked_out", "returned", "consumed",
	}); err != nil {
		return err
	}
	if len(txs) == 0 {
		return nil
	}

	type bucket struct {
		txCount, checkedOut, returned, consumed int
	}
	groupByTx := make(map[string]string, len(txs))
	buckets := map[string]*bucket{}
	for _, t := range txs {
		g := t.GetString("user_group")
		groupByTx[t.Id] = g
		b := buckets[g]
		if b == nil {
			b = &bucket{}
			buckets[g] = b
		}
		b.txCount++
	}

	linesFilter, linesParams := buildGroupActivityLinesFilter(opts)
	lines, err := app.FindRecordsByFilter("transaction_lines", linesFilter, "", 0, 0, linesParams)
	if err != nil {
		return fmt.Errorf("find transaction_lines: %w", err)
	}
	for _, l := range lines {
		g, ok := groupByTx[l.GetString("transaction")]
		if !ok {
			continue
		}
		b := buckets[g]
		if b == nil {
			continue
		}
		switch l.GetString("action") {
		case "checkout":
			b.checkedOut++
		case "return":
			b.returned++
		case "consume":
			b.consumed++
		}
	}

	groups, err := app.FindRecordsByFilter("groups", "", "", 0, 0)
	if err != nil {
		return fmt.Errorf("find groups: %w", err)
	}
	groupMeta := make(map[string]*core.Record, len(groups))
	for _, g := range groups {
		groupMeta[g.GetString("code")] = g
	}

	codes := make([]string, 0, len(buckets))
	for code := range buckets {
		codes = append(codes, code)
	}
	// Sort by transaction count desc so the SPA's visual ordering matches the export.
	for i := 0; i < len(codes); i++ {
		for j := i + 1; j < len(codes); j++ {
			if buckets[codes[j]].txCount > buckets[codes[i]].txCount {
				codes[i], codes[j] = codes[j], codes[i]
			}
		}
	}

	for _, code := range codes {
		b := buckets[code]
		name := code
		contactEmail := ""
		if code == "" {
			name = "(ungrouped)"
		}
		if meta := groupMeta[code]; meta != nil {
			if n := meta.GetString("name"); n != "" {
				name = n
			}
			contactEmail = meta.GetString("contact_email")
		}
		if err := writeRow(cw, []string{
			code, name, contactEmail,
			fmt.Sprintf("%d", b.txCount),
			fmt.Sprintf("%d", b.checkedOut),
			fmt.Sprintf("%d", b.returned),
			fmt.Sprintf("%d", b.consumed),
		}); err != nil {
			return err
		}
	}
	return nil
}

func buildGroupActivityTxFilter(opts GroupActivityOptions) (string, dbx.Params) {
	parts := []string{`status = "completed"`}
	params := dbx.Params{}
	if opts.From != "" {
		parts = append(parts, "completed_at >= {:from}")
		params["from"] = opts.From + " 00:00:00.000Z"
	}
	if opts.To != "" {
		parts = append(parts, "completed_at <= {:to}")
		params["to"] = opts.To + " 23:59:59.999Z"
	}
	if opts.KioskCode != "" {
		parts = append(parts, "kiosk_code = {:k}")
		params["k"] = opts.KioskCode
	}
	return joinAnd(parts), params
}

func buildGroupActivityLinesFilter(opts GroupActivityOptions) (string, dbx.Params) {
	// Indirect filter through the parent transaction; matches the SPA's
	// approach so the date/kiosk slice agrees with the transactions query.
	parts := []string{`transaction.status = "completed"`}
	params := dbx.Params{}
	if opts.From != "" {
		parts = append(parts, "transaction.completed_at >= {:from}")
		params["from"] = opts.From + " 00:00:00.000Z"
	}
	if opts.To != "" {
		parts = append(parts, "transaction.completed_at <= {:to}")
		params["to"] = opts.To + " 23:59:59.999Z"
	}
	if opts.KioskCode != "" {
		parts = append(parts, "transaction.kiosk_code = {:k}")
		params["k"] = opts.KioskCode
	}
	return joinAnd(parts), params
}

func joinAnd(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += " && "
		}
		out += p
	}
	return out
}

// AdjustmentAuditOptions narrows the inventory_audit export. From/To are
// YYYY-MM-DD (inclusive). KioskCode and Source filter by their respective
// columns; both empty means "all."
type AdjustmentAuditOptions struct {
	From      string
	To        string
	KioskCode string
	Source    string // "local" | "controller" | "" (no filter)
}

// WriteAdjustmentAuditCSV streams the controller's inventory_audit
// collection. Idempotency / fan-out is the projector's job — the CSV is a
// straight read of what's on disk, ordered newest-first to match the SPA.
func WriteAdjustmentAuditCSV(app core.App, w io.Writer, opts AdjustmentAuditOptions) error {
	parts := []string{}
	params := dbx.Params{}
	if opts.From != "" {
		parts = append(parts, "created >= {:from}")
		params["from"] = opts.From + " 00:00:00.000Z"
	}
	if opts.To != "" {
		parts = append(parts, "created <= {:to}")
		params["to"] = opts.To + " 23:59:59.999Z"
	}
	if opts.KioskCode != "" {
		parts = append(parts, "kiosk_code = {:k}")
		params["k"] = opts.KioskCode
	}
	if opts.Source != "" {
		parts = append(parts, "source = {:s}")
		params["s"] = opts.Source
	}

	rows, err := app.FindRecordsByFilter("inventory_audit", joinAnd(parts), "-created", 0, 0, params)
	if err != nil {
		return fmt.Errorf("find inventory_audit: %w", err)
	}

	cw := csv.NewWriter(w)
	defer cw.Flush()

	if err := cw.Write([]string{
		"created", "occurred_at", "kiosk_code",
		"item_code", "item_name",
		"source", "mode", "delta", "prev_quantity", "new_quantity",
		"reason", "admin_id", "source_adjustment_id", "command_id",
	}); err != nil {
		return err
	}
	for _, r := range rows {
		occurred := ""
		if d := r.GetDateTime("occurred_at").Time(); !d.IsZero() {
			occurred = d.Format(time.RFC3339)
		}
		if err := writeRow(cw, []string{
			r.GetDateTime("created").Time().Format(time.RFC3339),
			occurred,
			r.GetString("kiosk_code"),
			r.GetString("item_code"),
			r.GetString("item_name"),
			r.GetString("source"),
			r.GetString("mode"),
			fmt.Sprintf("%d", r.GetInt("delta")),
			fmt.Sprintf("%d", r.GetInt("prev_quantity")),
			fmt.Sprintf("%d", r.GetInt("new_quantity")),
			r.GetString("reason"),
			r.GetString("admin_id"),
			r.GetString("source_adjustment_id"),
			r.GetString("command_id"),
		}); err != nil {
			return err
		}
	}
	return nil
}

// NotificationsLogOptions narrows the notification_send_log export.
// LookbackDays defaults to 7 when zero (matches the SPA's default lookback).
type NotificationsLogOptions struct {
	LookbackDays int
}

// WriteNotificationsLogCSV streams the notification_send_log rows for the
// lookback window. Same schema on kiosk and controller — both binaries have
// the collection (templates landed on both; the log on both since notifier
// can fire from either).
func WriteNotificationsLogCSV(app core.App, w io.Writer, opts NotificationsLogOptions) error {
	days := opts.LookbackDays
	if days <= 0 {
		days = 7
	}
	cutoff := time.Now().UTC().Add(-time.Duration(days) * 24 * time.Hour)
	filter := "created >= {:cutoff}"
	params := dbx.Params{"cutoff": cutoff.Format("2006-01-02 15:04:05.000Z")}

	rows, err := app.FindRecordsByFilter("notification_send_log", filter, "-created", 0, 0, params)
	if err != nil {
		return fmt.Errorf("find notification_send_log: %w", err)
	}

	cw := csv.NewWriter(w)
	defer cw.Flush()

	if err := cw.Write([]string{
		"created", "event_type", "recipient", "status", "error", "payload_summary",
	}); err != nil {
		return err
	}
	for _, r := range rows {
		if err := writeRow(cw, []string{
			r.GetDateTime("created").Time().Format(time.RFC3339),
			r.GetString("event_type"),
			r.GetString("recipient"),
			r.GetString("status"),
			r.GetString("error"),
			r.GetString("payload_summary"),
		}); err != nil {
			return err
		}
	}
	return nil
}

// LifecycleAuditRow is the unified row shape for the instance-lifecycle
// CSV. The two source collections (kiosk-side `instance_audit` and
// controller-side `instance_lifecycle_audit`) carry different denormalization
// strategies; the handlers flatten both into this shape before calling the
// writer.
type LifecycleAuditRow struct {
	Created      time.Time
	KioskCode    string // empty on kiosk-local rows (single-kiosk by definition)
	ItemCode     string
	ItemName     string
	InstanceID   string
	InstanceCode string
	Action       string
	PrevActive   bool
	NewActive    bool
	Source       string
	Reason       string
	AdminID      string
}

// WriteLifecycleAuditCSV streams the flattened lifecycle rows. Column
// ordering matches the SPA's table for consistency between screen and file.
func WriteLifecycleAuditCSV(w io.Writer, rows []LifecycleAuditRow) error {
	cw := csv.NewWriter(w)
	defer cw.Flush()

	if err := cw.Write([]string{
		"created", "kiosk_code",
		"item_code", "item_name",
		"instance_id", "instance_code",
		"action", "prev_active", "new_active",
		"source", "reason", "admin_id",
	}); err != nil {
		return err
	}
	for _, r := range rows {
		if err := writeRow(cw, []string{
			r.Created.Format(time.RFC3339),
			r.KioskCode,
			r.ItemCode, r.ItemName,
			r.InstanceID, r.InstanceCode,
			r.Action,
			boolString(r.PrevActive), boolString(r.NewActive),
			r.Source, r.Reason, r.AdminID,
		}); err != nil {
			return err
		}
	}
	return nil
}

func boolString(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// WriteLowStockCSV streams the unified low-stock rows. Errors from a
// controller fan-out (offline kiosks) are NOT embedded here — the JSON
// endpoint already surfaces them; this writer is data-only.
func WriteLowStockCSV(w io.Writer, rows []LowStockRow) error {
	cw := csv.NewWriter(w)
	defer cw.Flush()

	if err := cw.Write([]string{
		"kiosk_code", "item_code", "item_name", "tracking_mode",
		"quantity_on_hand", "out", "available", "reorder_threshold",
	}); err != nil {
		return err
	}
	for _, r := range rows {
		if err := writeRow(cw, []string{
			r.KioskCode,
			r.ItemCode,
			r.ItemName,
			r.TrackingMode,
			fmt.Sprintf("%d", r.QuantityOnHand),
			fmt.Sprintf("%d", r.Out),
			fmt.Sprintf("%d", r.Available),
			fmt.Sprintf("%d", r.ReorderThreshold),
		}); err != nil {
			return err
		}
	}
	return nil
}
