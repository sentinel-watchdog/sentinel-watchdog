package config

import (
	"io/fs"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
)

// fakeInfo is a fs.FileInfo with chosen mode and owner.
type fakeInfo struct {
	mode fs.FileMode
	sys  any
}

func (f fakeInfo) Name() string       { return "f" }
func (f fakeInfo) Size() int64        { return 0 }
func (f fakeInfo) Mode() fs.FileMode  { return f.mode }
func (f fakeInfo) ModTime() time.Time { return time.Time{} }
func (f fakeInfo) IsDir() bool        { return f.mode.IsDir() }
func (f fakeInfo) Sys() any           { return f.sys }

func TestCheckOwnership(t *testing.T) {
	euid := os.Geteuid()
	other := euid + 1
	if euid == 0 {
		other = 4242
	}
	tests := []struct {
		name string
		info fakeInfo
		want string // "" = allowed
	}{
		{"root owned", fakeInfo{0o644, &syscall.Stat_t{Uid: 0}}, ""},
		{"owned by the daemon user", fakeInfo{0o600, &syscall.Stat_t{Uid: uint32(euid)}}, ""},
		{"owned by another user", fakeInfo{0o644, &syscall.Stat_t{Uid: uint32(other)}}, "must be owned by root"},
		{"group writable", fakeInfo{0o664, &syscall.Stat_t{Uid: 0}}, "writable by group or others"},
		{"world writable directory", fakeInfo{fs.ModeDir | 0o757, &syscall.Stat_t{Uid: 0}}, "writable by group or others"},
		// Fail closed: a check that cannot run must not look like a pass.
		{"no owner information", fakeInfo{0o644, nil}, "no owner information"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkOwnership(tt.info, euid)
			switch {
			case tt.want == "" && err != nil:
				t.Errorf("unexpected error %v", err)
			case tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)):
				t.Errorf("err = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestCheckAncestor(t *testing.T) {
	euid := os.Geteuid()
	other := euid + 1
	if euid == 0 {
		other = 4242
	}
	tests := []struct {
		name string
		info fakeInfo
		want string
	}{
		{"root directory", fakeInfo{fs.ModeDir | 0o755, &syscall.Stat_t{Uid: 0}}, ""},
		{"sticky world-writable (like /tmp)", fakeInfo{fs.ModeDir | fs.ModeSticky | 0o777, &syscall.Stat_t{Uid: 0}}, ""},
		{"world-writable without sticky bit", fakeInfo{fs.ModeDir | 0o777, &syscall.Stat_t{Uid: 0}}, "without the sticky bit"},
		{"owned by another user", fakeInfo{fs.ModeDir | 0o755, &syscall.Stat_t{Uid: uint32(other)}}, "must be owned by root"},
		// A link's mode is always 0777; its parent decides who can replace it.
		{"symbolic link", fakeInfo{fs.ModeSymlink | 0o777, &syscall.Stat_t{Uid: 0}}, ""},
		{"symbolic link owned by another user", fakeInfo{fs.ModeSymlink | 0o777, &syscall.Stat_t{Uid: uint32(other)}}, "must be owned by root"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkAncestor(tt.info, euid)
			switch {
			case tt.want == "" && err != nil:
				t.Errorf("unexpected error %v", err)
			case tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)):
				t.Errorf("err = %v, want %q", err, tt.want)
			}
		})
	}
}
