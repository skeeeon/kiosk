package sightings

import "time"

// LocationRow is one serialized unit's latest advisory location, optionally
// annotated with who currently holds it (custody). It is the wire shape of the
// location report (docs/location-sightings-plan.md, L4) — the inverse of the
// reconciliation report: it lists *everything that has been seen*, not just the
// custody-vs-location discrepancies.
//
// Shared by both binaries' location endpoints (handlers.Locations reads local
// item_instances; controller.Handlers.Locations reads the fleet
// instance_location view) so a single SPA view renders both — the same
// single-source-of-truth stance as reconcile.Discrepancy. KioskCode is always
// set (a node fills its own); the SPA shows the column only on the controller.
type LocationRow struct {
	KioskCode    string    `json:"kiosk_code"`
	InstanceCode string    `json:"instance_code"`
	ItemCode     string    `json:"item_code,omitempty"`
	ItemName     string    `json:"item_name,omitempty"`
	Zone         string    `json:"zone,omitempty"`
	Gateway      string    `json:"gateway,omitempty"`
	Lat          float64   `json:"lat,omitempty"`
	Lon          float64   `json:"lon,omitempty"`
	ObservedAt   time.Time `json:"observed_at"`
	// Holder is the worker currently holding the unit (custody), empty when the
	// unit isn't checked out. Location and custody are orthogonal; this column
	// is the join the operator cares about ("seen in the yard, held by Bob").
	Holder string `json:"holder,omitempty"`
	// Status is the unit's lifecycle status (in_service / maintenance /
	// retired). Populated node-side (local item_instances); empty on the
	// controller view, whose instance_location projection doesn't carry it.
	Status string `json:"status,omitempty"`
}
