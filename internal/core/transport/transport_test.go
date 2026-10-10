package transport

import (
	"context"
	"encoding/json/jsontext"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/authz"
	"github.com/sentinel-watchdog/sentinel-watchdog/pkg/api"
)

var (
	root    = authz.Peer{UID: 0}
	visitor = authz.Peer{UID: 1000, GID: 1000}
)

// socketPath returns a socket path in a fresh private directory, short
// enough for the Unix socket limit (t.TempDir paths can be too long on macOS).
func socketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "sw")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "run", "s.sock")
}

func echo(context.Context, jsontext.Value) (any, *api.Error) {
	return map[string]string{"ok": "yes"}, nil
}

func options(path string, peer authz.Peer, bindings ...Binding) Options {
	return Options{
		Path: path, Mode: 0o660, GID: -1,
		Peer:     func(*net.UnixConn) (authz.Peer, error) { return peer, nil },
		Bindings: append([]Binding{{Command: api.CoreStatus, Run: echo}}, bindings...),
		Logger:   slog.New(slog.DiscardHandler),
	}
}

func listenT(t *testing.T, opts Options) *Server {
	t.Helper()
	s, err := Listen(opts)
	if err != nil {
		t.Fatal(err)
	}
	s.Serve()
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	return s
}

func call(t *testing.T, path, command string) api.Response {
	t.Helper()
	resp, err := Call(context.Background(), path, api.Request{Version: api.Version, Command: command})
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestServeAndCall(t *testing.T) {
	path := socketPath(t)
	listenT(t, options(path, root))
	resp := call(t, path, "core.status")
	if resp.Error != nil || string(resp.Result) != `{"ok":"yes"}` {
		t.Errorf("response %+v", resp)
	}
}

func TestSocketModeGroupAndRemoval(t *testing.T) {
	path := socketPath(t)
	opts := options(path, root)
	opts.Mode, opts.GID = 0o640, os.Getgid()
	s := listenT(t, opts)
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&fs.ModeSocket == 0 || info.Mode().Perm() != 0o640 {
		t.Errorf("socket mode %s", info.Mode())
	}
	if st, ok := info.Sys().(*syscall.Stat_t); !ok || int(st.Gid) != os.Getgid() {
		t.Errorf("socket gid %v", info.Sys())
	}
	if dir, err := os.Stat(filepath.Dir(path)); err != nil || dir.Mode().Perm() != parentMode {
		t.Errorf("parent directory: %v %v", dir.Mode(), err)
	}
	if err := s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("socket left behind: %v", err)
	}
	if _, err := os.Lstat(path + ".lock"); err != nil {
		t.Errorf("lock file removed: %v", err)
	}
}

// A crashed daemon's socket is replaced; a live socket, a second daemon and
// anything that is not a socket are refused and left alone.
func TestListenExistingEntries(t *testing.T) {
	t.Run("stale socket", func(t *testing.T) {
		path := socketPath(t)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		l, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
		if err != nil {
			t.Fatal(err)
		}
		l.SetUnlinkOnClose(false)
		_ = l.Close() // the file stays, nobody listens: a crash
		listenT(t, options(path, root))
		if resp := call(t, path, "core.status"); resp.Error != nil {
			t.Errorf("response %+v", resp)
		}
	})
	t.Run("live socket of another process", func(t *testing.T) {
		path := socketPath(t)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		l, err := net.Listen("unix", path)
		if err != nil {
			t.Fatal(err)
		}
		defer l.Close()
		if _, err := Listen(options(path, root)); err == nil || !strings.Contains(err.Error(), "another process") {
			t.Errorf("err = %v", err)
		}
		if conn, err := net.Dial("unix", path); err != nil {
			t.Errorf("the live socket was harmed: %v", err)
		} else {
			_ = conn.Close()
		}
	})
	t.Run("second daemon", func(t *testing.T) {
		path := socketPath(t)
		listenT(t, options(path, root))
		if _, err := Listen(options(path, root)); err == nil || !strings.Contains(err.Error(), "in use by another sentineld") {
			t.Errorf("err = %v", err)
		}
		if resp := call(t, path, "core.status"); resp.Error != nil {
			t.Errorf("first daemon harmed: %+v", resp)
		}
	})
	for name, make := range map[string]func(path string) error{
		"regular file": func(path string) error { return os.WriteFile(path, []byte("keep"), 0o600) },
		"directory":    func(path string) error { return os.Mkdir(path, 0o700) },
		"symlink":      func(path string) error { return os.Symlink("/etc/passwd", path) },
	} {
		t.Run(name, func(t *testing.T) {
			path := socketPath(t)
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := make(path); err != nil {
				t.Fatal(err)
			}
			if _, err := Listen(options(path, root)); err == nil || !strings.Contains(err.Error(), "not a socket") {
				t.Errorf("err = %v", err)
			}
			if _, err := os.Lstat(path); err != nil {
				t.Errorf("entry removed: %v", err)
			}
		})
	}
}

func TestListenRefusesAnUnsafeParent(t *testing.T) {
	path := socketPath(t)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Dir(path), 0o777); err != nil { // writable by others, not sticky
		t.Fatal(err)
	}
	if _, err := Listen(options(path, root)); err == nil || !strings.Contains(err.Error(), "writable by group or others") {
		t.Errorf("err = %v", err)
	}
}

func TestListenRejectsBadBindings(t *testing.T) {
	path := socketPath(t)
	if _, err := Listen(options(path, root, Binding{Command: api.CoreStatus, Run: echo})); err == nil {
		t.Error("a command bound twice was accepted")
	}
	if _, err := Listen(options(path, root, Binding{Command: api.Command{Name: "status", Tier: api.TierRead}, Run: echo})); err == nil {
		t.Error("an invalid command name was accepted")
	}
}

// pipeServer returns a server for serve() over net.Pipe, without a socket.
func pipeServer(peer authz.Peer, bindings ...Binding) *Server {
	opts := options("", peer, bindings...)
	opts.Timeout = DefaultTimeout
	ctx, cancel := context.WithCancel(context.Background())
	s := &Server{opts: opts, commands: map[string]Binding{}, ctx: ctx, cancel: cancel}
	for _, b := range opts.Bindings {
		s.commands[b.Command.Name] = b
	}
	return s
}

// exchange writes raw to a server goroutine over a pipe and returns the
// response line it gets back.
func exchange(t *testing.T, s *Server, peer authz.Peer, peerErr error, raw string) api.Response {
	t.Helper()
	client, server := net.Pipe()
	defer client.Close()
	go s.serve(server, peer, peerErr)
	go func() { _, _ = io.WriteString(client, raw) }()
	resp, err := api.ReadResponse(client)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	return resp
}

func TestServeAnswers(t *testing.T) {
	var ran atomic.Int32
	operate := Binding{Command: api.Command{Name: "core.reload", Tier: api.TierOperate}, Run: func(context.Context, jsontext.Value) (any, *api.Error) {
		ran.Add(1)
		return nil, nil
	}}
	failing := Binding{Command: api.Command{Name: "core.events", Tier: api.TierRead}, Run: func(context.Context, jsontext.Value) (any, *api.Error) {
		return nil, &api.Error{Code: api.CodeUnsupported, Message: "later"}
	}}
	huge := Binding{Command: api.Command{Name: "core.huge", Tier: api.TierRead}, Run: func(context.Context, jsontext.Value) (any, *api.Error) {
		return strings.Repeat("x", api.MaxResponseBytes), nil
	}}
	tests := []struct {
		name    string
		peer    authz.Peer
		peerErr error
		raw     string
		code    api.Code
	}{
		{"ok", visitor, nil, `{"version":1,"command":"core.status"}` + "\n", ""},
		{"malformed", visitor, nil, "{nope\n", api.CodeBadRequest},
		{"other version", visitor, nil, `{"version":7,"command":"core.status"}` + "\n", api.CodeUnsupportedVersion},
		{"unknown command", visitor, nil, `{"version":1,"command":"core.nope"}` + "\n", api.CodeUnknownCommand},
		{"denied", visitor, nil, `{"version":1,"command":"core.reload"}` + "\n", api.CodePermissionDenied},
		{"no credentials", root, errors.New("peer credentials are supported on Linux only"), `{"version":1,"command":"core.status"}` + "\n", api.CodeUnavailable},
		{"command error", visitor, nil, `{"version":1,"command":"core.events"}` + "\n", api.CodeUnsupported},
		{"result too large", visitor, nil, `{"version":1,"command":"core.huge"}` + "\n", api.CodeInternal},
		{"forged tier is not a member", root, nil, `{"version":1,"command":"core.status","tier":"admin"}` + "\n", api.CodeBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := exchange(t, pipeServer(tt.peer, operate, failing, huge), tt.peer, tt.peerErr, tt.raw)
			var got api.Code
			if resp.Error != nil {
				got = resp.Error.Code
			}
			if got != tt.code {
				t.Errorf("code %q, want %q (%+v)", got, tt.code, resp.Error)
			}
		})
	}
	if ran.Load() != 0 {
		t.Error("a denied command ran")
	}
}

// A client that never sends its request is cut off at the deadline.
func TestServeDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := pipeServer(visitor)
		client, server := net.Pipe()
		defer client.Close()
		done := make(chan struct{})
		go func() { s.serve(server, visitor, nil); close(done) }()
		begin := time.Now()
		<-done
		if elapsed := time.Since(begin); elapsed != DefaultTimeout {
			t.Errorf("served %s, deadline %s", elapsed, DefaultTimeout)
		}
	})
}

// Past MaxConns, a connection is closed at once; Close cancels running
// commands and returns once they end.
func TestBusyServerAndClose(t *testing.T) {
	path := socketPath(t)
	entered := make(chan struct{})
	blocking := Binding{Command: api.Command{Name: "core.block", Tier: api.TierRead}, Run: func(ctx context.Context, _ jsontext.Value) (any, *api.Error) {
		close(entered)
		<-ctx.Done()
		return nil, &api.Error{Code: api.CodeUnavailable, Message: "daemon stopping"}
	}}
	opts := options(path, root, blocking)
	opts.MaxConns = 1
	s, err := Listen(opts)
	if err != nil {
		t.Fatal(err)
	}
	s.Serve()
	first := make(chan api.Response, 1)
	go func() {
		resp, _ := Call(context.Background(), path, api.Request{Version: api.Version, Command: "core.block"})
		first <- resp
	}()
	<-entered
	if _, err := Call(context.Background(), path, api.Request{Version: api.Version, Command: "core.status"}); err == nil {
		t.Error("a connection past MaxConns was served")
	}
	if err := s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if resp := <-first; resp.Error == nil || resp.Error.Code != api.CodeUnavailable {
		t.Errorf("running command not cancelled cleanly: %+v", resp)
	}
}

func TestCallErrors(t *testing.T) {
	path := socketPath(t)
	_, err := Call(context.Background(), path, api.Request{Version: api.Version, Command: "core.status"})
	if !errors.Is(err, syscall.ENOENT) {
		t.Errorf("no daemon: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Call(ctx, path, api.Request{Version: api.Version, Command: "core.status"}); err == nil {
		t.Error("cancelled call succeeded")
	}
}

// A command that ignores cancellation does not hold Close past its
// deadline: its connection is cut and Close says so.
func TestCloseCutsConnectionsAtTheDeadline(t *testing.T) {
	path := socketPath(t)
	entered, release := make(chan struct{}), make(chan struct{})
	stuck := Binding{Command: api.Command{Name: "core.stuck", Tier: api.TierRead}, Run: func(context.Context, jsontext.Value) (any, *api.Error) {
		close(entered)
		<-release
		return nil, nil
	}}
	s, err := Listen(options(path, root, stuck))
	if err != nil {
		t.Fatal(err)
	}
	s.Serve()
	answered := make(chan error, 1)
	go func() {
		_, err := Call(context.Background(), path, api.Request{Version: api.Version, Command: "core.stuck"})
		answered <- err
	}()
	<-entered
	expired, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.Close(expired); err == nil || !strings.Contains(err.Error(), "cut") {
		t.Errorf("Close = %v", err)
	}
	if err := <-answered; err == nil {
		t.Error("the client of a cut connection got an answer")
	}
	if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("socket left behind: %v", err)
	}
	close(release)
}

// Review 2c-2 R1: the exchange deadline cancels the running command, not
// only the connection.
func TestExchangeDeadlineCancelsHandler(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cancelled := make(chan struct{})
		wait := Binding{Command: api.Command{Name: "core.wait", Tier: api.TierRead}, Run: func(ctx context.Context, _ jsontext.Value) (any, *api.Error) {
			<-ctx.Done()
			close(cancelled)
			return nil, nil
		}}
		s := pipeServer(visitor, wait)
		client, server := net.Pipe()
		defer client.Close()
		done := make(chan struct{})
		go func() { s.serve(server, visitor, nil); close(done) }()
		defer func() { s.cancel(); <-done }()
		if _, err := io.WriteString(client, `{"version":1,"command":"core.wait"}`+"\n"); err != nil {
			t.Fatal(err)
		}
		time.Sleep(DefaultTimeout + time.Second)
		select {
		case <-cancelled:
		default:
			t.Error("the exchange deadline did not cancel the command")
		}
	})
}

// Review 2c-2 (second) R3: under sentineld's umask 027 the socket
// directory sentineld creates still gets 0755, so members of socket_group
// can reach the socket.
func TestSocketDirectoryWithDaemonUmask(t *testing.T) {
	if os.Getenv("SENTINEL_TEST_UMASK_CHILD") == "1" {
		syscall.Umask(0o027)
		TestSocketModeGroupAndRemoval(t)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestSocketDirectoryWithDaemonUmask$")
	cmd.Env = append(os.Environ(), "SENTINEL_TEST_UMASK_CHILD=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("socket directory under the daemon's umask: %v\n%s", err, out)
	}
}
