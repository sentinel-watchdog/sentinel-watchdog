package supervisor

import (
	"context"
	"errors"
	"io"
	"strconv"
	"time"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/audit"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/clock"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/module"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/platform/executor"
	"github.com/sentinel-watchdog/sentinel-watchdog/pkg/model"
)

// memorySampleInterval is how often the leader of a process with
// limits.memory is sampled (D-090). A process may allocate past its limit
// for up to this long before it is stopped.
const memorySampleInterval = 5 * time.Second

// memoryOver is a memory stop in progress or done: the sample over the
// limit and the incident that the stop's audit record names.
type memoryOver struct {
	rss      uint64
	incident string
}

// sampleTimer fires every memorySampleInterval for a service with a
// memory limit; its channel is nil, and never ready, for one without.
// clock.Timer fires once, so each round arms a new one.
type sampleTimer struct {
	clock clock.Clock
	t     clock.Timer
	c     <-chan time.Time
}

func (in *instance) sampler(s *service) *sampleTimer {
	st := &sampleTimer{clock: in.rt.Clock}
	if s.def.Process.Limits.Memory > 0 {
		st.rearm()
	}
	return st
}

func (st *sampleTimer) rearm() {
	st.t = st.clock.NewTimer(memorySampleInterval)
	st.c = st.t.C()
}

func (st *sampleTimer) stop() {
	if st.t != nil {
		st.t.Stop()
	}
}

// checkMemory samples the leader of s's process and stops it when it is
// over the limit, once. It reports whether the process has ended and been
// recorded: the caller's watch is over. A sample that cannot be read
// leaves the process running, unchecked until a sample succeeds (D-090:
// fail open, never read as zero).
func (in *instance) checkMemory(ctx context.Context, s *service, lp launched, sinks []io.Closer, starting bool) bool {
	in.mu.Lock()
	stopping := s.over != nil
	in.mu.Unlock()
	if stopping { // a memory stop that gave up waiting: its end is on Done
		return false
	}
	rss, err := lp.rss()
	switch {
	case errors.Is(err, executor.ErrGone):
		return false // its end is on Done
	case err != nil:
		in.mu.Lock()
		first := !s.memUnknown
		s.memUnknown = true
		in.mu.Unlock()
		if first { // one error per streak, not one per sample
			in.log.Error("supervisor cannot read the memory of the process; it keeps running, its limit unchecked until a sample succeeds",
				"service", s.def.Name, "error", err)
		}
		return false
	}
	in.mu.Lock()
	s.memRSS, s.memSampledAt, s.memUnknown = rss, in.rt.Clock.Now(), false
	in.mu.Unlock()
	if rss <= s.def.Process.Limits.Memory {
		return false
	}
	return in.memoryStop(ctx, s, lp, sinks, rss, starting)
}

// memoryStop stops s's process for being over its memory limit, and
// records the end as a failure the recovery acts on (D-090). The intent
// is recorded first, bounded by auditTimeout, but the stop does not wait
// for a working audit log: like a cleanup (D-084 (6)) it protects the
// host. Once decided, the stop keeps its stop_timeout even if the loop is
// ended meanwhile (a reload, an operator's action, module Stop): they wait
// for it instead of turning it into an immediate SIGKILL (review 3b-6 C1).
// Only module Stop's own deadline cuts it short, as it does every stop
// (review 3b-6 R1, rejected: the group is killed rather than orphaned).
// It reports whether the process ended and was recorded; otherwise the
// process stays owned and watched, and its end is still memory_limit.
func (in *instance) memoryStop(ctx context.Context, s *service, lp launched, sinks []io.Closer, rss uint64, starting bool) bool {
	pr := s.def.Process
	in.mu.Lock()
	incident := s.st.IncidentID
	if s.st.Health != HealthFailed {
		incident = model.NewEventID()
	}
	s.over = &memoryOver{rss: rss, incident: incident}
	s.phase = phaseStopping
	in.mu.Unlock()
	in.log.Warn("supervisor stops the process: its memory is over the limit", "service", s.def.Name,
		"rss_bytes", rss, "limit_bytes", pr.Limits.Memory)
	detached := context.WithoutCancel(ctx)
	if err := in.auditMemory(detached, s, incident, audit.ResultIntent, rss, nil); err != nil {
		in.log.Error("supervisor could not record the memory stop in the audit log; the process is stopped anyway",
			"service", s.def.Name, "error", err)
	}
	sctx, cancel := context.WithTimeout(detached, pr.StopTimeout+cleanupTimeout)
	defer cancel()
	exit, err := lp.proc.Stop(sctx, pr.StopSignal, pr.StopTimeout)
	if err != nil {
		in.log.Error("supervisor process did not end when stopped for its memory limit; it stays owned and watched",
			"service", s.def.Name, "error", err)
		in.outcomeMemory(detached, s, incident, audit.ResultFailed, rss, map[string]string{"failure": "did_not_end"})
		return false
	}
	if !in.claim(s, lp.proc) {
		in.log.Info("supervisor memory stop was completed by sentineld's stop", "service", s.def.Name)
		in.outcomeMemory(detached, s, incident, audit.ResultOK, rss, nil)
		return true
	}
	closeAll(sinks)
	_, outErr := lp.proc.Result()
	in.processFailed(s, FailureMemory, &exit, starting)
	in.mu.Lock()
	s.outputLost = outErr != nil
	in.mu.Unlock()
	in.outcomeMemory(detached, s, incident, audit.ResultOK, rss, nil)
	in.release(detached, s, lp.group)
	return true
}

// claim makes the caller the one that records the end of p, the process
// of s: false when another already has (it is no longer s's process).
func (in *instance) claim(s *service, p proc) bool {
	in.mu.Lock()
	defer in.mu.Unlock()
	if s.proc != p {
		return false
	}
	s.proc = nil
	return true
}

// auditMemory records a memory stop of s, bounded by auditTimeout, with
// the service as target and the incident as correlation; the actor is
// sentineld (D-087). It names sizes only, never the program.
func (in *instance) auditMemory(ctx context.Context, s *service, incident string, result audit.Result, rss uint64, extra map[string]string) error {
	detail := map[string]string{"trigger": triggerMemory, "rss_bytes": strconv.FormatUint(rss, 10),
		"limit_bytes": strconv.FormatUint(s.def.Process.Limits.Memory, 10)}
	for k, v := range extra {
		detail[k] = v
	}
	ctx, cancel := context.WithTimeout(ctx, auditTimeout)
	defer cancel()
	return in.rt.Audit.Record(ctx, module.AuditEntry{Action: actionStop, Target: s.def.Name, Result: result,
		CorrelationID: incident, Detail: detail})
}

// outcomeMemory records the outcome of a memory stop, logging a failure.
func (in *instance) outcomeMemory(ctx context.Context, s *service, incident string, result audit.Result, rss uint64, extra map[string]string) {
	if err := in.auditMemory(ctx, s, incident, result, rss, extra); err != nil {
		in.log.Error("supervisor could not record the outcome of a memory stop in the audit log", "service", s.def.Name, "error", err)
	}
}
