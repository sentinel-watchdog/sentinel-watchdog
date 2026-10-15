package sdnotify

import (
	"context"
	"fmt"
	"net"
	"os"
	"testing"
)

// systemd may name an abstract socket ("@…"); the name has no file and no
// path length issue, and needs no privilege.
func TestNotifyAbstractSocket(t *testing.T) {
	name := fmt.Sprintf("@sentinel-sdnotify-test-%d", os.Getpid())
	conn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: name, Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	n, err := Parse(name)
	if err != nil {
		t.Fatal(err)
	}
	if err := n.Notify(context.Background(), Stopping); err != nil {
		t.Fatal(err)
	}
	if got := receive(t, conn); got != Stopping {
		t.Errorf("datagram %q, want %q", got, Stopping)
	}
}
