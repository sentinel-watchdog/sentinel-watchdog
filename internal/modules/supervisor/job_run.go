package supervisor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/audit"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/clock"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/platform/executor"
	"github.com/sentinel-watchdog/sentinel-watchdog/pkg/model"
)

// jobRun is one launched run of a job. The module owns it, not the job's
// loop: a reload that ends the loop leaves it running under the
// definition it was launched with (Q7, Q12). Only its supervise goroutine
// decides how it ended, so its result is claimed once (review C5).
type jobRun struct {
	id        string
	def       Job // as launched
	scheduled time.Time
	started   time.Time
	proc      proc
	pid       int
	group     executor.Group
	sinks     []io.Closer
	stop      chan struct{} // closed to ask for a stop: shutdown, processes off, replace
	stopOnce  sync.Once
	stopWhy   string        // the failure a stop reports; set before stop is closed
	done      chan struct{} // closed once supervise has returned
}

// requestStop asks r's supervise to stop the run (canceled).
func (r *jobRun) requestStop() { r.requestStopFor(failureCanceled) }

// requestStopFor asks r's supervise to stop the run and report why; the
// first request decides.
func (r *jobRun) requestStopFor(why string) {
	r.stopOnce.Do(func() {
		r.stopWhy = why
		close(r.stop)
	})
}

// launchRun launches the reserved run of a, then hands it to its own
// supervise goroutine. ctx is the loop's: it cancels a launch not queued
// yet, and a process it started stays the module's, ending as its own
// lifetime says (review C2). The timeout runs from the launch, before any
// bookkeeping (review C6).
func (in *instance) launchRun(ctx context.Context, j *job, a *runAdmission) {
	def, id, slot := a.def, a.id, a.slot
	spec, sinks, code := in.prepareProgram(def.Program, "job", def.Name)
	if code != "" {
		in.runOutcome(ctx, a, audit.ResultFailed, map[string]string{"failure": string(code)})
		in.endUnlaunched(def, id, slot, failureLaunch)
		return
	}
	// The last look: a disable or a reload may have come while the state was
	// saved. A launch already on its way is stopped below (panel F5).
	in.mu.Lock()
	canceled := in.canceledLocked(ctx, j)
	in.mu.Unlock()
	if canceled {
		closeAll(sinks)
		in.endCanceled(ctx, a)
		return
	}
	lp, err := in.launcher.Launch(ctx, spec)
	if err != nil {
		closeAll(sinks)
		// Only the context's own error shows the program never ran: another,
		// also during a cancellation, may follow a start (re-review 3c-4b R1).
		if errors.Is(err, context.Canceled) {
			in.endCanceled(ctx, a)
			return
		}
		in.log.Error("supervisor could not start the job", "job", def.Name, "error", in.redactor()(err.Error()))
		in.runOutcome(ctx, a, audit.ResultFailed, map[string]string{"failure": string(launchFailure(err))})
		in.endUnlaunched(def, id, slot, failureLaunch)
		return
	}
	timer := in.rt.Clock.NewTimer(def.Timeout)
	now := in.rt.Clock.Now()
	r := &jobRun{id: id, def: def, scheduled: slot, started: now, proc: lp.proc, pid: lp.pid, group: lp.group,
		sinks: sinks, stop: make(chan struct{}), done: make(chan struct{})}
	in.mu.Lock()
	rec := in.runRecs[id]
	rec.Phase, rec.Started = runRunning, now
	if !lp.group.IsZero() {
		rec.Group = recordOf(lp.group)
	}
	in.runRecs[id] = rec
	in.runs[id] = r
	// Stop collected the runs, a reload disarmed them, or a reload ended the
	// job's loop, while the launch was on its way: it is stopped at once
	// (review 3c-4a C4, R2).
	late := in.runsCollected || !in.processes || j.ending
	in.gen++
	gen := in.gen
	in.mu.Unlock()
	go in.supervise(r, timer)
	if late {
		r.requestStop()
	}
	in.save(gen) // the group first: a crash from now on leaves it for the next start
	in.log.Info("supervisor started the job", "job", def.Name, "pid", lp.pid, "scheduled", slot)
	in.runOutcome(ctx, a, audit.ResultOK, pidDetail(lp.pid))
}

// supervise follows r until it ends, its timeout passes or a stop is
// asked; then it stops it if needed, cleans up its group, saves the
// result and publishes it, in that order.
func (in *instance) supervise(r *jobRun, timer clock.Timer) {
	defer close(r.done)
	defer timer.Stop()
	defer func() {
		if v := recover(); v != nil { // the run stays recorded: the next start cleans it up
			in.log.Error("supervisor job supervision failed; the run is left to the next start's cleanup", "job", r.def.Name, "run_id", r.id)
		}
	}()
	res := runResult{RunID: r.id, Scheduled: r.scheduled, Started: r.started}
	var exit executor.Exit
	timedOut, stopped := false, false
	select {
	case <-r.proc.Done():
	case <-timer.C():
		timedOut = true
	case <-r.stop:
		stopped = true
	}
	select { // select picks at random among what is ready: a process already ended keeps its result (review C6)
	case <-r.proc.Done():
		timedOut, stopped = false, false
	default:
	}
	switch {
	case timedOut:
		in.log.Warn("supervisor stops the job at its timeout", "job", r.def.Name, "timeout", r.def.Timeout)
		exit = in.stopRun(r)
		res.Result = resultTimedOut
	case stopped:
		exit = in.stopRun(r)
		res.Result, res.Failure = resultFailed, r.stopWhy
	default:
		exit, _ = r.proc.Result()
		switch {
		case exit.Signal != "":
			res.Result, res.Failure = resultFailed, failureSignal
		case exit.Code != 0:
			res.Result, res.Failure = resultFailed, failureExitCode
		default:
			res.Result = resultSucceeded
		}
	}
	select { // every end that saw the process gone: its output error, if any (re-review R3)
	case <-r.proc.Done():
		if _, outErr := r.proc.Result(); outErr != nil {
			in.log.Error("supervisor lost output of the job's run", "job", r.def.Name, "run_id", r.id, "error", outErr)
		}
	default:
	}
	closeAll(r.sinks)
	res.Ended, res.Code, res.Signal = in.rt.Clock.Now(), exit.Code, exit.Signal
	cleaned := in.cleanupRun(r)
	in.mu.Lock()
	if cleaned {
		delete(in.runRecs, r.id)
	} else if rec, ok := in.runRecs[r.id]; ok {
		rec.Phase = runCleanupPending
		in.runRecs[r.id] = rec
	}
	delete(in.runs, r.id)
	in.recordResultLocked(r.def.Name, res)
	in.gen++
	gen := in.gen
	in.mu.Unlock()
	in.save(gen)
	in.log.Info("supervisor job ended", "job", r.def.Name, "result", res.Result, "exit", exit.String())
	in.publishJob(jobEvent(r.def.Name, res))
}

// stopRun stops r's process with its stop signal and stop timeout; a
// process that does not end then is waited for: it stays owned (review
// C6). The executor sends SIGKILL at the stop timeout.
func (in *instance) stopRun(r *jobRun) executor.Exit {
	ctx, cancel := context.WithTimeout(context.Background(), r.def.Program.StopTimeout+2*killWait)
	defer cancel()
	exit, err := r.proc.Stop(ctx, r.def.Program.StopSignal, r.def.Program.StopTimeout)
	if err == nil {
		return exit
	}
	in.log.Error("supervisor job did not end when stopped; it stays owned until it ends", "job", r.def.Name, "pid", r.pid, "error", err)
	<-r.proc.Done()
	exit, _ = r.proc.Result()
	return exit
}

// cleanupRun reports whether nothing of r's group is alive any more: the
// executor kills the group when its leader ends, but a member may
// survive that, so the group is checked like a previous run's.
func (in *instance) cleanupRun(r *jobRun) bool {
	if r.group.IsZero() {
		return true
	}
	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()
	if err := in.launcher.Cleanup(ctx, r.group); err != nil {
		in.log.Error("supervisor job ended but members of its group survive; the record is kept for the next start",
			"job", r.def.Name, "pgid", r.group.Pgid, "error", err)
		return false
	}
	return true
}

// endCanceled ends a reserved run canceled at its launch, which never ran.
// A slot that returns stays due and reports nothing: it is run later.
func (in *instance) endCanceled(ctx context.Context, a *runAdmission) {
	if a.returnsSlot(failureCanceled) {
		in.dropReservation(ctx, a, failureCanceled)
		return
	}
	in.runOutcome(ctx, a, audit.ResultFailed, map[string]string{"failure": failureCanceled})
	in.endUnlaunched(a.def, a.id, a.slot, failureCanceled)
}

// endUnlaunched ends the reserved run id that never ran: its record goes,
// the failure is counted, saved and published.
func (in *instance) endUnlaunched(def Job, id string, slot time.Time, failure string) {
	res := runResult{RunID: id, Scheduled: slot, Ended: in.rt.Clock.Now(), Result: resultFailed, Failure: failure, Code: -1}
	in.mu.Lock()
	delete(in.runRecs, id)
	in.recordResultLocked(def.Name, res)
	in.gen++
	gen := in.gen
	in.mu.Unlock()
	in.save(gen)
	in.publishJob(jobEvent(def.Name, res))
}

// recordResultLocked counts res for the job name if it is still
// configured; the caller holds in.mu.
func (in *instance) recordResultLocked(name string, res runResult) {
	j := in.jobByName[name]
	if j == nil { // removed while it ran: its counters went with it
		return
	}
	switch res.Result {
	case resultSucceeded:
		j.st.Succeeded++
	case resultTimedOut:
		j.st.TimedOut++
	default:
		j.st.Failed++
	}
	j.st.Last = &res
}

// stopRuns stops every run the module owns, in parallel, and waits for
// them until ctx ends; it returns how many did not end. A run launched
// once they are collected stops itself (launchRun).
func (in *instance) stopRuns(ctx context.Context) int {
	in.mu.Lock()
	in.runsCollected = true
	runs := make([]*jobRun, 0, len(in.runs))
	for _, r := range in.runs {
		runs = append(runs, r)
	}
	in.mu.Unlock()
	for _, r := range runs {
		r.requestStop()
	}
	left := 0
	for _, r := range runs {
		select {
		case <-r.done:
		case <-ctx.Done():
			left++
		}
	}
	return left
}

// recoverRuns settles the run records a previous sentineld left (Q14): a
// run that was reserved or running is abandoned, its group killed if it
// is still there; a run whose group could not be confirmed empty is
// cleaned up again. A record whose group cannot be cleaned up stays and
// keeps its job's slots skipped.
func (in *instance) recoverRuns(ctx context.Context, ids []string) {
	for _, id := range ids {
		if ctx.Err() != nil {
			return
		}
		in.mu.Lock()
		rec, ok := in.runRecs[id]
		in.mu.Unlock()
		if !ok {
			continue
		}
		cleaned := true
		if rec.Group != (groupRecord{}) {
			cctx, cancel := context.WithTimeout(ctx, cleanupTimeout)
			err := in.launcher.Cleanup(cctx, rec.Group.group())
			cancel()
			in.auditCleanup(ctx, rec.Job, rec.Group.Pgid, err == nil)
			cleaned = err == nil
			if err != nil {
				in.log.Error("supervisor could not clean up the process group of a job's run a previous sentineld left",
					"job", rec.Job, "pgid", rec.Group.Pgid, "error", err)
			}
		}
		res := runResult{RunID: id, Scheduled: rec.Scheduled, Started: rec.Started, Ended: in.rt.Clock.Now(),
			Result: resultFailed, Failure: failureAbandon, Code: -1}
		report := rec.Phase != runCleanupPending // a cleanup_pending run was reported already
		in.mu.Lock()
		switch {
		case cleaned:
			delete(in.runRecs, id)
		case report:
			rec.Phase = runCleanupPending
			in.runRecs[id] = rec
		}
		if report {
			in.recordResultLocked(rec.Job, res)
		}
		in.gen++
		gen := in.gen
		in.mu.Unlock()
		in.save(gen)
		if report {
			in.log.Warn("supervisor found a run of the job that a previous sentineld left; it is abandoned", "job", rec.Job, "run_id", id)
			in.publishJob(jobEvent(rec.Job, res))
		}
	}
}

// jobEvent is the event of how a run of the job name ended (Q8): fixed
// attributes, never the program, its arguments, environment or output.
func jobEvent(name string, res runResult) model.Event {
	attrs := map[string]any{"job": name, "run_id": res.RunID, "scheduled": res.Scheduled.UTC().Format(time.RFC3339)}
	if !res.Started.IsZero() {
		attrs["started"] = res.Started.UTC().Format(time.RFC3339)
	}
	if !res.Started.IsZero() && res.Failure != failureAbandon { // how an abandoned run ended is not known
		attrs["duration_ms"] = res.Ended.Sub(res.Started).Milliseconds()
		if res.Signal != "" {
			attrs["signal"] = res.Signal
		} else {
			attrs["exit_code"] = res.Code
		}
	}
	if res.Failure != "" {
		attrs["failure"] = res.Failure
	}
	typ, msg := EventJobFailed, fmt.Sprintf("job %s failed (%s)", name, res.Failure)
	switch res.Result {
	case resultSucceeded:
		typ, msg = EventJobSucceeded, fmt.Sprintf("job %s succeeded", name)
	case resultTimedOut:
		typ, msg = EventJobTimeout, fmt.Sprintf("job %s was stopped at its timeout", name)
	}
	return model.Event{CorrelationID: res.RunID, Source: name, SourceType: model.SourceType(typeCron), Type: typ,
		Message: msg, Attributes: attrs}
}

// publishJob publishes e, logging a failure.
func (in *instance) publishJob(e model.Event) {
	if _, err := in.rt.Events.Publish(e); err != nil {
		in.log.Error("supervisor event not published", "job", e.Source, "event_type", e.Type, "error", err)
	}
}
