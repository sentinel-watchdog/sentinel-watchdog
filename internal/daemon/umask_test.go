package daemon

import (
	"syscall"
	"testing"
)

// TestMain runs the tests with umask 022. They write files under
// t.TempDir(), whose directories get mode 0777 &^ umask, and the code under
// test rightly refuses group-writable directories: with the umask 002 of
// many Linux desktops these tests would fail (docs/development.md, "Test levels").
func TestMain(m *testing.M) {
	syscall.Umask(0o022)
	m.Run()
}
