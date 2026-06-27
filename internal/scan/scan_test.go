package scan

import (
	"reflect"
	"testing"
)

// fakeLookups implements Lookups against fixed maps for table-driven tests.
type fakeLookups struct {
	usersByCode     map[string]*User
	itemsByCode     map[string]*Item
	instancesByCode map[string]*InstanceMatch
	instancesByRFID map[string]*InstanceMatch
	instancesByBLE  map[string]*InstanceMatch
	calls           []string // ordered list of lookup calls, for verifying dispatch
}

func (f *fakeLookups) UserByCode(c string) (*User, error) {
	f.calls = append(f.calls, "UserByCode:"+c)
	return f.usersByCode[c], nil
}
func (f *fakeLookups) ItemByCode(c string) (*Item, error) {
	f.calls = append(f.calls, "ItemByCode:"+c)
	return f.itemsByCode[c], nil
}
func (f *fakeLookups) ItemInstanceByCode(c string) (*InstanceMatch, error) {
	f.calls = append(f.calls, "ItemInstanceByCode:"+c)
	return f.instancesByCode[c], nil
}
func (f *fakeLookups) ItemInstanceByRFID(c string) (*InstanceMatch, error) {
	f.calls = append(f.calls, "ItemInstanceByRFID:"+c)
	return f.instancesByRFID[c], nil
}
func (f *fakeLookups) ItemInstanceByBLE(c string) (*InstanceMatch, error) {
	f.calls = append(f.calls, "ItemInstanceByBLE:"+c)
	return f.instancesByBLE[c], nil
}

func newFake() *fakeLookups {
	item1 := &Item{ID: "i1", Code: "DR-042", Name: "Impact Driver", Type: "tool", TrackingMode: "serialized"}
	return &fakeLookups{
		usersByCode: map[string]*User{
			"EMP-1": {ID: "u1", Code: "EMP-1", Name: "Alice", Role: "worker"},
		},
		itemsByCode: map[string]*Item{
			"DR-042": item1,
			"SCREW":  {ID: "i2", Code: "SCREW", Name: "Deck Screws", Type: "consumable", TrackingMode: "quantity"},
		},
		instancesByCode: map[string]*InstanceMatch{
			"DR-042-B": {
				Instance: &ItemInstance{ID: "inst-B", ItemID: "i1", Code: "DR-042-B", Serial: "SN-B", Status: "in_service"},
				Item:     item1,
			},
		},
		instancesByRFID: map[string]*InstanceMatch{
			"E280-INST-XYZ": {
				Instance: &ItemInstance{ID: "inst-B", ItemID: "i1", Code: "DR-042-B", RFIDEPC: "E280-INST-XYZ", Status: "in_service"},
				Item:     item1,
			},
		},
		instancesByBLE: map[string]*InstanceMatch{
			"BEACON-9": {
				Instance: &ItemInstance{ID: "inst-B", ItemID: "i1", Code: "DR-042-B", BLEID: "BEACON-9", Status: "in_service"},
				Item:     item1,
			},
		},
	}
}

// TestResolve_BLE: a BLE beacon id resolves to its instance, after the RFID
// step misses — the source-agnostic location/sightings path (L4).
func TestResolve_BLE(t *testing.T) {
	fake := newFake()
	r := &Resolver{
		Lookups: Lookups{
			UserByCode:         fake.UserByCode,
			ItemByCode:         fake.ItemByCode,
			ItemInstanceByCode: fake.ItemInstanceByCode,
			ItemInstanceByRFID: fake.ItemInstanceByRFID,
			ItemInstanceByBLE:  fake.ItemInstanceByBLE,
		},
	}
	got := r.Resolve("BEACON-9")
	if got.Type != ResultItemInstance {
		t.Fatalf("type: got %q want item_instance", got.Type)
	}
	m, ok := got.Record.(*InstanceMatch)
	if !ok || m.Instance == nil || m.Instance.BLEID != "BEACON-9" {
		t.Fatalf("expected instance with BLEID=BEACON-9, got %#v", got.Record)
	}
	// BLE is tried only after code/item/rfid all miss.
	want := []string{
		"ItemInstanceByCode:BEACON-9", "ItemByCode:BEACON-9",
		"ItemInstanceByRFID:BEACON-9", "ItemInstanceByBLE:BEACON-9",
	}
	if !reflect.DeepEqual(fake.calls, want) {
		t.Errorf("calls: got %v want %v", fake.calls, want)
	}
}

func TestResolve(t *testing.T) {
	tests := []struct {
		name       string
		userPrefix string
		itemPrefix string
		value      string
		wantType   ResultType
		wantRecord any
		wantValue  string
		wantCalls  []string
	}{
		{
			name:      "empty value is unknown",
			value:     "",
			wantType:  ResultUnknown,
			wantValue: "",
			wantCalls: nil,
		},
		{
			name:      "whitespace is trimmed then unknown",
			value:     "   ",
			wantType:  ResultUnknown,
			wantCalls: nil,
		},
		{
			name:       "user prefix routes to user lookup only",
			userPrefix: "U:",
			value:      "U:EMP-1",
			wantType:   ResultUser,
			wantRecord: &User{ID: "u1", Code: "EMP-1", Name: "Alice", Role: "worker"},
			wantCalls:  []string{"UserByCode:EMP-1"},
		},
		{
			name:       "user prefix with miss is unknown, does not fall through",
			userPrefix: "U:",
			value:      "U:NOPE",
			wantType:   ResultUnknown,
			wantValue:  "U:NOPE",
			wantCalls:  []string{"UserByCode:NOPE"},
		},
		{
			name:       "item prefix routes to item lookups only",
			itemPrefix: "I:",
			value:      "I:DR-042",
			wantType:   ResultItem,
			wantRecord: &Item{ID: "i1", Code: "DR-042", Name: "Impact Driver", Type: "tool", TrackingMode: "serialized"},
			wantCalls:  []string{"ItemInstanceByCode:DR-042", "ItemByCode:DR-042"},
		},
		{
			name:       "item prefix with no match is unknown, does not try users",
			itemPrefix: "I:",
			value:      "I:NOPE",
			wantType:   ResultUnknown,
			wantValue:  "I:NOPE",
			wantCalls: []string{
				"ItemInstanceByCode:NOPE", "ItemByCode:NOPE",
				"ItemInstanceByRFID:NOPE",
			},
		},
		{
			name:       "no prefix tries item code first",
			value:      "DR-042",
			wantType:   ResultItem,
			wantRecord: &Item{ID: "i1", Code: "DR-042", Name: "Impact Driver", Type: "tool", TrackingMode: "serialized"},
			wantCalls:  []string{"ItemInstanceByCode:DR-042", "ItemByCode:DR-042"},
		},
		{
			name:       "no prefix falls through to user code",
			value:      "EMP-1",
			wantType:   ResultUser,
			wantRecord: &User{ID: "u1", Code: "EMP-1", Name: "Alice", Role: "worker"},
			wantCalls: []string{
				"ItemInstanceByCode:EMP-1", "ItemByCode:EMP-1",
				"ItemInstanceByRFID:EMP-1",
				"UserByCode:EMP-1",
			},
		},
		{
			name:      "no prefix with no match is unknown",
			value:     "DOES-NOT-EXIST",
			wantType:  ResultUnknown,
			wantValue: "DOES-NOT-EXIST",
			wantCalls: []string{
				"ItemInstanceByCode:DOES-NOT-EXIST", "ItemByCode:DOES-NOT-EXIST",
				"ItemInstanceByRFID:DOES-NOT-EXIST",
				"UserByCode:DOES-NOT-EXIST",
			},
		},
		{
			name:       "value without prefix when prefix is configured falls through normally",
			userPrefix: "U:",
			value:      "EMP-1",
			wantType:   ResultUser,
			wantRecord: &User{ID: "u1", Code: "EMP-1", Name: "Alice", Role: "worker"},
			wantCalls: []string{
				"ItemInstanceByCode:EMP-1", "ItemByCode:EMP-1",
				"ItemInstanceByRFID:EMP-1",
				"UserByCode:EMP-1",
			},
		},
		{
			name:     "instance code wins, short-circuits before item lookup",
			value:    "DR-042-B",
			wantType: ResultItemInstance,
			wantRecord: &InstanceMatch{
				Instance: &ItemInstance{ID: "inst-B", ItemID: "i1", Code: "DR-042-B", Serial: "SN-B", Status: "in_service"},
				Item:     &Item{ID: "i1", Code: "DR-042", Name: "Impact Driver", Type: "tool", TrackingMode: "serialized"},
			},
			wantCalls: []string{"ItemInstanceByCode:DR-042-B"},
		},
		{
			name:     "instance rfid resolves when item code/rfid miss",
			value:    "E280-INST-XYZ",
			wantType: ResultItemInstance,
			wantRecord: &InstanceMatch{
				Instance: &ItemInstance{ID: "inst-B", ItemID: "i1", Code: "DR-042-B", RFIDEPC: "E280-INST-XYZ", Status: "in_service"},
				Item:     &Item{ID: "i1", Code: "DR-042", Name: "Impact Driver", Type: "tool", TrackingMode: "serialized"},
			},
			wantCalls: []string{
				"ItemInstanceByCode:E280-INST-XYZ", "ItemByCode:E280-INST-XYZ",
				"ItemInstanceByRFID:E280-INST-XYZ",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := newFake()
			r := &Resolver{
				UserPrefix: tt.userPrefix,
				ItemPrefix: tt.itemPrefix,
				Lookups: Lookups{
					UserByCode:         fake.UserByCode,
					ItemByCode:         fake.ItemByCode,
					ItemInstanceByCode: fake.ItemInstanceByCode,
					ItemInstanceByRFID: fake.ItemInstanceByRFID,
				},
			}
			got := r.Resolve(tt.value)
			if got.Type != tt.wantType {
				t.Errorf("type: got %q want %q", got.Type, tt.wantType)
			}
			if got.Value != tt.wantValue {
				t.Errorf("value: got %q want %q", got.Value, tt.wantValue)
			}
			if !reflect.DeepEqual(got.Record, tt.wantRecord) {
				t.Errorf("record: got %#v want %#v", got.Record, tt.wantRecord)
			}
			if !reflect.DeepEqual(fake.calls, tt.wantCalls) {
				t.Errorf("calls: got %v want %v", fake.calls, tt.wantCalls)
			}
		})
	}
}
