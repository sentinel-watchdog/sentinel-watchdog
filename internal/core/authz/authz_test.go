package authz

import (
	"errors"
	"strings"
	"testing"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/config"
	"github.com/sentinel-watchdog/sentinel-watchdog/pkg/api"
)

func groups(m map[string]uint32) LookupGroup {
	return func(name string) (uint32, error) {
		if gid, ok := m[name]; ok {
			return gid, nil
		}
		return 0, errors.New("unknown group")
	}
}

func TestTier(t *testing.T) {
	p, err := NewPolicy(config.Access{OperatorGroup: "ops", AdminGroup: "fw"}, groups(map[string]uint32{"ops": 100, "fw": 200}))
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		peer Peer
		want api.Tier
	}{
		{"root", Peer{UID: 0, GID: 0}, api.TierAdmin},
		{"admin group, primary", Peer{UID: 1000, GID: 200}, api.TierAdmin},
		{"admin group, supplementary", Peer{UID: 1000, GID: 1000, Groups: []uint32{5, 200}}, api.TierAdmin},
		{"admin and operator", Peer{UID: 1000, GID: 100, Groups: []uint32{200}}, api.TierAdmin},
		{"operator, primary", Peer{UID: 1000, GID: 100}, api.TierOperate},
		{"operator, supplementary", Peer{UID: 1000, GID: 1000, Groups: []uint32{100}}, api.TierOperate},
		{"anyone else", Peer{UID: 1000, GID: 1000, Groups: []uint32{27}}, api.TierRead},
	}
	for _, tt := range tests {
		if got := p.Tier(tt.peer); got != tt.want {
			t.Errorf("%s: tier %s, want %s", tt.name, got, tt.want)
		}
	}
}

// Empty groups mean root only: a user in gid 0 is not root.
func TestEmptyGroupsMeanRootOnly(t *testing.T) {
	var p Policy
	if got := p.Tier(Peer{UID: 1000, GID: 0, Groups: []uint32{0}}); got != api.TierRead {
		t.Errorf("tier %s", got)
	}
}

func TestAuthorize(t *testing.T) {
	p, _ := NewPolicy(config.Access{OperatorGroup: "ops"}, groups(map[string]uint32{"ops": 100}))
	operator := Peer{UID: 1000, GID: 100}
	if err := p.Authorize(operator, api.Command{Name: "core.reload", Tier: api.TierOperate}); err != nil {
		t.Errorf("operator denied: %v", err)
	}
	err := p.Authorize(operator, api.Command{Name: "firewall.apply", Tier: api.TierAdmin})
	if err == nil || err.Code != api.CodePermissionDenied || !strings.Contains(err.Message, "admin") {
		t.Errorf("err = %v", err)
	}
	if err := p.Authorize(Peer{UID: 0}, api.Command{Name: "core.x", Tier: ""}); err == nil {
		t.Error("a command without a valid tier was allowed")
	}
}

func TestNewPolicyRejectsUnknownGroups(t *testing.T) {
	_, err := NewPolicy(config.Access{AdminGroup: "missing"}, groups(nil))
	if err == nil || !strings.Contains(err.Error(), "daemon.access.admin_group") {
		t.Errorf("err = %v", err)
	}
}

func TestLookupGroupSystem(t *testing.T) {
	if _, err := LookupGroupSystem("sentinel-no-such-group-zz"); err == nil {
		t.Error("unknown group resolved")
	}
}

// Full review 2026-10-28 F2: docs/configuration.md documents a group as
// "account name or numeric id" (`1001`), and config validation accepts
// digits, but LookupGroupSystem resolves every value as a name.
func TestLookupGroupSystemNumericID(t *testing.T) {
	tests := []struct {
		in      string
		want    uint32
		wantErr bool
	}{
		{"0", 0, false},
		{"424242", 424242, false}, // an id with no /etc/group entry (LDAP, static build)
		{"4294967295", 0, true},   // (gid_t)-1 is reserved
		{"9999999999", 0, true},   // does not fit 32 bits
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := LookupGroupSystem(tt.in)
			if (err != nil) != tt.wantErr || got != tt.want {
				t.Errorf("LookupGroupSystem(%q) = %d, %v; want %d, error %v", tt.in, got, err, tt.want, tt.wantErr)
			}
		})
	}
}
