// Package privilege resolves the identity a supervised program runs as and
// reads the capabilities sentineld holds (D-084). It is a platform adapter:
// names are looked up in the system's account files, and capabilities come
// from /proc on Linux.
//
// sentineld is built with CGO_ENABLED=0 (D-023), so os/user reads
// /etc/passwd and /etc/group only, never NSS (SSSD, LDAP): numeric ids
// work everywhere and are never looked up (R-004).
package privilege

import (
	"errors"
	"math"
	"os/user"
	"strconv"
)

// Cred is the identity of a child process. Groups are its supplementary
// groups: empty unless configured, so a child never inherits sentineld's
// (threat model T-15).
type Cred struct {
	UID, GID uint32
	Groups   []uint32
}

// Errors of Resolve. Their text names no value: user and group names may
// come from the environment.
var (
	ErrUnknownUser  = errors.New("user not found in /etc/passwd")
	ErrUnknownGroup = errors.New("group not found in /etc/group")
	ErrNeedGroup    = errors.New("a numeric user needs a group: its primary group is not looked up")
	ErrBadID        = errors.New("not a valid user or group id")
	ErrNoIdentity   = errors.New("neither a user nor a group is set")
	errLookup       = errors.New("the account files could not be read")
)

// Lookup finds account entries. The variation it isolates is the system's
// account files versus the fixtures of the tests.
type Lookup struct {
	User  func(name string) (uid, gid uint32, err error)
	Group func(name string) (gid uint32, err error)
}

// System looks names up in the system's account files.
var System = Lookup{User: systemUser, Group: systemGroup}

// Resolve returns the identity for user and group, names or decimal ids.
//
//   - A named user gives its uid and primary gid; group overrides the gid.
//   - A numeric user needs a group (ErrNeedGroup): without NSS its primary
//     group cannot be found reliably.
//   - A group alone keeps uid; the caller passes sentineld's own.
func (l Lookup) Resolve(userSpec, groupSpec string, uid uint32) (Cred, error) {
	if userSpec == "" && groupSpec == "" {
		return Cred{}, ErrNoIdentity
	}
	cred := Cred{UID: uid}
	gidSet := false
	if userSpec != "" {
		if id, numeric, err := parseID(userSpec); err != nil {
			return Cred{}, err
		} else if numeric {
			cred.UID = id
		} else {
			u, g, err := l.User(userSpec)
			if err != nil {
				return Cred{}, err
			}
			cred.UID, cred.GID, gidSet = u, g, true
		}
	}
	if groupSpec != "" {
		if id, numeric, err := parseID(groupSpec); err != nil {
			return Cred{}, err
		} else if numeric {
			cred.GID = id
		} else {
			g, err := l.Group(groupSpec)
			if err != nil {
				return Cred{}, err
			}
			cred.GID = g
		}
		gidSet = true
	}
	if !gidSet {
		return Cred{}, ErrNeedGroup
	}
	return cred, nil
}

// parseID reports whether s is a decimal id and returns it. A string of
// digits that is not a valid id (too large, or the reserved -1 as 2^32-1)
// is an error, not a name.
func parseID(s string) (id uint32, numeric bool, err error) {
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, false, nil
		}
	}
	n, err := strconv.ParseUint(s, 10, 32)
	if err != nil || n == math.MaxUint32 {
		return 0, true, ErrBadID
	}
	return uint32(n), true, nil
}

func systemUser(name string) (uint32, uint32, error) {
	u, err := user.Lookup(name)
	if err != nil {
		var unknown user.UnknownUserError
		if errors.As(err, &unknown) {
			return 0, 0, ErrUnknownUser
		}
		return 0, 0, errLookup // the error may quote the name or a line of the file
	}
	uid, _, err := parseID(u.Uid)
	if err != nil {
		return 0, 0, ErrBadID
	}
	gid, _, err := parseID(u.Gid)
	if err != nil {
		return 0, 0, ErrBadID
	}
	return uid, gid, nil
}

func systemGroup(name string) (uint32, error) {
	g, err := user.LookupGroup(name)
	if err != nil {
		var unknown user.UnknownGroupError
		if errors.As(err, &unknown) {
			return 0, ErrUnknownGroup
		}
		return 0, errLookup
	}
	gid, _, err := parseID(g.Gid)
	if err != nil {
		return 0, ErrBadID
	}
	return gid, nil
}
