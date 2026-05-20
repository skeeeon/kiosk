package scan

import (
	"reflect"
	"testing"
)

// fakeLookups implements Lookups against fixed maps for table-driven tests.
type fakeLookups struct {
	usersByCode map[string]*User
	itemsByCode map[string]*Item
	itemsByRFID map[string]*Item
	calls       []string // ordered list of lookup calls, for verifying dispatch
}

func (f *fakeLookups) UserByCode(c string) (*User, error) {
	f.calls = append(f.calls, "UserByCode:"+c)
	return f.usersByCode[c], nil
}
func (f *fakeLookups) ItemByCode(c string) (*Item, error) {
	f.calls = append(f.calls, "ItemByCode:"+c)
	return f.itemsByCode[c], nil
}
func (f *fakeLookups) ItemByRFID(c string) (*Item, error) {
	f.calls = append(f.calls, "ItemByRFID:"+c)
	return f.itemsByRFID[c], nil
}

func newFake() *fakeLookups {
	return &fakeLookups{
		usersByCode: map[string]*User{
			"EMP-1": {ID: "u1", Code: "EMP-1", Name: "Alice", Role: "worker"},
		},
		itemsByCode: map[string]*Item{
			"DR-042": {ID: "i1", Code: "DR-042", Name: "Impact Driver", Type: "tool", TrackingMode: "serialized", Serial: "SN-1"},
			"SCREW":  {ID: "i2", Code: "SCREW", Name: "Deck Screws", Type: "consumable", TrackingMode: "quantity"},
		},
		itemsByRFID: map[string]*Item{
			"E280-RFID-XYZ": {ID: "i1", Code: "DR-042", Name: "Impact Driver", Type: "tool", TrackingMode: "serialized", Serial: "SN-1"},
		},
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
			name:       "empty value is unknown",
			value:      "",
			wantType:   ResultUnknown,
			wantValue:  "",
			wantCalls:  nil,
		},
		{
			name:       "whitespace is trimmed then unknown",
			value:      "   ",
			wantType:   ResultUnknown,
			wantCalls:  nil,
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
			wantRecord: &Item{ID: "i1", Code: "DR-042", Name: "Impact Driver", Type: "tool", TrackingMode: "serialized", Serial: "SN-1"},
			wantCalls:  []string{"ItemByCode:DR-042"},
		},
		{
			name:       "item prefix falls back to rfid",
			itemPrefix: "I:",
			value:      "I:E280-RFID-XYZ",
			wantType:   ResultItem,
			wantRecord: &Item{ID: "i1", Code: "DR-042", Name: "Impact Driver", Type: "tool", TrackingMode: "serialized", Serial: "SN-1"},
			wantCalls:  []string{"ItemByCode:E280-RFID-XYZ", "ItemByRFID:E280-RFID-XYZ"},
		},
		{
			name:       "item prefix with no match is unknown, does not try users",
			itemPrefix: "I:",
			value:      "I:NOPE",
			wantType:   ResultUnknown,
			wantValue:  "I:NOPE",
			wantCalls:  []string{"ItemByCode:NOPE", "ItemByRFID:NOPE"},
		},
		{
			name:       "no prefix tries item code first",
			value:      "DR-042",
			wantType:   ResultItem,
			wantRecord: &Item{ID: "i1", Code: "DR-042", Name: "Impact Driver", Type: "tool", TrackingMode: "serialized", Serial: "SN-1"},
			wantCalls:  []string{"ItemByCode:DR-042"},
		},
		{
			name:       "no prefix falls through to rfid",
			value:      "E280-RFID-XYZ",
			wantType:   ResultItem,
			wantRecord: &Item{ID: "i1", Code: "DR-042", Name: "Impact Driver", Type: "tool", TrackingMode: "serialized", Serial: "SN-1"},
			wantCalls:  []string{"ItemByCode:E280-RFID-XYZ", "ItemByRFID:E280-RFID-XYZ"},
		},
		{
			name:       "no prefix falls through to user code",
			value:      "EMP-1",
			wantType:   ResultUser,
			wantRecord: &User{ID: "u1", Code: "EMP-1", Name: "Alice", Role: "worker"},
			wantCalls:  []string{"ItemByCode:EMP-1", "ItemByRFID:EMP-1", "UserByCode:EMP-1"},
		},
		{
			name:       "no prefix with no match is unknown",
			value:      "DOES-NOT-EXIST",
			wantType:   ResultUnknown,
			wantValue:  "DOES-NOT-EXIST",
			wantCalls:  []string{"ItemByCode:DOES-NOT-EXIST", "ItemByRFID:DOES-NOT-EXIST", "UserByCode:DOES-NOT-EXIST"},
		},
		{
			name:       "value without prefix when prefix is configured falls through normally",
			userPrefix: "U:",
			value:      "EMP-1",
			wantType:   ResultUser,
			wantRecord: &User{ID: "u1", Code: "EMP-1", Name: "Alice", Role: "worker"},
			wantCalls:  []string{"ItemByCode:EMP-1", "ItemByRFID:EMP-1", "UserByCode:EMP-1"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := newFake()
			r := &Resolver{
				UserPrefix: tt.userPrefix,
				ItemPrefix: tt.itemPrefix,
				Lookups: Lookups{
					UserByCode: fake.UserByCode,
					ItemByCode: fake.ItemByCode,
					ItemByRFID: fake.ItemByRFID,
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
