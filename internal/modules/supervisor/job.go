package supervisor

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/audit"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/module"
	"github.com/sentinel-watchdog/sentinel-watchdog/pkg/api"
)

const (
	// jobOnTime is how late a slot may still be launched; a later one is
	// missed (Q4). Inclusive: a slot exactly a minute late runs.
	jobOnTime = time.Minute
	// jobWake bounds every sleep of a job's loop: a clock set back or
	// forward is seen within it (phase decision C8).
	jobWake = time.Minute
	// jobRetry is the pause after an admission the audit log, the state
	// file or the record cap refused: the slot is tried again while it is
	// on time, then missed (decision 3c-4a Q-B).
	jobRetry = 10 * time.Second

	// actionJobRun is the audit action of a job's launch (Q18).
	actionJobRun = "supervisor.job_run"
	// blockedRecords: the state file holds as many process group and run
	// records as it may.
	blockedRecords = "records_full"
	// blockedClockBehind: the clock is behind the job's watermark (F9).
	blockedClockBehind = "clock_behind_watermark"
	// blockedRunsFull: the last slot of an allow job found
	// settings.max_concurrent_runs reached; a reason, not a phase (A6).
	blockedRunsFull = "runs_full"
)

// Failure codes of a run (Q8), in its job_failed event and status;
// failureMissed, failureNotDue and failureStopFailed only answer an
// audited intent whose launch was given up (review R4).
const (
	failureExitCode = "exit_code"
	failureSignal   = "signal"
	failureLaunch   = "launch_failed"
	failureAbandon  = "abandoned"
	failureMissed   = "missed"
	failureNotDue   = "not_due"
	// failureReplaced: the run was stopped for the next slot's (Q3).
	failureReplaced = "replaced"
	// failureStopFailed: what replace stops did not end, or its group was
	// not confirmed empty: nothing is launched (Q1, Q17).
	failureStopFailed = "replace_stop_failed"
)

// phaseScheduled: a job waits for its next slot (its other phases are the
// process services' not_armed, disabled, blocked and running).
const phaseScheduled = "scheduled"

// job is one configured job and what the module knows about it.
type job struct {
	def    Job
	digest string

	// Guarded by instance.mu.
	st       jobState
	nextRun  time.Time  // the next slot the loop waits for
	blocked  string     // why the last admission was refused, "" when it was not
	skipping slotAction // what skipped the last slot: the same again is not logged
	ahead    bool       // the watermark is ahead of the clock (F9): shown, logged once
	runsFull bool       // the last slot found settings.max_concurrent_runs reached: shown
	catchUp  bool       // run_once: the next slot is late, so a catch-up is due: shown
	waitLog  bool       // the catch-up's wait has been logged
	// The loop's lifetime inside the module's (as a service's, D-088):
	// cancel ends it, done is closed when it has returned; ending is set
	// once a reload ends it. Its runs are the module's, not the loop's:
	// they outlive it (Q7, Q12).
	cancel context.CancelFunc
	done   chan struct{}
	ending bool
}

// startJob starts the loop of j in its own lifetime inside ctx.
func (in *instance) startJob(ctx context.Context, j *job) {
	ctx, cancel := context.WithCancel(ctx)
	in.mu.Lock()
	j.cancel, j.done, j.ending = cancel, make(chan struct{}), false
	done := j.done
	in.mu.Unlock()
	in.workers.Add(1)
	go func() {
		defer in.workers.Done()
		defer close(done)
		defer cancel()
		in.runJob(ctx, j)
	}()
}

// endJob ends the loop of j and waits for it; false when ctx ended first.
// Its runs go on.
func (in *instance) endJob(ctx context.Context, j *job) bool {
	in.mu.Lock()
	j.ending = true
	cancel, done := j.cancel, j.done
	in.mu.Unlock()
	if cancel == nil { // never started: disabled or not armed
		return true
	}
	cancel()
	select {
	case <-done:
		return true
	case <-ctx.Done():
		return false
	}
}

// runJob is the loop of one job: it sleeps until the next slot, at most
// jobWake, and decides each slot that is due. A panic in the module's own
// code is logged without its value; the runs already launched go on.
func (in *instance) runJob(ctx context.Context, j *job) {
	defer func() {
		if v := recover(); v != nil {
			in.log.Error("supervisor job loop failed; the job is not scheduled until the next reload or restart", "job", j.def.Name)
		}
	}()
	for ctx.Err() == nil {
		now := in.rt.Clock.Now()
		in.mu.Lock()
		through := j.st.Through
		ahead := through.After(now.Add(jobWake)) // the clock is behind the slots already consumed
		warn := ahead && !j.ahead
		j.ahead = ahead
		in.mu.Unlock()
		if warn {
			in.log.Warn("supervisor job waits: the clock is behind the slots it already consumed; "+
				"a watermark more than a day ahead is reset at the next reload or start", "job", j.def.Name, "watermark", through)
		}
		slot, err := j.def.Schedule.Next(through, j.def.Location)
		if err != nil { // cannot happen after Parse (D-096): only zone data without a run
			in.log.Error("supervisor finds no run time for the job", "job", j.def.Name, "error", err)
			if !in.sleep(ctx, jobWake) {
				return
			}
			continue
		}
		late := now.Sub(slot) > jobOnTime
		in.mu.Lock()
		j.nextRun = slot
		j.catchUp = late && j.def.Missed == missedRunOnce
		j.waitLog = j.waitLog && j.catchUp
		in.mu.Unlock()
		switch {
		case slot.After(now):
			if !in.sleep(ctx, min(slot.Sub(now), jobWake)) {
				return
			}
		case late && j.def.Missed == missedSkip:
			in.missSlots(j, slot, now)
		case late:
			if !in.lateSlot(ctx, j, slot, now) && !in.sleep(ctx, jobRetry) {
				return
			}
		case !in.dueSlot(ctx, j, slot) && !in.sleep(ctx, jobRetry):
			return
		}
	}
}

// missSlots consumes every slot older than a minute, from first on: they
// are missed (Q4, missed_runs: skip). The slot inside the last minute, if
// any, is still on time (review C1). One catch-up counts once; status
// shows the interval (decision 3c-4a Q-A).
func (in *instance) missSlots(j *job, first, now time.Time) {
	cut := now.Add(-jobOnTime) // a slot at cut is exactly a minute late: on time
	through := cut.Add(-time.Nanosecond)
	if first.After(through) { // never short of the slot decided: the loop always moves on
		through = first
	}
	in.mu.Lock()
	if through.After(j.st.Through) {
		j.st.Through = through
	}
	j.st.Missed++
	j.st.LastMissed = &missedRecord{From: first, To: cut}
	in.gen++
	gen := in.gen
	in.mu.Unlock()
	in.save(gen)
	in.log.Warn("supervisor job missed its slots: sentineld was not running, or the clock jumped forward; they are not run",
		"job", j.def.Name, "from", first, "to", cut)
}

// retryPending cleans up again the groups of the job name's runs that
// ended without their group confirmed empty, and forgets the records it
// cleaned. The batch shares one cleanupTimeout: many records must not use
// up the slot's minute (review R3); the ones left are tried at the next
// slot.
func (in *instance) retryPending(ctx context.Context, name string) {
	in.mu.Lock()
	pending := map[string]groupRecord{}
	for id, rec := range in.runRecs {
		if rec.Job == name && rec.Phase == runCleanupPending {
			pending[id] = rec.Group
		}
	}
	in.mu.Unlock()
	cctx, cancel := context.WithTimeout(ctx, cleanupTimeout)
	defer cancel()
	for id, g := range pending {
		if cctx.Err() != nil {
			return
		}
		if in.launcher.Cleanup(cctx, g.group()) != nil {
			continue
		}
		in.auditCleanup(ctx, name, g.Pgid, true)
		in.mu.Lock()
		delete(in.runRecs, id)
		in.gen++
		gen := in.gen
		in.mu.Unlock()
		in.save(gen)
	}
}

func (in *instance) setJobBlocked(j *job, why string) {
	in.mu.Lock()
	j.blocked = why
	in.mu.Unlock()
}

// auditRun records a launch of the job name, bounded by auditTimeout: the
// run id is its correlation id. It names the job, never the program, its
// arguments or its environment.
func (in *instance) auditRun(ctx context.Context, a *runAdmission, result audit.Result, extra map[string]string) error {
	detail := map[string]string{"trigger": a.trigger, "scheduled": a.slot.UTC().Format(time.RFC3339)}
	for k, v := range extra {
		detail[k] = v
	}
	ctx, cancel := context.WithTimeout(ctx, auditTimeout)
	defer cancel()
	return in.rt.Audit.Record(ctx, module.AuditEntry{Action: actionJobRun, Target: a.def.Name, Result: result,
		CorrelationID: a.id, Detail: detail})
}

// runOutcome answers the intent of a, also while the module stops (C5 of
// 3b): an intent must not stay unanswered.
func (in *instance) runOutcome(ctx context.Context, a *runAdmission, result audit.Result, detail map[string]string) {
	if err := in.auditRun(context.WithoutCancel(ctx), a, result, detail); err != nil {
		in.log.Error("supervisor could not record the outcome of a job's launch in the audit log", "job", a.def.Name, "error", err)
	}
}

// pidDetail is the audit detail of a launched run.
func pidDetail(pid int) map[string]string { return map[string]string{"pid": strconv.Itoa(pid)} }

// runnable reports whether a loop may run for j: nothing keeps it idle
// (Q10). The caller holds in.mu; armed is the central switch.
func (j *job) runnable(armed bool) bool { return armed && !j.def.Disabled && !j.st.Disabled }

// startJobs settles the run records a previous sentineld left (orphans,
// in their own goroutine) and starts the loop of every job that may run:
// enabled, with programs armed (AC2). A loop skips its slots while a
// record of its name is not settled.
func (in *instance) startJobs(ctx context.Context, orphans []string) {
	if len(orphans) > 0 {
		in.workers.Add(1)
		go func() {
			defer in.workers.Done()
			in.recoverRuns(ctx, orphans)
		}()
	}
	in.mu.Lock()
	jobs := slices.DeleteFunc(slices.Clone(in.jobs), func(j *job) bool { return !j.runnable(in.processes) })
	in.mu.Unlock()
	for _, j := range jobs {
		in.startJob(ctx, j)
	}
}

// classifyJobsLocked compares the jobs of nx with the running ones: a job
// whose digest, enabled flag and arming are unchanged keeps its loop and
// state (its route is swapped with every route); any other configured one
// gets a new loop, and the loops of changed and removed jobs end. A job
// that a stuck loop kept from starting is started again (review 3c-4a R3).
// The operator's flag is not compared: a job it holds has no loop, and a
// reload does not change it. The caller holds in.mu; it runs before
// in.processes takes nx's value.
func (in *instance) classifyJobsLocked(nx *instance) (start, end []*job) {
	for _, j := range nx.jobs {
		cur := in.jobByName[j.def.Name]
		if cur != nil && cur.digest == j.digest && cur.def.Disabled == j.def.Disabled && in.processes == nx.processes &&
			cur.blocked != blockedReload {
			continue
		}
		start = append(start, j)
		if cur != nil {
			end = append(end, cur)
		}
	}
	for _, cur := range in.jobs {
		if _, ok := nx.jobByName[cur.def.Name]; !ok {
			end = append(end, cur)
		}
	}
	return start, end
}

// pruneRetiredJobsLocked forgets the stuck loops that have ended since.
func (in *instance) pruneRetiredJobsLocked() {
	for name, j := range in.retiredJobs {
		select {
		case <-j.done:
			delete(in.retiredJobs, name)
		default:
		}
	}
}

// endJobs ends the loops of end before anything new starts (review
// 3c-4a R2): the runs go on (Q7, Q12). A loop that does not end within ctx
// is kept by name: its name takes no new loop until it has ended.
func (in *instance) endJobs(ctx context.Context, end []*job) {
	for _, j := range end {
		if !in.endJob(ctx, j) {
			in.mu.Lock()
			in.retiredJobs[j.def.Name] = j
			in.mu.Unlock()
		}
	}
}

// jobsAfter is the list of jobs once start replaces its names.
func (in *instance) jobsAfter(nx *instance, start []*job) []*job {
	started := map[string]*job{}
	for _, j := range start {
		started[j.def.Name] = j
	}
	out := make([]*job, 0, len(nx.jobs))
	for _, j := range nx.jobs {
		if s := started[j.def.Name]; s != nil {
			out = append(out, s)
		} else {
			out = append(out, in.jobByName[j.def.Name])
		}
	}
	return out
}

// reconfigureJobs applies the rest of a reload to the jobs (the reload
// table of decision 3c-4a), once the loops that go have ended: the jobs of
// start begin from their state (the same digest keeps it; a new, changed
// or idle one starts from now, never before its watermark: Q3, C2). A job
// a stuck loop keeps waiting is idle meanwhile. Programs disarmed stop
// every run (Q13). It returns an error while a stuck loop keeps a name, or
// while a run has not stopped for the disarming: the next reload looks
// again.
func (in *instance) reconfigureJobs(ctx context.Context, nx *instance, start []*job) error {
	now := in.rt.Clock.Now()
	in.mu.Lock()
	old := &fileState{Jobs: map[string]jobState{}}
	for _, j := range in.jobs {
		old.Jobs[j.def.Name] = j.st
	}
	reconcileJobs(old, start, now, nx.processes)
	stuck := slices.Sorted(maps.Keys(in.retiredJobs))
	var launch []*job
	for _, j := range start {
		switch {
		case slices.Contains(stuck, j.def.Name):
			j.blocked, j.st.Idle = blockedReload, true
		case j.runnable(nx.processes):
			launch = append(launch, j)
		}
	}
	jobs := in.jobsAfter(nx, start)
	reset := resetFarWatermarks(jobs, now) // a loop that keeps running reads its watermark at each wake
	in.jobs = jobs
	in.jobByName = make(map[string]*job, len(jobs))
	for _, j := range jobs {
		in.jobByName[j.def.Name] = j
	}
	var runs []*jobRun
	if !nx.processes { // every reload while disarmed: a run not stopped yet is still reported (re-review R2)
		for _, r := range in.runs {
			runs = append(runs, r)
		}
	}
	in.gen++
	gen := in.gen
	in.mu.Unlock()
	in.save(gen)
	if len(reset) > 0 {
		in.log.Warn("supervisor reset the watermark of jobs that was more than a day ahead of the clock: the clock was wrong forward; a slot of that day may run again",
			"jobs", reset)
	}
	for _, r := range runs {
		r.requestStop()
	}
	unfinished := 0
	for _, r := range runs {
		select {
		case <-r.done:
		case <-ctx.Done():
			unfinished++
		}
	}
	for _, j := range launch {
		in.startJob(in.life, j)
	}
	var errs []error
	if len(stuck) > 0 {
		errs = append(errs, fmt.Errorf("supervisor: jobs not replaced, their loop did not end: %s", strings.Join(stuck, ", ")))
	}
	if unfinished > 0 {
		errs = append(errs, fmt.Errorf("supervisor: %d job runs did not stop for modules.supervisor.processes: false", unfinished))
	}
	return errors.Join(errs...)
}

// jobOwnsLocked reports whether a run of a job named name is not over;
// the caller holds in.mu.
func (in *instance) jobOwnsLocked(name string) bool {
	for _, r := range in.runRecs {
		if r.Job == name {
			return true
		}
	}
	return false
}

// jobRefusal says why a is refused on j now, nil when it may go on: a job
// runs on its schedule, so only enable and disable apply (Q9, Q10). A loop
// that did not end for an earlier action or reload may still launch, so its
// name waits for it. The caller holds in.mu.
func (in *instance) jobRefusal(j *job, a action) *api.Error {
	name := j.def.Name
	switch {
	case a != actEnable && a != actDisable:
		return &api.Error{Code: api.CodeUnsupported, Message: fmt.Sprintf("%s is a job: it runs on its schedule; %s applies to services", name, a)}
	case in.retiredJobs[name] != nil:
		return conflict("job %s waits: its loop has not ended yet; try again", name)
	case a == actEnable && j.def.Disabled:
		return errConfigDisabled("job", name)
	}
	return nil
}

// carryJob carries action a out on j, holding ops; request is the
// correlation id of the operator's request.
func (in *instance) carryJob(ctx context.Context, j *job, a action, request string) actionResult {
	if a == actDisable {
		return in.disableJob(ctx, j, request)
	}
	return in.enableJob(j, request)
}

// disableJob holds j from now on, persisted (Q10): the flag and j.ending
// are set together, so no launch begins past this point, then saved (a
// change that takes away is applied first), then the loop ends. Its active
// runs finish (3c-4 Q7).
func (in *instance) disableJob(ctx context.Context, j *job, request string) actionResult {
	in.mu.Lock()
	if j.st.Disabled {
		in.mu.Unlock()
		return in.held()
	}
	j.st.Disabled, j.st.Idle, j.ending = true, true, true
	if j.cancel != nil { // now, not after the save: a launch not queued yet gives way
		j.cancel()
	}
	j.blocked, j.skipping, j.runsFull, j.catchUp = "", 0, false, false
	in.gen++
	gen := in.gen
	in.mu.Unlock()
	notSaved := in.write(gen) != nil
	if notSaved {
		in.log.Error("supervisor applied an operator's action but could not save it; a restart of sentineld would not remember it",
			"job", j.def.Name, "action", actDisable)
	}
	if !in.endJob(ctx, j) {
		return in.stuckJob(j, request)
	}
	in.auditAction("job", j.def.Name, actDisable, request, audit.ResultOK)
	return actionResult{changed: true, notSaved: notSaved}
}

// stuckJob reports a loop that did not end for a disable within the
// action's bound. j.ending keeps it from launching anything new; its name
// takes no new loop until it has ended.
func (in *instance) stuckJob(j *job, request string) actionResult {
	if in.life.Err() != nil {
		return actionResult{err: &api.Error{Code: api.CodeUnavailable, Message: "the supervisor is stopping"}}
	}
	in.mu.Lock()
	in.retiredJobs[j.def.Name] = j
	in.mu.Unlock()
	in.log.Error("supervisor job's loop did not end for an operator's action; it launches nothing new", "job", j.def.Name, "action", actDisable)
	in.auditAction("job", j.def.Name, actDisable, request, audit.ResultFailed)
	return actionResult{changed: true, err: &api.Error{Code: api.CodeUnavailable,
		Message: fmt.Sprintf("job %s is disabled, but its loop did not end yet: it launches nothing new (see supervisor status %s)", j.def.Name, j.def.Name)}}
}

// enableJob lifts the hold of j: saved before it applies, as it lets work
// run. It starts from now, under run_once too (Q9, Q10), and it stays idle
// while programs are not armed.
func (in *instance) enableJob(j *job, request string) actionResult {
	in.mu.Lock()
	disabled := j.st.Disabled
	in.mu.Unlock()
	if !disabled {
		return actionResult{}
	}
	now := in.rt.Clock.Now()
	if err := in.commitJob(j, func(st *jobState) {
		st.Disabled = false
		if in.processes {
			st.Idle = false
			if st.Through.Before(now) {
				st.Through = now
			}
		}
	}); err != nil {
		return in.notCommitted("job", j.def.Name, err)
	}
	in.mu.Lock()
	armed := in.processes
	in.mu.Unlock()
	if armed {
		in.startJob(in.life, j)
	}
	in.auditAction("job", j.def.Name, actEnable, request, audit.ResultOK)
	return actionResult{changed: true}
}

// jobPhaseLocked is how j shows in status; the caller holds in.mu.
func (in *instance) jobPhaseLocked(j *job) string {
	switch {
	case j.def.Disabled, j.st.Disabled:
		return phaseDisabled
	case !in.processes:
		return phaseNotArmed
	case j.blocked != "":
		return phaseBlocked
	case in.jobOwnsLocked(j.def.Name):
		return phaseRunning
	}
	return phaseScheduled
}

// nextRunLocked is the slot j's loop waits for, zero when it has none.
func (in *instance) nextRunLocked(j *job) time.Time {
	if !j.runnable(in.processes) {
		return time.Time{}
	}
	return j.nextRun
}

// jobStatusLocked is the status of j; the caller holds in.mu.
func (in *instance) jobStatusLocked(j *job) api.JobStatus {
	st := j.st
	out := api.JobStatus{Name: j.def.Name, Schedule: j.def.Expr, Timezone: j.def.zoneName(), Timeout: j.def.Timeout.String(),
		ConcurrencyPolicy: j.def.Concurrency, MissedRuns: j.def.Missed, CatchUpPending: j.catchUp && j.runnable(in.processes),
		Phase: in.jobPhaseLocked(j), BlockedReason: j.blocked, NextRun: in.nextRunLocked(j), ActiveRuns: []api.JobRun{},
		Watermark: st.Through,
		Runs:      st.Runs, Succeeded: st.Succeeded, Failed: st.Failed, TimedOut: st.TimedOut, Skipped: st.Skipped, Missed: st.Missed}
	if st.LastMissed != nil {
		out.LastMissedFrom, out.LastMissedTo = st.LastMissed.From, st.LastMissed.To
	}
	if j.ahead && out.BlockedReason == "" {
		out.BlockedReason = blockedClockBehind
	}
	if j.runsFull && out.BlockedReason == "" && j.runnable(in.processes) {
		out.BlockedReason = blockedRunsFull
	}
	if l := st.Last; l != nil {
		out.Last = &api.JobRunEnded{RunID: l.RunID, Scheduled: l.Scheduled, Started: l.Started, Ended: l.Ended,
			Result: l.Result, Failure: l.Failure, ExitCode: l.Code, Signal: l.Signal}
	}
	ids := make([]string, 0)
	for id, rec := range in.runRecs {
		if rec.Job == j.def.Name {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	for _, id := range ids {
		rec := in.runRecs[id]
		run := api.JobRun{RunID: id, Scheduled: rec.Scheduled, Started: rec.Started, Phase: rec.Phase}
		if r := in.runs[id]; r != nil {
			run.Pid = r.pid
		}
		out.ActiveRuns = append(out.ActiveRuns, run)
	}
	return out
}
