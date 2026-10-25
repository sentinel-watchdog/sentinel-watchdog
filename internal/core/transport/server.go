// Package transport serves the control socket (ADR-0012): it accepts a
// connection, reads one request, authorizes it from the peer's kernel
// credentials, audits it when its command needs the operate or admin
// tier, runs the command bound to it and writes one response. It also
// creates the socket file safely and offers the client side.
package transport

import (
	"context"
	"crypto/rand"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/audit"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/authz"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/clock"
	"github.com/sentinel-watchdog/sentinel-watchdog/pkg/api"
)

// Defaults of Options.
const (
	DefaultTimeout  = 5 * time.Second // one whole exchange
	DefaultMaxConns = 32
)

// Run executes a command for peer, already authorized. args is the
// request's argument object (empty when absent). It returns the value to
// encode as the result, or an error for the client. ctx ends at the
// exchange's deadline or when the server closes: a command must honour it.
type Run func(ctx context.Context, peer authz.Peer, args jsontext.Value) (any, *api.Error)

// Binding ties a command, with the tier it declares, to its code. The
// table of bindings is fixed when the server is created: what a client may
// run, and with which tier, never comes from the client.
type Binding struct {
	Command api.Command
	Run     Run
	// Timeout bounds the exchange once the request names this command,
	// for a command that needs longer than Options.Timeout (a reload
	// stops and starts modules); zero keeps Options.Timeout.
	Timeout time.Duration
}

// Recorder writes audit records: the audit log in sentineld. It is an
// interface so that tests can make recording fail and check that the
// command then does not run.
type Recorder interface {
	Record(ctx context.Context, e audit.Entry) (audit.Record, error)
}

// Options configure a Server.
type Options struct {
	// Path, Mode and GID of the socket file (daemon.socket, socket_mode,
	// the gid of socket_group or -1 to keep the daemon's).
	Path string
	Mode fs.FileMode
	GID  int
	// Policy gives each peer its tier (SetPolicy replaces it); Peer reads
	// the peer's credentials from the kernel (internal/platform/peercred).
	Policy   authz.Policy
	Peer     func(*net.UnixConn) (authz.Peer, error)
	Bindings []Binding
	// Recorder audits every request for a command of tier operate or
	// admin, accepted or denied (ADR-0012). Listen refuses such bindings
	// without it.
	Recorder Recorder
	// Clock paces the admission of connections per user; defaults to
	// clock.Real().
	Clock  clock.Clock
	Logger *slog.Logger
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
	admit    *admission    // used by the accept loop only
	policy   atomic.Pointer[authz.Policy]

	ctx    context.Context // ends handlers when the server closes
	cancel context.CancelFunc
	mu     sync.Mutex
	conns  map[net.Conn]struct{}
	wg     sync.WaitGroup // Serve's accept loop and connection goroutines
	closed bool

	releaseOnce sync.Once
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
	if opts.Clock == nil {
		opts.Clock = clock.Real()
	}
	if opts.Peer == nil {
		return nil, errors.New("transport: no peer credentials function")
	}
	commands := map[string]Binding{}
	for _, b := range opts.Bindings {
		if b.Run == nil {
			return nil, fmt.Errorf("transport: command %q has no Run function", b.Command.Name)
		}
		if !api.ValidCommandName(b.Command.Name) {
			return nil, fmt.Errorf("transport: invalid command name %q", b.Command.Name)
		}
		if _, dup := commands[b.Command.Name]; dup {
			return nil, fmt.Errorf("transport: command %q bound twice", b.Command.Name)
		}
		if b.Command.Tier != api.TierRead && opts.Recorder == nil {
			return nil, fmt.Errorf("transport: command %q needs tier %q and there is no audit recorder", b.Command.Name, b.Command.Tier)
		}
		commands[b.Command.Name] = b
	}
	sock, err := listen(opts.Path, opts.Mode, opts.GID)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Server{opts: opts, commands: commands, sock: sock, slots: make(chan struct{}, opts.MaxConns),
		admit: newAdmission(opts.Clock), ctx: ctx, cancel: cancel, conns: map[net.Conn]struct{}{}}
	s.SetPolicy(opts.Policy)
	return s, nil
}

// SetPolicy replaces the policy for the requests read from now on (a
// reload that changes daemon.access); a request already authorized keeps
// its tier.
func (s *Server) SetPolicy(p authz.Policy) {
	s.policy.Store(&p)
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
		// The kernel's credentials are a snapshot taken at connect():
		// reading them does not block.
		peer, peerErr := s.opts.Peer(conn)
		if !s.admitted(peer, peerErr) || !s.takeSlot(peerErr == nil && peer.UID == 0) {
			// Busy or too frequent: close at once rather than queue a
			// goroutine per waiting client.
			_ = conn.Close()
			continue
		}
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

// admitted applies the per-user admission limit. A peer whose credentials
// are unknown shares one bucket.
func (s *Server) admitted(peer authz.Peer, peerErr error) bool {
	uid := peer.UID
	if peerErr != nil {
		uid = ^uint32(0)
	}
	ok, first := s.admit.allow(uid)
	if first {
		s.opts.Logger.Warn("control socket: too many connections from one user; refusing until the rate drops", "uid", uid)
	}
	return ok
}

// takeSlot takes a connection slot. The last slot is kept for root, so
// other users holding every slot cannot lock the administrator out.
func (s *Server) takeSlot(root bool) bool {
	// Only this goroutine adds tokens, so the length cannot grow between
	// the check and the send.
	if !root && s.opts.MaxConns > 1 && len(s.slots) >= s.opts.MaxConns-1 {
		return false
	}
	select {
	case s.slots <- struct{}{}:
		return true
	default:
		return false
	}
}

// serve answers one connection: one request, one response, then close.
// Every step happens before the exchange's deadline.
func (s *Server) serve(conn net.Conn, peer authz.Peer, peerErr error) {
	defer conn.Close()
	ex := &exchange{conn: conn, parent: s.ctx}
	ex.limit(s.opts.Timeout)
	defer func() { ex.cancel() }()
	if err := api.WriteResponse(conn, s.respond(ex, peer, peerErr)); err != nil {
		// Too large, or the client is gone: try a short error, which fails
		// harmlessly in the second case.
		_ = api.WriteResponse(conn, s.fail("response could not be written", err))
	}
}

// exchange is one connection being served: the command shares its
// deadline, which a command with a longer time limit extends.
type exchange struct {
	conn   net.Conn
	parent context.Context // ends when the server closes
	ctx    context.Context
	cancel context.CancelFunc
}

// limit sets the exchange's deadline d from now, on the connection and on
// the command's context.
func (e *exchange) limit(d time.Duration) {
	if e.cancel != nil {
		e.cancel()
	}
	deadline := time.Now().Add(d) // a net.Conn deadline is wall-clock time
	_ = e.conn.SetDeadline(deadline)
	e.ctx, e.cancel = context.WithDeadline(e.parent, deadline)
}

func (s *Server) respond(ex *exchange, peer authz.Peer, peerErr error) api.Response {
	req, err := api.ReadRequest(ex.conn)
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
	policy := s.policy.Load()
	privileged := b.Command.Tier != api.TierRead
	target := targetOf(b.Command, req.Args)
	if denied := policy.Authorize(peer, b.Command); denied != nil {
		s.opts.Logger.Warn("control socket: permission denied", "command", req.Command, "uid", peer.UID, "gid", peer.GID)
		if privileged {
			// A denial never depends on the audit log: a failure is only logged.
			_ = s.record(ex.ctx, peer, b.Command, target, audit.ResultDenied, "",
				map[string]string{"tier": string(policy.Tier(peer))})
		}
		return api.Response{Version: api.Version, Error: denied}
	}
	if b.Timeout > s.opts.Timeout {
		ex.limit(b.Timeout) // authorized: only now may it hold the connection longer
	}
	if !privileged {
		return s.run(ex.ctx, b, peer, req.Args)
	}
	// Fail closed: the command runs only once its intent is durable.
	correlation := rand.Text()
	if err := s.record(ex.ctx, peer, b.Command, target, audit.ResultIntent, correlation, nil); err != nil {
		message := "the audit log cannot record this request; it was not run"
		if errors.Is(err, audit.ErrBroken) {
			message = "the audit chain is broken; operate and admin commands are refused until the evidence is restored (see sentinelctl status)"
		}
		return api.Fail(api.CodeUnavailable, message)
	}
	// The command may record its own records on behalf of this request
	// (a module's action that outlives the exchange): they carry its id.
	resp := s.run(audit.WithCorrelation(ex.ctx, correlation), b, peer, req.Args)
	result, detail := audit.ResultOK, map[string]string(nil)
	if resp.Error != nil {
		result, detail = audit.ResultFailed, map[string]string{"code": string(resp.Error.Code)}
	}
	// The command has run: its outcome is recorded even if the exchange's
	// deadline has passed, within a deadline of its own. A failure here is
	// reported by the recorder (the daemon is then degraded); the client
	// still gets the command's response.
	done, cancel := context.WithTimeout(context.WithoutCancel(ex.ctx), s.opts.Timeout)
	defer cancel()
	_ = s.record(done, peer, b.Command, target, result, correlation, detail)
	return resp
}

// run runs a bound command and encodes its result.
func (s *Server) run(ctx context.Context, b Binding, peer authz.Peer, args jsontext.Value) api.Response {
	result, cmdErr := b.Run(ctx, peer, args)
	if cmdErr != nil {
		return api.Response{Version: api.Version, Error: cmdErr}
	}
	resp, err := api.Result(result)
	if err != nil {
		return s.fail("result could not be encoded", err)
	}
	return resp
}

// record writes one audit record for a request of peer, and logs a failure.
func (s *Server) record(ctx context.Context, peer authz.Peer, cmd api.Command, target string, result audit.Result,
	correlation string, detail map[string]string) error {
	uid, gid := peer.UID, peer.GID
	_, err := s.opts.Recorder.Record(ctx, audit.Entry{
		Actor:  audit.Actor{Kind: audit.ActorSocket, UID: &uid, GID: &gid, PID: peer.PID},
		Action: cmd.Name, Target: target, Result: result, CorrelationID: correlation, Detail: detail,
	})
	if err != nil {
		s.opts.Logger.Error("control socket: audit record not written", "command", cmd.Name, "result", result, "uid", uid, "error", err)
	}
	return err
}

// targetOf is the target of a request for cmd in its audit records: for a
// command that acts on a named object (TargetsName), the "name" member of
// args when it is a valid name, else "-" (the command refuses it); "" for
// other commands. A client's raw input never reaches the audit log.
func targetOf(cmd api.Command, args jsontext.Value) string {
	if !cmd.TargetsName {
		return ""
	}
	var a struct {
		Name string `json:"name"`
	}
	if json.Unmarshal(args, &a) != nil || !api.ValidName(a.Name) {
		return "-"
	}
	return a.Name
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

// Close stops serving (Shutdown) and releases the socket's lock (Release).
func (s *Server) Close(ctx context.Context) error {
	err := s.Shutdown(ctx)
	s.Release()
	return err
}

// Shutdown stops accepting and cancels running commands, which then answer
// their client; it waits for the connections until ctx ends, closes those
// still open, then removes the socket file. The lock stays held until
// Release: a second sentineld cannot start while this one still stops its
// modules or writes its audit log. It returns an error if connections had
// to be cut.
func (s *Server) Shutdown(ctx context.Context) error {
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
	return errors.Join(err, s.sock.removeFile())
}

// Release releases the socket's lock and its directory, once the daemon
// has nothing left running. Calling it again does nothing.
func (s *Server) Release() {
	s.releaseOnce.Do(s.sock.release)
}
