package reconcile

import (
	"testing"
	"time"
)

func TestReconcile(t *testing.T) {
	now := time.Date(2026, 6, 27, 12, 0, 0, 0, time.UTC)
	cfg := Config{
		StaleAfter:   48 * time.Hour,
		CustodyZones: CustodyZoneSet([]string{"Main Crib", "Cabinet A"}),
	}

	cust := func(id, code, holder string) CustodyState {
		return CustodyState{KioskCode: "K", InstanceID: id, InstanceCode: code, Holder: holder}
	}
	loc := func(id, code, zone string, observedAt time.Time) LocationState {
		return LocationState{KioskCode: "K", InstanceID: id, InstanceCode: code, Zone: zone, ObservedAt: observedAt}
	}

	tests := []struct {
		name     string
		custody  []CustodyState
		location []LocationState
		wantKind map[string]string // instance_code -> kind (absent = no discrepancy)
	}{
		{
			name:     "not_taken: out but last seen in a custody zone",
			custody:  []CustodyState{cust("i1", "A-1", "Bob")},
			location: []LocationState{loc("i1", "A-1", "Cabinet A", now.Add(-1*time.Hour))},
			wantKind: map[string]string{"A-1": KindNotTaken},
		},
		{
			name:     "stale: out, last seen long ago, off a custody zone",
			custody:  []CustodyState{cust("i2", "A-2", "Bob")},
			location: []LocationState{loc("i2", "A-2", "Jobsite Gate", now.Add(-72*time.Hour))},
			wantKind: map[string]string{"A-2": KindStale},
		},
		{
			name:     "healthy: out, recently seen off-custody — no flag",
			custody:  []CustodyState{cust("i3", "A-3", "Bob")},
			location: []LocationState{loc("i3", "A-3", "Jobsite Gate", now.Add(-1*time.Hour))},
			wantKind: map[string]string{},
		},
		{
			name:     "unaccounted: seen off-custody but not in custody",
			custody:  nil,
			location: []LocationState{loc("i4", "A-4", "Yard Exit", now.Add(-10*time.Minute))},
			wantKind: map[string]string{"A-4": KindUnaccounted},
		},
		{
			name:     "idle in cabinet, not in custody — normal, no flag",
			custody:  nil,
			location: []LocationState{loc("i5", "A-5", "Cabinet A", now.Add(-10*time.Minute))},
			wantKind: map[string]string{},
		},
		{
			name:     "out but never observed — not flagged (no location data)",
			custody:  []CustodyState{cust("i6", "A-6", "Bob")},
			location: nil,
			wantKind: map[string]string{},
		},
		{
			name:     "not_taken wins over stale when both could apply",
			custody:  []CustodyState{cust("i7", "A-7", "Bob")},
			location: []LocationState{loc("i7", "A-7", "Main Crib", now.Add(-100*time.Hour))},
			wantKind: map[string]string{"A-7": KindNotTaken},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Reconcile(tc.custody, tc.location, cfg, now)
			gotKind := map[string]string{}
			for _, d := range got {
				gotKind[d.InstanceCode] = d.Kind
			}
			if len(gotKind) != len(tc.wantKind) {
				t.Fatalf("discrepancy count: got %v, want %v", gotKind, tc.wantKind)
			}
			for code, kind := range tc.wantKind {
				if gotKind[code] != kind {
					t.Errorf("instance %s: got kind %q, want %q", code, gotKind[code], kind)
				}
			}
		})
	}
}

// A cross-kiosk pair with the same instance id must not collide on the join key.
func TestReconcile_KeyIncludesKiosk(t *testing.T) {
	now := time.Date(2026, 6, 27, 12, 0, 0, 0, time.UTC)
	cfg := Config{CustodyZones: CustodyZoneSet([]string{"Crib"})}

	custody := []CustodyState{{KioskCode: "K1", InstanceID: "same", InstanceCode: "C1", Holder: "Bob"}}
	location := []LocationState{
		{KioskCode: "K1", InstanceID: "same", InstanceCode: "C1", Zone: "Crib", ObservedAt: now},
		{KioskCode: "K2", InstanceID: "same", InstanceCode: "C2", Zone: "Yard Exit", ObservedAt: now},
	}
	got := Reconcile(custody, location, cfg, now)
	// K1/same: not_taken (in custody, in crib). K2/same: unaccounted (not in
	// custody, off-zone). Same instance id, different kiosks — both surface.
	if len(got) != 2 {
		t.Fatalf("want 2 discrepancies across kiosks, got %d (%+v)", len(got), got)
	}
}
