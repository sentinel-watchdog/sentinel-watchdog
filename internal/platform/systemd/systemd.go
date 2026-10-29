// Package systemd observes systemd units over D-Bus (D-093, D-094). It
// never creates, edits, enables or disables a unit (PLAN.md §2): in Phase
// 3c-1 it only reads a unit's state.
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
	// Job: a job is queued or running for the unit.
	Job bool
	// InvocationID changes at every start (32 hex digits); empty for a
	// unit never started since boot.
	InvocationID string
	// NRestarts counts systemd's own automatic restarts (Restart=); a
	// manual start resets it. Zero for a unit that is not loaded.
	NRestarts uint32
	// Result is the service's last result (success, exit-code, …), as Sub.
	Result string
}

// propertyError names the property that could not be decoded, never its
// value.
func propertyError(name, why string) error { return fmt.Errorf("property %s: %s", name, why) }
