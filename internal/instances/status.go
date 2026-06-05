package instances

import "github.com/skeeeon/kiosk/internal/instances/status"

// The lifecycle status vocabulary + the in-transaction status writer live in
// the leaf sub-package internal/instances/status (so internal/commit can drive
// a transition inside its own transaction without importing this package and
// closing an import cycle — see that package's doc comment). These aliases let
// the rest of the instances package, and its tests, keep referring to the
// unqualified names (StatusInService, SetStatusInTx, writeAudit, …) unchanged.

const (
	StatusInService   = status.StatusInService
	StatusMaintenance = status.StatusMaintenance
	StatusRetired     = status.StatusRetired

	ActionCreate          = status.ActionCreate
	ActionToMaintenance   = status.ActionToMaintenance
	ActionReturnToService = status.ActionReturnToService
	ActionRetire          = status.ActionRetire
	ActionUnretire        = status.ActionUnretire
)

type (
	// SetStatusInput is re-exported because external callers (the command
	// handlers, via PerformSetStatus) construct it by this name.
	SetStatusInput = status.SetStatusInput
	// auditWriteInput stays unexported here — only this package's mutation
	// functions build audit rows directly.
	auditWriteInput = status.AuditWriteInput
)

var (
	// SetStatusInTx is the in-transaction status writer; exported so the
	// command/HTTP paths inside this package and external integration tests
	// can reach it under the instances.* name.
	SetStatusInTx = status.SetStatusInTx

	actionForTransition = status.ActionForTransition
	writeAudit          = status.WriteAudit
)
