package rfid

import (
	"sort"
	"testing"
)

// TestDiff_StateSpace is the table-driven heart of the diff
// correctness story. For each cell of the two-axis state space we
// construct a single-instance "expected" plus an observed list that
// puts it in the right cell, and assert the result.
func TestDiff_StateSpace(t *testing.T) {
	const (
		instID = "inst-1"
		itemID = "item-1"
		epc    = EPC("aabbcc")
		holder = "user-bob"
	)

	cases := []struct {
		name           string
		expected       []InstanceState
		observed       []EPC
		wantCheckouts  int
		wantReturns    int
		wantUnresolved int
		assertion      func(t *testing.T, r DiffResult)
	}{
		{
			name: "expected present + observed → no-op",
			expected: []InstanceState{
				{InstanceID: instID, ItemID: itemID, EPC: epc, IsCheckedOut: false},
			},
			observed: []EPC{epc},
		},
		{
			name: "expected present + NOT observed → checkout",
			expected: []InstanceState{
				{InstanceID: instID, ItemID: itemID, EPC: epc, IsCheckedOut: false},
			},
			observed:      nil,
			wantCheckouts: 1,
			assertion: func(t *testing.T, r DiffResult) {
				if got := r.Checkouts[0].InstanceID; got != instID {
					t.Errorf("checkout InstanceID = %q, want %q", got, instID)
				}
				if got := r.Checkouts[0].CheckoutUserID; got != "" {
					t.Errorf("checkout should have no CheckoutUserID, got %q", got)
				}
			},
		},
		{
			name: "checked out + observed → return",
			expected: []InstanceState{
				{InstanceID: instID, ItemID: itemID, EPC: epc, IsCheckedOut: true, CheckoutUserID: holder},
			},
			observed:    []EPC{epc},
			wantReturns: 1,
			assertion: func(t *testing.T, r DiffResult) {
				if got := r.Returns[0].CheckoutUserID; got != holder {
					t.Errorf("return CheckoutUserID = %q, want %q", got, holder)
				}
			},
		},
		{
			name: "checked out + NOT observed → no-op (still out)",
			expected: []InstanceState{
				{InstanceID: instID, ItemID: itemID, EPC: epc, IsCheckedOut: true, CheckoutUserID: holder},
			},
			observed: nil,
		},
		{
			name:           "observed unknown EPC → unresolved",
			expected:       nil,
			observed:       []EPC{"deadbeef"},
			wantUnresolved: 1,
		},
		{
			name: "instance with empty EPC is ignored",
			expected: []InstanceState{
				{InstanceID: instID, ItemID: itemID, EPC: "", IsCheckedOut: false},
			},
			observed: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Diff(tc.observed, tc.expected)
			if len(got.Checkouts) != tc.wantCheckouts {
				t.Errorf("Checkouts: want %d, got %d (%v)", tc.wantCheckouts, len(got.Checkouts), got.Checkouts)
			}
			if len(got.Returns) != tc.wantReturns {
				t.Errorf("Returns: want %d, got %d (%v)", tc.wantReturns, len(got.Returns), got.Returns)
			}
			if len(got.Unresolved) != tc.wantUnresolved {
				t.Errorf("Unresolved: want %d, got %d (%v)", tc.wantUnresolved, len(got.Unresolved), got.Unresolved)
			}
			if tc.assertion != nil && (tc.wantCheckouts+tc.wantReturns+tc.wantUnresolved) > 0 {
				tc.assertion(t, got)
			}
		})
	}
}

// TestDiff_MixedBatch checks a realistic enclosure_diff scenario: a
// few of each cell in one read. This is the case the worker actually
// produces in production — they take some things out, put some
// things back, leave others alone.
func TestDiff_MixedBatch(t *testing.T) {
	expected := []InstanceState{
		// Stays in enclosure
		{InstanceID: "i-stay", ItemID: "drill", EPC: "stay-epc", IsCheckedOut: false},
		// Leaves the enclosure during this visit
		{InstanceID: "i-leave", ItemID: "drill", EPC: "leave-epc", IsCheckedOut: false},
		// Was out, returned this visit
		{InstanceID: "i-return", ItemID: "saw", EPC: "return-epc", IsCheckedOut: true, CheckoutUserID: "alice"},
		// Was out, stays out (not in the enclosure during this visit)
		{InstanceID: "i-still-out", ItemID: "saw", EPC: "still-out-epc", IsCheckedOut: true, CheckoutUserID: "bob"},
	}
	observed := []EPC{
		"stay-epc",
		"return-epc",
		"unknown-epc", // stray
	}

	got := Diff(observed, expected)

	if len(got.Checkouts) != 1 || got.Checkouts[0].InstanceID != "i-leave" {
		t.Errorf("Checkouts: want [i-leave], got %v", got.Checkouts)
	}
	if len(got.Returns) != 1 || got.Returns[0].InstanceID != "i-return" || got.Returns[0].CheckoutUserID != "alice" {
		t.Errorf("Returns: want [i-return by alice], got %v", got.Returns)
	}
	if len(got.Unresolved) != 1 || got.Unresolved[0] != "unknown-epc" {
		t.Errorf("Unresolved: want [unknown-epc], got %v", got.Unresolved)
	}
}

// TestDiff_NoActiveInventory: a kiosk with nothing serialized
// produces empty output, even when the reader sees tags. All tags
// land in Unresolved.
func TestDiff_NoActiveInventory(t *testing.T) {
	got := Diff([]EPC{"aa", "bb"}, nil)
	if len(got.Checkouts) != 0 || len(got.Returns) != 0 {
		t.Errorf("empty inventory should produce no checkouts/returns; got %v", got)
	}
	want := []EPC{"aa", "bb"}
	sort.Slice(got.Unresolved, func(i, j int) bool { return got.Unresolved[i] < got.Unresolved[j] })
	for i := range want {
		if got.Unresolved[i] != want[i] {
			t.Errorf("Unresolved[%d]: want %q, got %q", i, want[i], got.Unresolved[i])
		}
	}
}

// TestDiff_NothingObserved_AllExpected: zero reads in an enclosure
// full of expected-present items → everything checks out. This is the
// "operator took everything" edge case. Worth testing because the
// inner loop counts 0-observed correctly only if observedSet
// initialization is right.
func TestDiff_NothingObserved_AllExpected(t *testing.T) {
	expected := []InstanceState{
		{InstanceID: "a", EPC: "a-epc"},
		{InstanceID: "b", EPC: "b-epc"},
		{InstanceID: "c", EPC: "c-epc"},
	}
	got := Diff(nil, expected)
	if len(got.Checkouts) != 3 {
		t.Errorf("want 3 checkouts, got %d", len(got.Checkouts))
	}
	if len(got.Returns) != 0 || len(got.Unresolved) != 0 {
		t.Errorf("want zero returns/unresolved, got R=%d U=%d", len(got.Returns), len(got.Unresolved))
	}
}

// TestDiff_ResultSlicesAlwaysNonNil so callers can range over them
// without nil checks. Empty-slice vs nil-slice is a subtle Go gotcha;
// the diff's contract says the slices are always there.
func TestDiff_ResultSlicesAlwaysNonNil(t *testing.T) {
	got := Diff(nil, nil)
	if got.Checkouts == nil || got.Returns == nil || got.Unresolved == nil {
		t.Errorf("all result slices should be non-nil even when empty; got %+v", got)
	}
}

// TestDiff_DuplicateObservedDoesNotDuplicate: the reader can emit the
// same EPC many times in a window (dedupEPCs in reader.go already
// strips those, but the diff function should be robust if it gets a
// pre-dedup list anyway). Same instance shouldn't be classified
// twice.
func TestDiff_DuplicateObservedDoesNotDuplicate(t *testing.T) {
	expected := []InstanceState{
		{InstanceID: "a", EPC: "a-epc", IsCheckedOut: true, CheckoutUserID: "alice"},
	}
	got := Diff([]EPC{"a-epc", "a-epc", "a-epc"}, expected)
	if len(got.Returns) != 1 {
		t.Errorf("duplicate observation should produce 1 return, got %d", len(got.Returns))
	}
}
