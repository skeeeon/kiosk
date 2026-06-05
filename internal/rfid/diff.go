package rfid

// diff.go owns the enclosure_diff state reconciliation: given the
// kiosk's notion of what's "expected present" (active serialized
// instances NOT currently in open_checkouts) plus what the reader
// actually observed, derive the cart line changes that follow. See
// docs/rfid.md, Phase 4.
//
// The diff is intentionally a pure function over slices — no I/O, no
// kiosk state. The handler layer above gathers inputs from PB and
// the LLRP reader, calls Diff, then synthesizes cart lines from the
// outputs. Keeping policy out of here (self-return vs cross-user,
// inactive instance handling, etc.) makes the four-cell state space
// trivially testable.

// InstanceState is one row of the kiosk's "active serialized
// inventory" snapshot, plus its current open_checkouts status. The
// caller assembles these from item_instances + open_checkouts joins
// before calling Diff. EPC is the lower-case hex string the reader
// emits and matches against `item_instances.rfid_epc`.
type InstanceState struct {
	InstanceID   string
	ItemID       string
	EPC          EPC
	IsCheckedOut bool
	// CheckoutUserID is the user holding this instance when
	// IsCheckedOut is true. Empty when IsCheckedOut is false. The
	// caller uses this to decide self-return vs cross-user-return
	// synthesis; the diff itself just carries it through.
	CheckoutUserID string
	// Eligible is true when the instance is checkout-eligible (status
	// in_service). A maintenance unit is still expected-present (it's
	// physically in the enclosure) but NOT eligible: if it leaves, the
	// diff records it as skipped rather than synthesizing a checkout that
	// commit would reject.
	Eligible bool
}

// DiffEntry identifies one instance affected by the diff. The handler
// turns this into a cart.Line — DiffEntry stays minimal so this
// package doesn't pull in the cart types and the function stays
// testable without a PB app.
type DiffEntry struct {
	InstanceID string
	ItemID     string
	EPC        EPC
	// CheckoutUserID is populated only on returns (and only when the
	// instance had an open_checkouts holder). Empty on checkouts and
	// no-ops since they don't have a prior-state user to reference.
	CheckoutUserID string
}

// DiffResult is what Diff returns. Slices are non-nil-but-empty when
// there's nothing in them — easier to range over in the handler than
// nil-vs-empty checks.
type DiffResult struct {
	// Checkouts: instances that were expected-present but the reader
	// didn't see them — they left the enclosure. Synthesized as
	// action=checkout for the cart user.
	Checkouts []DiffEntry
	// Returns: instances that were checked out (per open_checkouts)
	// and the reader observed them back — they returned to the
	// enclosure. Synthesized as action=return, scoped to the cart
	// user when CheckoutUserID matches; the handler decides what to
	// do with cross-user returns (current scope: skip + log).
	Returns []DiffEntry
	// Unresolved: observed EPCs that don't match any active
	// serialized instance on this kiosk. Could be a stray read from
	// a neighboring enclosure, a decommissioned tag still in
	// circulation, or junk. Surfaced so the handler can publish them
	// on event.scan.rfid.observed for audit, but they never affect
	// the cart.
	Unresolved []EPC
	// SkippedIneligible: instances that left the enclosure but aren't
	// checkout-eligible (a maintenance unit). Recorded instead of
	// synthesized as a checkout (commit would reject it). The handler
	// surfaces the count to the operator.
	SkippedIneligible []DiffEntry
}

// Diff reconciles observed EPCs against expected-present state for a
// single read window. The two-axis state space:
//
//	                            observed=true       observed=false
//	IsCheckedOut=false         no-op (still in)    checkout (left)
//	IsCheckedOut=true          return (came back)  no-op (still out)
//
// Plus: observed EPC with no matching InstanceState → unresolved.
//
// The function builds an EPC→InstanceState map for O(observed)
// resolution, then walks `expected` once to classify each instance.
// O(n+m) overall.
func Diff(observed []EPC, expected []InstanceState) DiffResult {
	res := DiffResult{
		Checkouts:         []DiffEntry{},
		Returns:           []DiffEntry{},
		Unresolved:        []EPC{},
		SkippedIneligible: []DiffEntry{},
	}

	// Set lookups for the two axes. observedSet for "was this EPC seen";
	// expectedByEPC for "does this EPC resolve to an active instance".
	observedSet := make(map[EPC]struct{}, len(observed))
	for _, e := range observed {
		observedSet[e] = struct{}{}
	}
	expectedByEPC := make(map[EPC]*InstanceState, len(expected))
	for i := range expected {
		// Skip instances with no EPC set — they can't participate in
		// RFID-driven diff regardless of state. Such rows exist for
		// serialized instances that haven't been tagged yet.
		if expected[i].EPC == "" {
			continue
		}
		expectedByEPC[expected[i].EPC] = &expected[i]
	}

	// Walk expected once. Each instance falls into exactly one of the
	// four state-space cells.
	for i := range expected {
		inst := &expected[i]
		if inst.EPC == "" {
			// Untagged instances never appear in the reader's view; we
			// can't infer anything about them from RFID alone, so they
			// stay no-op. (A serialized item with both tagged and
			// untagged instances is the operator's problem to fix.)
			continue
		}
		_, wasObserved := observedSet[inst.EPC]
		switch {
		case !inst.IsCheckedOut && !wasObserved:
			// Expected present, not seen → it left the enclosure. Only
			// checkout-eligible (in_service) units become a checkout; a
			// maintenance unit that left is recorded as skipped, since
			// commit would reject a checkout of a non-in_service unit.
			entry := DiffEntry{
				InstanceID: inst.InstanceID,
				ItemID:     inst.ItemID,
				EPC:        inst.EPC,
			}
			if inst.Eligible {
				res.Checkouts = append(res.Checkouts, entry)
			} else {
				res.SkippedIneligible = append(res.SkippedIneligible, entry)
			}
		case inst.IsCheckedOut && wasObserved:
			// Was checked out, came back.
			res.Returns = append(res.Returns, DiffEntry{
				InstanceID:     inst.InstanceID,
				ItemID:         inst.ItemID,
				EPC:            inst.EPC,
				CheckoutUserID: inst.CheckoutUserID,
			})
		default:
			// No-op cells: (expected present + observed) and
			// (checked out + not observed). Nothing to add.
		}
	}

	// Walk observed once for unresolved tags. An EPC the reader saw
	// that doesn't resolve to any active serialized instance is a
	// stray — log-and-drop responsibility lives in the caller, but we
	// surface them here so the audit trail captures everything the
	// reader actually returned.
	for _, e := range observed {
		if _, ok := expectedByEPC[e]; !ok {
			res.Unresolved = append(res.Unresolved, e)
		}
	}

	return res
}
