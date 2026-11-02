// Package systemd observes and restarts systemd units over D-Bus (D-093,
// D-094, D-095). It never creates, edits, enables or disables a unit
// (PLAN.md §2): it reads a unit's state and asks systemd to restart it,
// nothing else.
//
// It talks to systemd over systemd's private socket, which only root may
// connect to, so its one peer is systemd: no other local user can send it
// anything. What systemd sends is still checked: every property has a
// fixed type, a bounded length and a closed vocabulary before it leaves
// this package.
package systemd

import (
	"errors"
	"fmt"
)

// ErrUnavailable means that systemd is not the init system here, or that
// its bus cannot be reached. A caller treats it as "nothing known", never
// as a failed unit (R-007).
var ErrUnavailable = errors.New("systemd is not available")

// ErrChanging means that the unit changed while it was being read (a
// restart between two property reads, or the unit unloaded), so no
// consistent state could be read. The next read decides.
var ErrChanging = errors.New("the unit changed while it was read")

// ErrInvalid means that systemd answered something this package does not
// accept: a value of another type, too long, or outside the vocabulary it
// knows (a newer systemd may add a state). Nothing about the unit can be
// concluded from it, least of all that it failed.
var ErrInvalid = errors.New("systemd's reply could not be read")

// LoadState is a unit's LoadState.
type LoadState string

// Load states (systemd's unit_load_state_table).
const (
	LoadLoaded     LoadState = "loaded"
	LoadNotFound   LoadState = "not-found"
	LoadBadSetting LoadState = "bad-setting"
	LoadError      LoadState = "error"
	LoadMasked     LoadState = "masked"
	LoadStub       LoadState = "stub"
	LoadMerged     LoadState = "merged"
)

// ActiveState is a unit's ActiveState.
type ActiveState string

// Active states (systemd's unit_active_state_table; maintenance and
// refreshing exist in newer versions).
const (
	ActiveActive       ActiveState = "active"
	ActiveReloading    ActiveState = "reloading"
	ActiveInactive     ActiveState = "inactive"
	ActiveFailed       ActiveState = "failed"
	ActiveActivating   ActiveState = "activating"
	ActiveDeactivating ActiveState = "deactivating"
	ActiveMaintenance  ActiveState = "maintenance"
	ActiveRefreshing   ActiveState = "refreshing"
)

var (
	loadStates = map[LoadState]bool{LoadLoaded: true, LoadNotFound: true, LoadBadSetting: true,
		LoadError: true, LoadMasked: true, LoadStub: true, LoadMerged: true}
	activeStates = map[ActiveState]bool{ActiveActive: true, ActiveReloading: true, ActiveInactive: true,
		ActiveFailed: true, ActiveActivating: true, ActiveDeactivating: true, ActiveMaintenance: true,
		ActiveRefreshing: true}
)

// UnitState is one consistent reading of a unit.
type UnitState struct {
	// ID is the unit's canonical name: another name than the one asked
	// for when that one is an alias.
	ID     string
	Load   LoadState
	Active ActiveState
	Sub    string // SubState: at most 32 bytes of [a-z0-9-]
	// Job is the id of the job queued or running for the unit; 0 when
	// there is none.
	Job uint32
	// InvocationID changes at every start (32 hex digits); empty for a
	// unit never started since boot.
	InvocationID string
	// NRestarts counts systemd's own automatic restarts (Restart=); a
	// manual start resets it. Zero for a unit that is not loaded.
	NRestarts uint32
	// Result is the service's last result (success, exit-code, …), as Sub.
	Result string
}

// ErrUnknownOutcome means that a restart was asked and its outcome is not
// known: the call may have reached systemd and no usable reply came back,
// so a job may be queued. The caller settles it with a fresh read, never
// by asking again (D-093 (3)).
var ErrUnknownOutcome = errors.New("the outcome of the request is not known")

// Codes of RefusedError: why systemd refused a restart.
const (
	RefusedConflict      = "conflict"       // another job is queued that the restart would replace
	RefusedNotFound      = "not_found"      // no such unit
	RefusedMasked        = "masked"         // the unit is masked
	RefusedLoadFailed    = "load_failed"    // the unit's file has errors
	RefusedManual        = "manual_refused" // RefuseManualStart or RefuseManualStop
	RefusedNotApplicable = "not_applicable" // the unit cannot be restarted
	RefusedDenied        = "denied"         // systemd denied the caller
	RefusedOther         = "refused"        // any other error systemd answered
)

// RefusedError is systemd's refusal of a restart. systemd answers a
// request with an error only before it queues a job, so nothing was
// queued. Code is one of the Refused* codes, never systemd's text.
type RefusedError struct{ Code string }

func (e *RefusedError) Error() string { return "systemd refused the restart: " + e.Code }

// refusals maps the error names systemd answers a restart with to their
// code (systemd's bus-common-errors.h, v239 and later).
var refusals = map[string]string{
	"org.freedesktop.systemd1.TransactionIsDestructive":   RefusedConflict,
	"org.freedesktop.systemd1.TransactionJobsConflicting": RefusedConflict,
	"org.freedesktop.systemd1.NoSuchUnit":                 RefusedNotFound,
	"org.freedesktop.systemd1.UnitMasked":                 RefusedMasked,
	"org.freedesktop.systemd1.LoadFailed":                 RefusedLoadFailed,
	"org.freedesktop.systemd1.BadUnitSetting":             RefusedLoadFailed,
	"org.freedesktop.systemd1.OnlyByDependency":           RefusedManual,
	"org.freedesktop.systemd1.JobTypeNotApplicable":       RefusedNotApplicable,
	"org.freedesktop.DBus.Error.AccessDenied":             RefusedDenied,
}

// propertyError names the property that could not be decoded, never its
// value.
func propertyError(name, why string) error { return fmt.Errorf("property %s: %s", name, why) }
