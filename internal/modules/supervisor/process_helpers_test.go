package supervisor

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/clock"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/config"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/platform/executor"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/platform/privilege"
)

// armedCentral is centralFile with process services armed.
const armedCentral = centralFile + "    processes: true\n"

// fakeLauncher records launches and cleanups; each launch gives a
// fakeProcess that runs until ended or stopped.
type fakeLauncher struct {
	mu         sync.Mutex
	specs      []executor.Spec
	procs      []*fakeProcess
	cleanups   []executor.Group
	launchErr  error
	cleanupErr error
	// output is written to the launched process's stdout, line by line;
	// partial after it, without a newline: only closing the sink logs it.
	output  []string
	partial string
	// rss samples the i-th launched process (nil: 0 bytes); sampled
	// counts the samples; noSampling makes the launcher one that cannot
	// sample (memorySampler).
	rss        func(i int) (uint64, error)
	sampled    int
	noSampling bool
	// gate, when set, holds every Launch until it is closed; cleanupGate
	// holds every Cleanup the same way.
	gate        chan struct{}
	cleanupGate chan struct{}
	closed      bool
	pid         int
}

func (l *fakeLauncher) Launch(_ context.Context, spec executor.Spec) (launched, error) {
	l.mu.Lock()
	gate, err := l.gate, l.launchErr
	l.mu.Unlock()
	if gate != nil {
		<-gate
	}
	if err != nil {
		return launched{}, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.pid++
	p := &fakeProcess{done: make(chan struct{})}
	l.specs = append(l.specs, spec)
	l.procs = append(l.procs, p)
	if spec.Stdout != nil {
		for _, line := range l.output {
			_, _ = fmt.Fprintln(spec.Stdout, line)
		}
		_, _ = fmt.Fprint(spec.Stdout, l.partial)
	}
	g := executor.Group{Pgid: 1000 + l.pid, Sid: 50, Start: 7, Boot: testBoot}
	i := len(l.procs) - 1
	return launched{proc: p, pid: 1000 + l.pid, group: g, rss: func() (uint64, error) {
		l.mu.Lock()
		l.sampled++
		f := l.rss
		l.mu.Unlock()
		if f == nil {
			return 0, nil
		}
		return f(i)
	}}, nil
}

func (l *fakeLauncher) SamplesMemory() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return !l.noSampling
}

// setRSS decides every sample from now on, by launch index.
func (l *fakeLauncher) setRSS(f func(i int) (uint64, error)) {
	l.mu.Lock()
	l.rss = f
	l.mu.Unlock()
}

func (l *fakeLauncher) samples() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.sampled
}

func (l *fakeLauncher) Cleanup(ctx context.Context, g executor.Group) error {
	l.mu.Lock()
	gate := l.cleanupGate
	l.mu.Unlock()
	if gate != nil {
		select { // the real cleanup waits between rounds under ctx
		case <-gate:
		case <-ctx.Done():
			return fmt.Errorf("%w: %w", executor.ErrCleanupIncomplete, ctx.Err())
		}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.cleanups = append(l.cleanups, g)
	return l.cleanupErr
}

func (l *fakeLauncher) Close() {
	l.mu.Lock()
	l.closed = true
	l.mu.Unlock()
}

func (l *fakeLauncher) launches() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.procs)
}

func (l *fakeLauncher) proc(i int) *fakeProcess {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.procs[i]
}

func (l *fakeLauncher) spec(i int) executor.Spec {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.specs[i]
}

func (l *fakeLauncher) cleaned() []executor.Group {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]executor.Group(nil), l.cleanups...)
}

const testBoot = "6a3c8f1e-2b4d-4e5f-8a9b-0c1d2e3f4a5b"

// fakeProcess ends when end is called or when stopped (unless stubborn).
type fakeProcess struct {
	mu       sync.Mutex
	done     chan struct{}
	exit     executor.Exit
	outErr   error
	stops    []stopCall
	stubborn bool // Stop waits for ctx
	// killedAtDeadline: Stop waits for ctx, then the process ends (as
	// the real executor's SIGKILL ends it) while Stop reports ctx's error.
	killedAtDeadline bool
	// gracefulAfter, when set: Stop ends the process with its signal after
	// this long, or with SIGKILL when ctx ends first (as the executor).
	gracefulAfter time.Duration
	// held: each Stop waits, whatever its ctx, until its own channel in
	// holds is closed, then ends the process.
	held  bool
	holds []chan struct{}
	once  sync.Once
}

type stopCall struct {
	sig     syscall.Signal
	timeout time.Duration
}

func (p *fakeProcess) Done() <-chan struct{} { return p.done }

func (p *fakeProcess) Result() (executor.Exit, error) {
	<-p.done
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.exit, p.outErr
}

func (p *fakeProcess) Stop(ctx context.Context, sig syscall.Signal, timeout time.Duration) (executor.Exit, error) {
	p.mu.Lock()
	p.stops = append(p.stops, stopCall{sig, timeout})
	stubborn, killed, graceful := p.stubborn, p.killedAtDeadline, p.gracefulAfter
	var hold chan struct{}
	if p.held {
		hold = make(chan struct{})
		p.holds = append(p.holds, hold)
	}
	p.mu.Unlock()
	if hold != nil {
		<-hold
	}
	if graceful > 0 {
		select {
		case <-p.done:
		case <-time.After(graceful):
			p.end(executor.Exit{Code: -1, Signal: "SIGTERM"}, nil)
		case <-ctx.Done():
			p.end(executor.Exit{Code: -1, Signal: "SIGKILL"}, nil)
			return executor.Exit{}, ctx.Err()
		}
		return p.Result()
	}
	if killed {
		select {
		case <-p.done: // already killed by an earlier Stop
			return p.Result()
		case <-ctx.Done():
		}
		p.end(executor.Exit{Code: -1, Signal: "SIGKILL"}, nil)
		return executor.Exit{}, ctx.Err()
	}
	if stubborn {
		select {
		case <-p.done:
		case <-ctx.Done():
			return executor.Exit{}, ctx.Err()
		}
	}
	p.end(executor.Exit{Code: -1, Signal: "SIGTERM"}, nil)
	return p.Result()
}

// end makes the process end with exit (once).
func (p *fakeProcess) end(exit executor.Exit, outErr error) {
	p.once.Do(func() {
		p.mu.Lock()
		p.exit, p.outErr = exit, outErr
		p.mu.Unlock()
		close(p.done)
	})
}

func (p *fakeProcess) stopCalls() []stopCall {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]stopCall(nil), p.stops...)
}

// fakeLookup knows the user worker (uid 1001, gid 1001) and the group
// staff (gid 50).
var fakeLookup = privilege.Lookup{
	User: func(name string) (uint32, uint32, error) {
		if name == "worker" {
			return 1001, 1001, nil
		}
		return 0, 0, privilege.ErrUnknownUser
	},
	Group: func(name string) (uint32, error) {
		if name == "staff" {
			return 50, nil
		}
		return 0, privilege.ErrUnknownGroup
	},
}

// configureProcs configures files (central file armedCentral unless
// given) with l as launcher.
func configureProcs(t *testing.T, files map[string]string, l *fakeLauncher) (*instance, error) {
	t.Helper()
	if _, ok := files["sentinel.yaml"]; !ok {
		files["sentinel.yaml"] = armedCentral
	}
	mc, err := moduleConfig(t, files, map[string]string{"TOKEN": "tok-9f8e7d"})
	if err != nil {
		return nil, err
	}
	c, err := New(Options{NewChecker: newFakes().build, Lookup: fakeLookup,
		NewLauncher: func(clock.Clock) (launcher, error) { return l, nil }}).Configure(mc)
	if err != nil {
		return nil, err
	}
	return c.(*instance), nil
}

func mustConfigureProcs(t *testing.T, files map[string]string, l *fakeLauncher) *instance {
	t.Helper()
	in, err := configureProcs(t, files, l)
	if err != nil {
		t.Fatal(err)
	}
	return in
}

var errFakeExec = errors.New("exec format error")

func asValidation(err error, target **config.ValidationError) bool { return errors.As(err, target) }
