package systemd

import (
	"encoding/hex"

	"github.com/godbus/dbus/v5"
)

// maxToken bounds SubState and Result; systemd's longest is far shorter.
const maxToken = 32

// decodeState turns the property values read from systemd (each variant's
// Value) into a UnitState, checking every type, length and vocabulary.
// The Service properties (NRestarts, Result) may be missing for a unit
// that is not loaded.
func decodeState(p map[string]any) (UnitState, error) {
	var st UnitState
	var err error
	if st.ID, err = str(p, "Id"); err != nil {
		return UnitState{}, err
	}
	if _, perr := ParseUnitName(st.ID); perr != nil {
		return UnitState{}, propertyError("Id", "not a unit name")
	}
	load, err := str(p, "LoadState")
	if err != nil {
		return UnitState{}, err
	}
	if st.Load = LoadState(load); !loadStates[st.Load] {
		return UnitState{}, propertyError("LoadState", "unknown value")
	}
	active, err := str(p, "ActiveState")
	if err != nil {
		return UnitState{}, err
	}
	if st.Active = ActiveState(active); !activeStates[st.Active] {
		return UnitState{}, propertyError("ActiveState", "unknown value")
	}
	if st.Sub, err = str(p, "SubState"); err != nil {
		return UnitState{}, err
	}
	if !token(st.Sub) {
		return UnitState{}, propertyError("SubState", "not a short lower-case word")
	}
	if st.Job, err = job(p["Job"]); err != nil {
		return UnitState{}, err
	}
	if st.InvocationID, err = invocation(p["InvocationID"]); err != nil {
		return UnitState{}, err
	}
	if v, ok := p["NRestarts"]; ok {
		n, ok := v.(uint32)
		if !ok {
			return UnitState{}, propertyError("NRestarts", "not a uint32")
		}
		st.NRestarts = n
	}
	if _, ok := p["Result"]; ok {
		if st.Result, err = str(p, "Result"); err != nil {
			return UnitState{}, err
		}
		if !token(st.Result) {
			return UnitState{}, propertyError("Result", "not a short lower-case word")
		}
	}
	return st, nil
}

// str returns the string property name of p.
func str(p map[string]any, name string) (string, error) {
	v, ok := p[name]
	if !ok {
		return "", propertyError(name, "missing")
	}
	s, ok := v.(string)
	if !ok {
		return "", propertyError(name, "not a string")
	}
	return s, nil
}

// token reports whether s is a non-empty word of [a-z0-9-], at most
// maxToken bytes: the shape of systemd's state and result names.
func token(s string) bool {
	if s == "" || len(s) > maxToken {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
			return false
		}
	}
	return true
}

// job decodes the Job property, (uo): a job id and its path, id 0 when
// there is none.
func job(v any) (bool, error) {
	fields, ok := v.([]any)
	if !ok || len(fields) != 2 {
		return false, propertyError("Job", "not a (uo) struct")
	}
	id, ok := fields[0].(uint32)
	if !ok {
		return false, propertyError("Job", "not a (uo) struct")
	}
	if _, ok := fields[1].(dbus.ObjectPath); !ok {
		return false, propertyError("Job", "not a (uo) struct")
	}
	return id != 0, nil
}

// invocation decodes InvocationID, ay: 16 bytes, or none for a unit never
// started, as 32 hex digits.
func invocation(v any) (string, error) {
	b, ok := v.([]byte)
	switch {
	case !ok:
		return "", propertyError("InvocationID", "not a byte array")
	case len(b) == 0:
		return "", nil
	case len(b) != 16:
		return "", propertyError("InvocationID", "not 16 bytes")
	}
	return hex.EncodeToString(b), nil
}
