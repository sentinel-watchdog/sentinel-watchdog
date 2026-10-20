package privilege

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

// fixtures is a Lookup over a fixed account table; it fails the test if a
// numeric id is ever looked up (R-004: ids work without NSS).
func fixtures(t *testing.T) Lookup {
	return Lookup{
		User: func(name string) (uint32, uint32, error) {
			switch name {
			case "worker":
				return 1001, 1002, nil
			case "1001":
				t.Errorf("numeric user %q looked up", name)
			}
			return 0, 0, ErrUnknownUser
		},
		Group: func(name string) (uint32, error) {
			switch name {
			case "jobs":
				return 2000, nil
			case "2000":
				t.Errorf("numeric group %q looked up", name)
			}
			return 0, ErrUnknownGroup
		},
	}
}

func TestResolve(t *testing.T) {
	tests := []struct {
		name, user, group string
		want              Cred
		err               error
	}{
		{"named user, primary group", "worker", "", Cred{UID: 1001, GID: 1002}, nil},
		{"named user and group", "worker", "jobs", Cred{UID: 1001, GID: 2000}, nil},
		{"numeric user and group", "1001", "2000", Cred{UID: 1001, GID: 2000}, nil},
		{"named user, numeric group", "worker", "0", Cred{UID: 1001, GID: 0}, nil},
		{"group only keeps the uid", "", "jobs", Cred{UID: 500, GID: 2000}, nil},
		{"numeric user without group", "1001", "", Cred{}, ErrNeedGroup},
		{"unknown user", "nobody-here", "", Cred{}, ErrUnknownUser},
		{"unknown group", "worker", "nogroup-here", Cred{}, ErrUnknownGroup},
		{"id too large", "4294967296", "1", Cred{}, ErrBadID},
		{"reserved id -1", "4294967295", "1", Cred{}, ErrBadID},
		{"nothing", "", "", Cred{}, ErrNoIdentity},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := fixtures(t).Resolve(tt.user, tt.group, 500)
			if !errors.Is(err, tt.err) || !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Resolve = %+v, %v; want %+v, %v", got, err, tt.want, tt.err)
			}
		})
	}
}

// The errors never quote a name: it may come from the environment.
func TestResolveErrorsQuoteNothing(t *testing.T) {
	for _, spec := range [][2]string{{"zq-secret", ""}, {"worker", "zq-secret"}} {
		if _, err := fixtures(t).Resolve(spec[0], spec[1], 0); err == nil || strings.Contains(err.Error(), "zq-secret") {
			t.Errorf("%v: %v", spec, err)
		}
	}
}

func TestParseStatus(t *testing.T) {
	tests := []struct {
		name, status string
		want         Caps
		err          error
	}{
		{"root", "Name:\tsentineld\nCapInh:\t0000000000000000\nCapEff:\t000001ffffffffff\n", 0x000001ffffffffff, nil},
		{"none", "CapEff:\t0000000000000000\n", 0, nil},
		{"kill only", "CapEff:\t0000000000000020\n", 1 << CapKill, nil},
		{"missing", "Name:\tx\n", 0, ErrNoCapEff},
		{"garbage", "CapEff:\tzz\n", 0, ErrNoCapEff},
		{"too long", "CapEff:\t1ffffffffffffffff\n", 0, ErrNoCapEff},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseStatus(strings.NewReader(tt.status))
			if got != tt.want || !errors.Is(err, tt.err) {
				t.Errorf("ParseStatus = %#x, %v; want %#x, %v", got, err, tt.want, tt.err)
			}
		})
	}
	root, _ := ParseStatus(strings.NewReader("CapEff:\t000001ffffffffff\n"))
	if !root.Has(CapSetUID) || !root.Has(CapSetGID) || !root.Has(CapKill) || Caps(0).Has(CapKill) || root.Has(64) {
		t.Error("Has")
	}
}

// FuzzParseStatus: /proc content is read as untrusted input; the parser
// never panics and reads a bounded amount.
func FuzzParseStatus(f *testing.F) {
	f.Add("CapEff:\t000001ffffffffff\n")
	f.Add("CapEff:")
	f.Add(strings.Repeat("x", 100) + "\nCapEff: 1\n")
	f.Fuzz(func(t *testing.T, s string) {
		_, _ = ParseStatus(strings.NewReader(s))
	})
}

// FuzzResolve: ids and names from configuration never panic, and a
// numeric id is never looked up.
func FuzzResolve(f *testing.F) {
	for _, s := range []string{"0", "4294967295", "worker", "-1", "1e3", "٣"} {
		f.Add(s, s)
	}
	f.Fuzz(func(t *testing.T, u, g string) {
		_, _ = fixtures(t).Resolve(u, g, 0)
	})
}
