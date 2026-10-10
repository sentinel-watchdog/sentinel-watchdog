package peercred

import (
	"net"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// The peer of a connection made by this process is this process: its uid,
// gid and supplementary groups.
func TestReadReportsTheConnectingProcess(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.sock")
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	client, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	server, err := l.AcceptUnix()
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	peer, err := Read(server)
	if err != nil {
		t.Fatal(err)
	}
	if int(peer.UID) != os.Getuid() || int(peer.GID) != os.Getgid() {
		t.Errorf("peer %+v, process uid %d gid %d", peer, os.Getuid(), os.Getgid())
	}
	want, err := os.Getgroups()
	if err != nil {
		t.Fatal(err)
	}
	got := make([]int, len(peer.Groups))
	for i, g := range peer.Groups {
		got[i] = int(g)
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("groups %v, process groups %v", got, want)
	}
}
