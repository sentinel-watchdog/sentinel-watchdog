// Package authz decides what a peer of the control socket may do (ADR-0012):
// its tier comes from the credentials the kernel recorded when it connected,
// never from anything the client sends.
package authz

import (
	"fmt"
	"os/user"
	"slices"
	"strconv"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/config"
	"github.com/sentinel-watchdog/sentinel-watchdog/pkg/api"
)

// Peer is the identity of a connected client, as the kernel reports it.
type Peer struct {
	UID, GID uint32
	// Groups are the supplementary groups of the peer when it connected.
	Groups []uint32
}

// Policy maps peers to tiers. The zero Policy grants admin to root and
// read to everyone else who could connect.
type Policy struct {
	OperatorGIDs, AdminGIDs []uint32
}

// LookupGroup resolves a group name to its gid. Tests replace it; the
// default is LookupGroupSystem.
type LookupGroup func(name string) (uint32, error)

// LookupGroupSystem resolves a group with os/user. A static build (no cgo)
// reads /etc/group only: a group known only to LDAP or SSSD is not found.
func LookupGroupSystem(name string) (uint32, error) {
	g, err := user.LookupGroup(name)
	if err != nil {
		return 0, err
	}
	gid, err := strconv.ParseUint(g.Gid, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("group %q has a non-numeric gid %q", name, g.Gid)
	}
	return uint32(gid), nil
}

// NewPolicy resolves the groups of daemon.access. A group that does not
// exist is an error: sentineld must not start with a delegation it cannot
// apply.
func NewPolicy(access config.Access, lookup LookupGroup) (Policy, error) {
	var p Policy
	for _, g := range []struct {
		key, name string
		gids      *[]uint32
	}{
		{"daemon.access.operator_group", access.OperatorGroup, &p.OperatorGIDs},
		{"daemon.access.admin_group", access.AdminGroup, &p.AdminGIDs},
	} {
		if g.name == "" {
			continue
		}
		gid, err := lookup(g.name)
		if err != nil {
			return Policy{}, fmt.Errorf("%s: group %q: %w", g.key, g.name, err)
		}
		*g.gids = append(*g.gids, gid)
	}
	return p, nil
}

// Tier returns the peer's tier: admin for root and members of the admin
// group, operate for members of the operator group, read otherwise
// (connecting already required the socket's mode and group).
func (p Policy) Tier(peer Peer) api.Tier {
	switch {
	case peer.UID == 0 || p.member(peer, p.AdminGIDs):
		return api.TierAdmin
	case p.member(peer, p.OperatorGIDs):
		return api.TierOperate
	default:
		return api.TierRead
	}
}

// Authorize returns a permission_denied error when the peer's tier does not
// allow cmd, including when cmd declares no valid tier.
func (p Policy) Authorize(peer Peer, cmd api.Command) *api.Error {
	if have := p.Tier(peer); !have.Allows(cmd.Tier) {
		return &api.Error{Code: api.CodePermissionDenied,
			Message: fmt.Sprintf("%s needs the %s tier; this user has %s", cmd.Name, cmd.Tier, have)}
	}
	return nil
}

func (p Policy) member(peer Peer, gids []uint32) bool {
	for _, gid := range gids {
		if peer.GID == gid || slices.Contains(peer.Groups, gid) {
			return true
		}
	}
	return false
}
