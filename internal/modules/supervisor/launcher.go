package supervisor

import (
	"context"
	"syscall"
	"time"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/clock"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/platform/executor"
)

// launcher starts and cleans up the processes of process services. The
// variation it isolates is real processes (the executor) versus the fakes
// of the unit tests, which need no root, /proc or real time (D-084 (6)).
type launcher interface {
	Launch(ctx context.Context, spec executor.Spec) (launched, error)
	// Cleanup kills what is left of a group a previous launch recorded;
	// nil means nothing of it is alive.
	Cleanup(ctx context.Context, g executor.Group) error
	Close()
}

// proc is a launched process.
type proc interface {
	Done() <-chan struct{}
	// Result is valid once Done is closed: how the process ended, and the
	// first error of its output sinks.
	Result() (executor.Exit, error)
	Stop(ctx context.Context, sig syscall.Signal, timeout time.Duration) (executor.Exit, error)
}

// launched is a started process and what is known of it at once.
type launched struct {
	proc  proc
	pid   int
	group executor.Group // zero outside Linux
	// rss samples the resident memory of the process's leader in bytes;
	// executor.ErrGone once it has ended (D-084 (7)).
	rss func() (uint64, error)
}

// memorySampler is implemented by a launcher whose processes can be
// sampled on this system (launched.rss works). A memory limit that cannot
// be enforced fails the launch with FailureUnsupported (D-021, D-090).
type memorySampler interface {
	SamplesMemory() bool
}

// execLauncher is the launcher of production: the executor.
type execLauncher struct{ e *executor.Executor }

func newExecLauncher(c clock.Clock) (launcher, error) { return execLauncher{executor.New(c)}, nil }

func (l execLauncher) Launch(ctx context.Context, spec executor.Spec) (launched, error) {
	p, err := l.e.Start(ctx, spec)
	if err != nil {
		return launched{}, err
	}
	g := p.Group()
	return launched{proc: execProc{p}, pid: p.Pid(), group: g,
		rss: func() (uint64, error) { return l.e.LeaderRSS(g) }}, nil
}

// SamplesMemory implements memorySampler.
func (execLauncher) SamplesMemory() bool { return executor.SamplesMemory }

func (l execLauncher) Cleanup(ctx context.Context, g executor.Group) error {
	return l.e.Cleanup(ctx, g)
}

func (l execLauncher) Close() { l.e.Close() }

// execProc adapts executor.Process to proc.
type execProc struct{ *executor.Process }

func (p execProc) Result() (executor.Exit, error) { return p.Exit(), p.OutputErr() }
