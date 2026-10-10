// Package transport serves the control socket (ADR-0012): it accepts a
// connection, reads one request, authorizes it from the peer's kernel
// credentials, runs the command bound to it and writes one response. It
// also creates the socket file safely and offers the client side.
package transport

import (
	"context"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/authz"
	"github.com/sentinel-watchdog/sentinel-watchdog/pkg/api"
)

// Defaults of Options.
const (
	DefaultTimeout  = 5 * time.Second // one whole exchange
	DefaultMaxConns = 32
)

// Run executes a command. args is the request's argument object (empty
// when absent). It returns the value to encode as the result, or an error
// for the client. ctx ends at the exchange's deadline or when the server
// closes: a command must honour it.
type Run func(ctx context.Context, args jsontext.Value) (any, *api.Error)

// Binding ties a command, with the tier it declares, to its code. The
// table of bindings is fixed when the server is created: what a client may
// run, and with which tier, never comes from the client.
type Binding struct {
	Command api.Command
	Run     Run
}

// Options configure a Server.
type Options struct {
	// Path, Mode and GID of the socket file (daemon.socket, socket_mode,
	// the gid of socket_group or -1 to keep the daemon's).
	Path string
	Mode fs.FileMode
	GID  int
	// Policy gives each peer its tier; Peer reads the peer's credentials
	// from the kernel (internal/platform/peercred).
	Policy   authz.Policy
	Peer     func(*net.UnixConn) (authz.Peer, error)
	Bindings []Binding
	Logger   *slog.Logger
	// Timeout bounds one exchange; MaxConns bounds the connections served
	// at once (more are closed at once).
	Timeout  time.Duration
	MaxConns int
}

// Server is a listening control socket. Create it with Listen, run Serve,
// stop it with Close.
type Server struct {
	opts     Options
	commands map[string]Binding
	sock     *socket
	slots    chan struct{} // one token per connection being served

	ctx    context.Context // ends handlers when the server closes
	cancel context.CancelFunc
	mu     sync.Mutex
	conns  map[net.Conn]struct{}
	wg     sync.WaitGroup // Serve's accept loop and connection goroutines
	closed bool
}

// Listen creates the socket file (Path) and starts listening, without
// serving yet: the caller holds the socket, so a second daemon fails here,
// before it does any work.
func Listen(opts Options) (*Server, error) {
	if opts.Timeout <= 0 {
		opts.Timeout = DefaultTimeout
	}
	if opts.MaxConns <= 0 {
		opts.MaxConns = DefaultMaxConns
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	commands := map[string]Binding{}
	for _, b := range opts.Bindings {
		if !api.ValidCommandName(b.Command.Name) {
			return nil, fmt.Errorf("transport: invalid command name %q", b.Command.Name)
		}
		if _, dup := commands[b.Command.Name]; dup {
			return nil, fmt.Errorf("transport: command %q bound twice", b.Command.Name)
		}
		commands[b.Command.Name] = b
	}
	sock, err := listen(opts.Path, opts.Mode, opts.GID)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Server{opts: opts, commands: commands, sock: sock, slots: make(chan struct{}, opts.MaxConns),
		ctx: ctx, cancel: cancel, conns: map[net.Conn]struct{}{}}, nil
}

// Serve accepts connections in its own goroutine until Close.
func (s *Server) Serve() {
	s.wg.Go(s.accept)
}

func (s *Server) accept() {
	for {
		conn, err := s.sock.listener.AcceptUnix()
		if err != nil {
			if !errors.Is(err, net.ErrClosed) {
				s.opts.Logger.Error("control socket: accept failed; no longer serving", "error", err)
			}
			return
		}
		select {
		case s.slots <- struct{}{}:
		default:
			// Busy: close at once rather than queue a goroutine per waiting client.
			_ = conn.Close()
			continue
		}
		peer, peerErr := s.opts.Peer(conn)
		if !s.track(conn) {
			<-s.slots
			_ = conn.Close()
			return
		}
		s.wg.Go(func() {
			defer func() { <-s.slots }()
			defer s.untrack(conn)
			s.serve(conn, peer, peerErr)
		})
	}
}

// serve answers one connection: one request, one response, then close.
// Every step happens before the deadline set here.
func (s *Server) serve(conn net.Conn, peer authz.Peer, peerErr error) {
	defer conn.Close()
	deadline := time.Now().Add(s.opts.Timeout) // a net.Conn deadline is wall-clock time
	_ = conn.SetDeadline(deadline)
	ctx, cancel := context.WithDeadline(s.ctx, deadline) // the command shares the exchange's deadline
	defer cancel()
	if err := api.WriteResponse(conn, s.respond(ctx, conn, peer, peerErr)); err != nil {
		// Too large, or the client is gone: try a short error, which fails
		// harmlessly in the second case.
		_ = api.WriteResponse(conn, s.fail("response could not be written", err))
	}
}

func (s *Server) respond(ctx context.Context, conn net.Conn, peer authz.Peer, peerErr error) api.Response {
	req, err := api.ReadRequest(conn)
	if errors.Is(err, api.ErrUnsupportedVersion) {
		return api.Fail(api.CodeUnsupportedVersion, fmt.Sprintf("this daemon speaks protocol version %d", api.Version))
	}
	if err != nil {
		return api.Fail(api.CodeBadRequest, "malformed request: "+err.Error())
	}
	if peerErr != nil {
		s.opts.Logger.Warn("control socket: peer credentials unavailable", "command", req.Command, "error", peerErr)
		return api.Fail(api.CodeUnavailable, peerErr.Error())
	}
	b, ok := s.commands[req.Command]
	if !ok {
		return api.Fail(api.CodeUnknownCommand, fmt.Sprintf("unknown command %q", req.Command))
	}
	if denied := s.opts.Policy.Authorize(peer, b.Command); denied != nil {
		s.opts.Logger.Warn("control socket: permission denied", "command", req.Command, "uid", peer.UID, "gid", peer.GID)
		return api.Response{Version: api.Version, Error: denied}
	}
	result, cmdErr := b.Run(ctx, req.Args)
	if cmdErr != nil {
		return api.Response{Version: api.Version, Error: cmdErr}
	}
	resp, err := api.Result(result)
	if err != nil {
		return s.fail("result could not be encoded", err)
	}
	return resp
}

// fail logs err and returns an internal error with a fixed public message.
func (s *Server) fail(message string, err error) api.Response {
	s.opts.Logger.Error("control socket: "+message, "error", err)
	return api.Fail(api.CodeInternal, message)
}

func (s *Server) track(conn net.Conn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	s.conns[conn] = struct{}{}
	return true
}

func (s *Server) untrack(conn net.Conn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.conns, conn)
}

// Close stops accepting and cancels running commands, which then answer
// their client; it waits for the connections until ctx ends, closes those
// still open, then removes the socket file and releases its lock. It
// returns an error if connections had to be cut.
func (s *Server) Close(ctx context.Context) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.mu.Unlock()
	s.cancel()
	_ = s.sock.listener.Close() // ends accept
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	var err error
	select {
	case <-done:
	case <-ctx.Done():
		err = errors.New("transport: connections still open at the deadline were cut")
		s.mu.Lock()
		for conn := range s.conns {
			_ = conn.Close() // unblocks their reads and writes
		}
		s.mu.Unlock()
	}
	return errors.Join(err, s.sock.close())
}
