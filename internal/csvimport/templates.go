package csvimport

import (
	"encoding/csv"
	"fmt"
	"io"
)

// Template writers emit a header row plus a single illustrative example
// row. The example values are intentionally generic ("WORKER-001",
// "GROUP-A") so the operator can use them as a starting point without
// having to wonder what each column wants. The header column shape MUST
// match what Run() recognizes — these are the contract.
//
// Kept separate from the importer functions so the template format and the
// importer format can be diffed in one place if either drifts.

// WriteItemsTemplate emits the items CSV header plus one example tool row
// and one example consumable row, with all optional columns populated so
// the operator sees the full menu.
func WriteItemsTemplate(w io.Writer) error {
	cw := csv.NewWriter(w)
	defer cw.Flush()

	if err := cw.Write([]string{
		"code", "name", "type", "unit", "tracking_mode",
		"category", "active", "notes",
		"quantity_on_hand", "reorder_threshold",
	}); err != nil {
		return err
	}
	rows := [][]string{
		{"HAMMER-16OZ", "16 oz Claw Hammer", "tool", "each", "quantity",
			"Hand Tools", "true", "Steel handle",
			"12", "3"},
		{"GLOVE-NITRILE-M", "Nitrile Gloves (Medium)", "consumable", "pair", "quantity",
			"PPE", "true", "Box of 100",
			"50", "10"},
	}
	for _, r := range rows {
		if err := cw.Write(r); err != nil {
			return err
		}
	}
	if err := cw.Error(); err != nil {
		return fmt.Errorf("flush items template: %w", err)
	}
	return nil
}

// WriteUsersTemplate emits the users CSV header plus one worker and one
// foreman example. `group` is a code, not an id — the importer resolves
// against the groups collection and auto-creates the row if missing.
func WriteUsersTemplate(w io.Writer) error {
	cw := csv.NewWriter(w)
	defer cw.Flush()

	if err := cw.Write([]string{
		"code", "name", "email", "phone", "role", "group", "active",
	}); err != nil {
		return err
	}
	rows := [][]string{
		{"WORKER-001", "Alex Worker", "alex@example.com", "+1-555-0101", "worker", "CREW-A", "true"},
		{"FOREMAN-001", "Sam Foreman", "sam@example.com", "+1-555-0102", "foreman", "CREW-A", "true"},
	}
	for _, r := range rows {
		if err := cw.Write(r); err != nil {
			return err
		}
	}
	if err := cw.Error(); err != nil {
		return fmt.Errorf("flush users template: %w", err)
	}
	return nil
}

// WriteGroupsTemplate emits the groups CSV header plus one example row
// with contact metadata filled in.
func WriteGroupsTemplate(w io.Writer) error {
	cw := csv.NewWriter(w)
	defer cw.Flush()

	if err := cw.Write([]string{
		"code", "name", "contact_email", "contact_phone", "notes", "active",
	}); err != nil {
		return err
	}
	rows := [][]string{
		{"CREW-A", "Crew A", "crew-a-lead@example.com", "+1-555-0100", "Morning shift", "true"},
	}
	for _, r := range rows {
		if err := cw.Write(r); err != nil {
			return err
		}
	}
	if err := cw.Error(); err != nil {
		return fmt.Errorf("flush groups template: %w", err)
	}
	return nil
}

// TemplateFor returns the template writer for a given kind. Returns nil
// for unknown kinds so callers can 404 cleanly.
func TemplateFor(kind Kind) func(io.Writer) error {
	switch kind {
	case KindItems:
		return WriteItemsTemplate
	case KindUsers:
		return WriteUsersTemplate
	case KindGroups:
		return WriteGroupsTemplate
	default:
		return nil
	}
}
