package controller

import (
	"context"
	"fmt"
	"log/slog"
	"sort"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/pocketbase/pocketbase/core"
	"golang.org/x/sync/errgroup"

	"github.com/skeeeon/kiosk/internal/catalog"
)

// CatalogIntegrityReport diffs the controller's catalog DB state against the
// JetStream KV buckets. The controller is authoritative; "expected" is
// derived from kiosk_items + users records, "actual" is what's in KV right
// now.
//
// Used for both /api/kiosk/catalog/integrity (read-only inspection) and as
// the input to /api/kiosk/catalog/reconcile (which acts on the deltas).
type CatalogIntegrityReport struct {
	Items  CatalogIntegrityBucket `json:"items"`
	Users  CatalogIntegrityBucket `json:"users"`
	Groups CatalogIntegrityBucket `json:"groups"`
}

// CatalogIntegrityBucket is the per-bucket slice of the report. Keys are
// returned sorted so consecutive runs produce identical output, which makes
// diffs in the admin UI stable.
type CatalogIntegrityBucket struct {
	Bucket       string   `json:"bucket"`
	ExpectedKeys int      `json:"expected_keys"`
	ActualKeys   int      `json:"actual_keys"`
	MissingInKV  []string `json:"missing_in_kv"`
	ExtraInKV    []string `json:"extra_in_kv"`
}

// CatalogReconcileReport summarizes a reconcile run.
type CatalogReconcileReport struct {
	Items  CatalogReconcileBucket `json:"items"`
	Users  CatalogReconcileBucket `json:"users"`
	Groups CatalogReconcileBucket `json:"groups"`
}

// CatalogReconcileBucket counts the work done in one direction. Errors are
// per-key strings (not error values) so the JSON shape is stable and the
// admin UI can render them.
type CatalogReconcileBucket struct {
	Bucket        string   `json:"bucket"`
	Published     int      `json:"published"`
	Deleted       int      `json:"deleted"`
	PublishErrors []string `json:"publish_errors,omitempty"`
	DeleteErrors  []string `json:"delete_errors,omitempty"`
}

// expectedItemKeys walks the kiosk_items membership rows and resolves each
// to its "<kiosk_code>.<item_code>" KV key plus a payload byte-slice ready
// to Put. Used by both Integrity (keys-only) and Reconcile (payloads too).
func expectedItemKeys(app core.App) (map[string][]byte, error) {
	memberships, err := app.FindRecordsByFilter("kiosk_items", "", "", 0, 0)
	if err != nil {
		return nil, fmt.Errorf("load kiosk_items: %w", err)
	}
	// Bulk-load kiosks and items keyed by id to avoid N round-trips.
	kiosks, err := app.FindRecordsByFilter("kiosks", "", "", 0, 0)
	if err != nil {
		return nil, fmt.Errorf("load kiosks: %w", err)
	}
	kioskByID := make(map[string]*core.Record, len(kiosks))
	for _, k := range kiosks {
		kioskByID[k.Id] = k
	}
	items, err := app.FindRecordsByFilter("items", "", "", 0, 0)
	if err != nil {
		return nil, fmt.Errorf("load items: %w", err)
	}
	itemByID := make(map[string]*core.Record, len(items))
	for _, it := range items {
		itemByID[it.Id] = it
	}

	out := make(map[string][]byte, len(memberships))
	for _, m := range memberships {
		k, ok := kioskByID[m.GetString("kiosk")]
		if !ok {
			continue
		}
		it, ok := itemByID[m.GetString("item")]
		if !ok {
			continue
		}
		payload := itemPayloadFrom(it)
		data, err := catalog.MarshalItem(payload)
		if err != nil {
			return nil, fmt.Errorf("marshal item %s: %w", payload.Code, err)
		}
		key := k.GetString("kiosk_code") + "." + payload.Code
		out[key] = data
	}
	return out, nil
}

// expectedUserKeys walks the users collection and resolves each to its KV
// key (the user's code) plus a payload. Users are not kiosk-scoped — the
// bucket is shared. The user's `group` FK is resolved to the group's code
// here so the wire carries the human-readable identifier, not the
// controller-local id (kiosks have their own ids per record).
func expectedUserKeys(app core.App) (map[string][]byte, error) {
	users, err := app.FindRecordsByFilter("users", "", "", 0, 0)
	if err != nil {
		return nil, fmt.Errorf("load users: %w", err)
	}
	groups, err := app.FindRecordsByFilter("groups", "", "", 0, 0)
	if err != nil {
		return nil, fmt.Errorf("load groups: %w", err)
	}
	groupCodeByID := make(map[string]string, len(groups))
	for _, g := range groups {
		groupCodeByID[g.Id] = g.GetString("code")
	}
	out := make(map[string][]byte, len(users))
	for _, u := range users {
		code := u.GetString("code")
		if code == "" {
			continue
		}
		payload := catalog.UserPayload{
			Code:      code,
			Name:      u.GetString("name"),
			Email:     u.GetString("email"),
			Phone:     u.GetString("phone"),
			Role:      u.GetString("role"),
			GroupCode: groupCodeByID[u.GetString("group")],
			Active:    u.GetBool("active"),
		}
		data, err := catalog.MarshalUser(payload)
		if err != nil {
			return nil, fmt.Errorf("marshal user %s: %w", code, err)
		}
		out[code] = data
	}
	return out, nil
}

// expectedGroupKeys walks the groups collection and resolves each to its KV
// key (the group's code) plus a payload. Org-wide bucket.
func expectedGroupKeys(app core.App) (map[string][]byte, error) {
	groups, err := app.FindRecordsByFilter("groups", "", "", 0, 0)
	if err != nil {
		return nil, fmt.Errorf("load groups: %w", err)
	}
	out := make(map[string][]byte, len(groups))
	for _, g := range groups {
		code := g.GetString("code")
		if code == "" {
			continue
		}
		payload := catalog.GroupPayload{
			Code:         code,
			Name:         g.GetString("name"),
			ContactEmail: g.GetString("contact_email"),
			ContactPhone: g.GetString("contact_phone"),
			Notes:        g.GetString("notes"),
			Active:       g.GetBool("active"),
		}
		data, err := catalog.MarshalGroup(payload)
		if err != nil {
			return nil, fmt.Errorf("marshal group %s: %w", code, err)
		}
		out[code] = data
	}
	return out, nil
}

// actualKVKeys enumerates the current keys in a KV bucket via ListKeys.
// Returns a set keyed by string for O(1) diff lookups.
func actualKVKeys(ctx context.Context, kv jetstream.KeyValue) (map[string]struct{}, error) {
	lister, err := kv.ListKeys(ctx)
	if err != nil {
		return nil, fmt.Errorf("list keys: %w", err)
	}
	defer lister.Stop()
	out := make(map[string]struct{})
	for k := range lister.Keys() {
		out[k] = struct{}{}
	}
	return out, nil
}

// diffKeys produces the (missing_in_kv, extra_in_kv) lists. Expected is the
// DB-derived authoritative set; actual is the KV snapshot. Both outputs are
// sorted so consecutive runs are stable. Always non-nil so JSON encoding
// emits [] instead of null — the SPA reads .length without a guard.
func diffKeys(expected map[string][]byte, actual map[string]struct{}) (missing, extra []string) {
	missing = []string{}
	extra = []string{}
	for k := range expected {
		if _, ok := actual[k]; !ok {
			missing = append(missing, k)
		}
	}
	for k := range actual {
		if _, ok := expected[k]; !ok {
			extra = append(extra, k)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	return missing, extra
}

// Integrity diffs the controller's catalog DB against the live KV buckets.
// Read-only — does not modify KV.
//
// The three buckets are diffed in parallel because each kv.ListKeys() pays
// a fixed ephemeral-consumer setup cost (a few hundred ms even for empty
// buckets). Sequentially that adds up to a perceptible delay on the admin
// view; in parallel the wall-clock collapses to the slowest of the three.
// Each goroutine writes to a distinct field of `report`, so no
// synchronization beyond errgroup.Wait is needed.
func (p *CatalogPublisher) Integrity(ctx context.Context) (CatalogIntegrityReport, error) {
	var report CatalogIntegrityReport
	g, gctx := errgroup.WithContext(ctx)

	g.Go(func() error {
		expected, err := expectedItemKeys(p.app)
		if err != nil {
			return err
		}
		actual, err := actualKVKeys(gctx, p.items)
		if err != nil {
			return fmt.Errorf("enumerate items KV: %w", err)
		}
		missing, extra := diffKeys(expected, actual)
		report.Items = CatalogIntegrityBucket{
			Bucket:       p.items.Bucket(),
			ExpectedKeys: len(expected),
			ActualKeys:   len(actual),
			MissingInKV:  missing,
			ExtraInKV:    extra,
		}
		return nil
	})

	g.Go(func() error {
		expected, err := expectedUserKeys(p.app)
		if err != nil {
			return err
		}
		actual, err := actualKVKeys(gctx, p.users)
		if err != nil {
			return fmt.Errorf("enumerate users KV: %w", err)
		}
		missing, extra := diffKeys(expected, actual)
		report.Users = CatalogIntegrityBucket{
			Bucket:       p.users.Bucket(),
			ExpectedKeys: len(expected),
			ActualKeys:   len(actual),
			MissingInKV:  missing,
			ExtraInKV:    extra,
		}
		return nil
	})

	g.Go(func() error {
		expected, err := expectedGroupKeys(p.app)
		if err != nil {
			return err
		}
		actual, err := actualKVKeys(gctx, p.groups)
		if err != nil {
			return fmt.Errorf("enumerate groups KV: %w", err)
		}
		missing, extra := diffKeys(expected, actual)
		report.Groups = CatalogIntegrityBucket{
			Bucket:       p.groups.Bucket(),
			ExpectedKeys: len(expected),
			ActualKeys:   len(actual),
			MissingInKV:  missing,
			ExtraInKV:    extra,
		}
		return nil
	})

	if err := g.Wait(); err != nil {
		return report, err
	}
	return report, nil
}

// Reconcile pushes missing keys (DB-present, KV-absent) to KV. If
// deleteOrphans is true, also deletes KV keys that aren't backed by a DB
// record. Failures are collected per-key in the report — the operation
// continues past individual errors because each KV op is idempotent and a
// partial run is safe to retry.
//
// One-directional: the DB is always authoritative. Reverse direction (KV
// teaches the DB) is deliberately not supported — that would let an
// operator's `nats kv put` override the controller's source of truth.
//
// The three buckets are reconciled in parallel — same rationale as
// Integrity(): each bucket pays a fixed ListKeys setup cost, and the
// per-key Put/Delete loops within a bucket are independent across
// buckets. Per-key ops within a single bucket stay sequential (pipelining
// KV ops complicates ack handling without a clear payoff at catalog
// scale). Each goroutine writes to a distinct field of `report`.
func (p *CatalogPublisher) Reconcile(ctx context.Context, deleteOrphans bool) (CatalogReconcileReport, error) {
	var report CatalogReconcileReport
	g, gctx := errgroup.WithContext(ctx)

	g.Go(func() error {
		expected, err := expectedItemKeys(p.app)
		if err != nil {
			return err
		}
		actual, err := actualKVKeys(gctx, p.items)
		if err != nil {
			return fmt.Errorf("enumerate items KV: %w", err)
		}
		report.Items = p.reconcileBucket(gctx, p.items, expected, actual, deleteOrphans)
		return nil
	})

	g.Go(func() error {
		expected, err := expectedUserKeys(p.app)
		if err != nil {
			return err
		}
		actual, err := actualKVKeys(gctx, p.users)
		if err != nil {
			return fmt.Errorf("enumerate users KV: %w", err)
		}
		report.Users = p.reconcileBucket(gctx, p.users, expected, actual, deleteOrphans)
		return nil
	})

	g.Go(func() error {
		expected, err := expectedGroupKeys(p.app)
		if err != nil {
			return err
		}
		actual, err := actualKVKeys(gctx, p.groups)
		if err != nil {
			return fmt.Errorf("enumerate groups KV: %w", err)
		}
		report.Groups = p.reconcileBucket(gctx, p.groups, expected, actual, deleteOrphans)
		return nil
	})

	if err := g.Wait(); err != nil {
		return report, err
	}

	slog.Info("controller.catalog.reconcile_complete",
		"items_published", report.Items.Published,
		"items_deleted", report.Items.Deleted,
		"users_published", report.Users.Published,
		"users_deleted", report.Users.Deleted,
		"groups_published", report.Groups.Published,
		"groups_deleted", report.Groups.Deleted)

	return report, nil
}

// reconcileBucket does the per-bucket push + optional delete. Pulled out
// so the items and users halves of Reconcile share a single implementation.
func (p *CatalogPublisher) reconcileBucket(ctx context.Context, kv jetstream.KeyValue, expected map[string][]byte, actual map[string]struct{}, deleteOrphans bool) CatalogReconcileBucket {
	out := CatalogReconcileBucket{Bucket: kv.Bucket()}

	// Push: every expected key — not just the missing ones. This is the
	// "force-resync" semantic operators expect from a reconcile button:
	// after a rollback or config change, you want every value in KV to
	// reflect the DB right now, even if the key happened to already exist
	// with stale payload. KV history=1 means each Put is just an
	// overwrite; cost is one round-trip per key.
	missing, extra := diffKeys(expected, actual)
	_ = missing // missing is logically a subset of expected; we push all
	for key, data := range expected {
		if _, err := kv.Put(ctx, key, data); err != nil {
			out.PublishErrors = append(out.PublishErrors,
				fmt.Sprintf("%s: %v", key, err))
			continue
		}
		out.Published++
	}

	if deleteOrphans {
		for _, key := range extra {
			if err := kv.Delete(ctx, key); err != nil {
				out.DeleteErrors = append(out.DeleteErrors,
					fmt.Sprintf("%s: %v", key, err))
				continue
			}
			out.Deleted++
		}
	}

	return out
}
