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
	"github.com/sentinel-watchdog/sentinel-watchdog/pkg/model"
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
)

// Failure codes of a run (Q8), in its job_failed event and status;
// failureMissed, failureNotArmed and failureNotDue only answer an audited
// intent whose launch was given up (review R4).
const (
	failureExitCode = "exit_code"
	failureSignal   = "signal"
	failureLaunch   = "launch_failed"
	failureAbandon  = "abandoned"
	failureMissed   = "missed"
	failureNotArmed = "not_armed"
	failureNotDue   = "not_due"
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
	nextRun  time.Time // the next slot the loop waits for
	blocked  string    // why the last admission was refused, "" when it was not
	skipping bool      // the last slot was skipped: the next skip is not logged again
	ahead    bool      // the watermark is ahead of the clock (F9): shown, logged once
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
		in.mu.Lock()
		j.nextRun = slot
		in.mu.Unlock()
		switch {
		case slot.After(now):
			if !in.sleep(ctx, min(slot.Sub(now), jobWake)) {
				return
			}
		case now.Sub(slot) > jobOnTime:
			in.missSlots(j, slot, now)
		case in.active(j.def.Name):
			if in.retryPending(ctx, j.def.Name) && !in.active(j.def.Name) {
				continue // the group is gone now: decide the slot again
			}
			in.skipSlot(j, slot)
		case !in.admit(ctx, j, slot):
			if !in.sleep(ctx, jobRetry) {
				return
			}
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

// skipSlot consumes slot without a run: a run of the job is still active
// (forbid, D-018) or its record is not cleared yet. Logged and counted, no
// event (Q6).
func (in *instance) skipSlot(j *job, slot time.Time) {
	in.mu.Lock()
	if slot.After(j.st.Through) {
		j.st.Through = slot
	}
	j.st.Skipped++
	first := !j.skipping // one warning per streak (review 3c-4a R6)
	j.skipping = true
	in.gen++
	gen := in.gen
	in.mu.Unlock()
	in.save(gen)
	if first {
		in.log.Warn("supervisor skips the slots of the job while its previous run is still active", "job", j.def.Name, "scheduled", slot)
	}
}

// retryPending cleans up again the groups of the job name's runs that
// ended without their group confirmed empty, and forgets the records it
// cleaned; it reports whether it cleaned one.
func (in *instance) retryPending(ctx context.Context, name string) bool {
	in.mu.Lock()
	pending := map[string]groupRecord{}
	for id, rec := range in.runRecs {
		if rec.Job == name && rec.Phase == runCleanupPending {
			pending[id] = rec.Group
		}
	}
	in.mu.Unlock()
	cleaned := false
	for id, g := range pending {
		cctx, cancel := context.WithTimeout(ctx, cleanupTimeout)
		err := in.launcher.Cleanup(cctx, g.group())
		cancel()
		if err != nil {
			continue
		}
		in.auditCleanup(ctx, name, g.Pgid, true)
		in.mu.Lock()
		delete(in.runRecs, id)
		in.gen++
		gen := in.gen
		in.mu.Unlock()
		in.save(gen)
		cleaned = true
	}
	return cleaned
}

// active reports whether a run of the job name is not over: running, or
// recorded (reserved, or its group not confirmed empty), whatever
// definition it was launched under (Q12, review C3); or whether a
// process service of that name, removed or replaced by the job, still
// owns a process (G7).
func (in *instance) active(name string) bool {
	in.mu.Lock()
	defer in.mu.Unlock()
	if _, ok := in.groups[name]; ok || in.retired[name] != nil { // a service of that name still owns a process
		return true
	}
	if in.retiredJobs[name] != nil { // a stuck loop of an older definition may still launch
		return true
	}
	for _, r := range in.runRecs {
		if r.Job == name {
			return true
		}
	}
	for _, r := range in.runs {
		if r.def.Name == name {
			return true
		}
	}
	return false
}

// admit launches the run of j for slot: the intent in the audit log, then
// the reservation saved with the slot consumed, then the eligibility
// checked again, then the launch (Q18, review C6, R4). It returns false
// when the audit log, the state file or the record cap refused it: the
// caller retries while the slot is on time (Q-B).
func (in *instance) admit(ctx context.Context, j *job, slot time.Time) bool {
	def := j.def
	id := model.NewEventID()
	if err := in.auditRun(ctx, def.Name, id, slot, audit.ResultIntent, nil); err != nil {
		if ctx.Err() == nil {
			in.log.Error("supervisor cannot record the run in the audit log; the job is not run", "job", def.Name, "error", err)
			in.setJobBlocked(j, blockedAudit)
		}
		return false
	}
	in.mu.Lock()
	if len(in.groups)+len(in.runRecs)+in.reserved >= maxGroups {
		in.mu.Unlock()
		in.log.Error("supervisor keeps too many process group and run records; the job is not run", "job", def.Name)
		in.runOutcome(ctx, def.Name, id, slot, audit.ResultFailed, map[string]string{"failure": blockedRecords})
		in.setJobBlocked(j, blockedRecords)
		return false
	}
	prev := j.st
	j.st.Through = slot
	j.st.Runs++
	in.runRecs[id] = runRecord{Job: def.Name, Phase: runReserved, Scheduled: slot}
	in.gen++
	gen := in.gen
	in.mu.Unlock()
	if err := in.write(gen); err != nil {
		in.mu.Lock()
		delete(in.runRecs, id)
		j.st.Through, j.st.Runs = prev.Through, prev.Runs
		in.gen++ // the file may hold the reservation (a failed directory sync): write the rollback (review 3c-4a R6)
		gen = in.gen
		in.mu.Unlock()
		in.save(gen)
		in.log.Error("supervisor cannot save the run of the job; it is not run", "job", def.Name, "error", err)
		in.runOutcome(ctx, def.Name, id, slot, audit.ResultFailed, map[string]string{"failure": blockedState})
		in.setJobBlocked(j, blockedState)
		return false
	}
	now := in.rt.Clock.Now()
	in.mu.Lock()
	why := ""
	switch {
	case !in.processes:
		why = failureNotArmed
	case j.ending || in.runsCollected:
		why = failureCanceled
	case now.Sub(slot) > jobOnTime: // the audit log or the save took the slot's minute
		why = failureMissed
	case slot.After(now): // the clock went back meanwhile: the slot is not due (review 3c-4a R7)
		why = failureNotDue
	}
	if why != "" {
		delete(in.runRecs, id)
		j.st.Runs--
		if why == failureNotDue {
			j.st.Through = prev.Through
		}
		if why == failureMissed {
			j.st.Missed++
			j.st.LastMissed = &missedRecord{From: slot, To: slot}
		}
		in.gen++
		gen = in.gen
	}
	j.blocked, j.skipping = "", false
	in.mu.Unlock()
	if why != "" {
		in.save(gen)
		in.runOutcome(ctx, def.Name, id, slot, audit.ResultFailed, map[string]string{"failure": why})
		return true
	}
	in.launchRun(j, def, id, slot)
	return true
}

func (in *instance) setJobBlocked(j *job, why string) {
	in.mu.Lock()
	j.blocked = why
	in.mu.Unlock()
}

// auditRun records a launch of the job name, bounded by auditTimeout: the
// run id is its correlation id. It names the job, never the program, its
// arguments or its environment.
func (in *instance) auditRun(ctx context.Context, name, id string, slot time.Time, result audit.Result, extra map[string]string) error {
	detail := map[string]string{"trigger": "schedule", "scheduled": slot.UTC().Format(time.RFC3339)}
	for k, v := range extra {
		detail[k] = v
	}
	ctx, cancel := context.WithTimeout(ctx, auditTimeout)
	defer cancel()
	return in.rt.Audit.Record(ctx, module.AuditEntry{Action: actionJobRun, Target: name, Result: result,
		CorrelationID: id, Detail: detail})
}

// runOutcome answers the intent of run id, also while the module stops
// (C5 of 3b): an intent must not stay unanswered.
func (in *instance) runOutcome(ctx context.Context, name, id string, slot time.Time, result audit.Result, detail map[string]string) {
	if err := in.auditRun(context.WithoutCancel(ctx), name, id, slot, result, detail); err != nil {
		in.log.Error("supervisor could not record the outcome of a job's launch in the audit log", "job", name, "error", err)
	}
}

// pidDetail is the audit detail of a launched run.
func pidDetail(pid int) map[string]string { return map[string]string{"pid": strconv.Itoa(pid)} }

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
	for _, j := range in.jobs {
		if in.processes && !j.def.Disabled {
			in.startJob(ctx, j)
		}
	}
}

// classifyJobsLocked compares the jobs of nx with the running ones: a job
// whose digest, enabled flag and arming are unchanged keeps its loop and
// state (its route is swapped with every route); any other configured one
// gets a new loop, and the loops of changed and removed jobs end. A job
// that a stuck loop kept from starting is started again (review 3c-4a R3).
// idle names the jobs whose loop did not run (disabled or not armed): they
// start from now, not from where they stopped (review S7). The caller
// holds in.mu; it runs before in.processes takes nx's value.
func (in *instance) classifyJobsLocked(nx *instance) (start, end []*job, idle map[string]bool) {
	idle = map[string]bool{}
	for _, j := range nx.jobs {
		cur := in.jobByName[j.def.Name]
		if cur != nil && cur.digest == j.digest && cur.def.Disabled == j.def.Disabled && in.processes == nx.processes &&
			cur.blocked != blockedReload {
			continue
		}
		start = append(start, j)
		if cur != nil {
			end = append(end, cur)
			if cur.def.Disabled || !in.processes {
				idle[j.def.Name] = true
			}
		}
	}
	for _, cur := range in.jobs {
		if _, ok := nx.jobByName[cur.def.Name]; !ok {
			end = append(end, cur)
		}
	}
	return start, end, idle
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
// or idle one starts from now, never before its watermark: Q3, C2).
// Programs disarmed stop every run (Q13). It returns an error while a
// stuck loop keeps a name, or while a run has not stopped for the
// disarming: the next reload looks again.
func (in *instance) reconfigureJobs(ctx context.Context, nx *instance, start []*job, idle map[string]bool) error {
	now := in.rt.Clock.Now()
	in.mu.Lock()
	old := &fileState{Jobs: map[string]jobState{}}
	for _, j := range in.jobs {
		old.Jobs[j.def.Name] = j.st
	}
	reconcileJobs(old, start, now)
	for _, j := range start {
		if idle[j.def.Name] && j.st.Through.Before(now) {
			j.st.Through = now
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
	in.mu.Lock()
	stuck := slices.Sorted(maps.Keys(in.retiredJobs))
	in.mu.Unlock()
	for _, j := range start {
		if slices.Contains(stuck, j.def.Name) {
			in.setJobBlocked(j, blockedReload)
			continue
		}
		if in.processes && !j.def.Disabled {
			in.startJob(in.life, j)
		}
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

// jobRefusal refuses an operator's action on the job name: a job runs on
// its schedule; enable and disable come in 3c-4b (Q9, Q15).
func jobRefusal(name string, a action) *api.Error {
	if a == actEnable || a == actDisable {
		return &api.Error{Code: api.CodeUnsupported, Message: fmt.Sprintf("%s is a job: %s of jobs is planned (3c-4b)", name, a)}
	}
	return &api.Error{Code: api.CodeUnsupported, Message: fmt.Sprintf("%s is a job: it runs on its schedule; %s applies to services", name, a)}
}

// jobPhaseLocked is how j shows in status; the caller holds in.mu.
func (in *instance) jobPhaseLocked(j *job) string {
	switch {
	case j.def.Disabled:
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
	if j.def.Disabled || !in.processes {
		return time.Time{}
	}
	return j.nextRun
}

// jobStatusLocked is the status of j; the caller holds in.mu.
func (in *instance) jobStatusLocked(j *job) api.JobStatus {
	st := j.st
	out := api.JobStatus{Name: j.def.Name, Schedule: j.def.Expr, Timezone: j.def.zoneName(), Timeout: j.def.Timeout.String(),
		Phase: in.jobPhaseLocked(j), BlockedReason: j.blocked, NextRun: in.nextRunLocked(j), ActiveRuns: []api.JobRun{},
		Watermark: st.Through,
		Runs:      st.Runs, Succeeded: st.Succeeded, Failed: st.Failed, TimedOut: st.TimedOut, Skipped: st.Skipped, Missed: st.Missed}
	if st.LastMissed != nil {
		out.LastMissedFrom, out.LastMissedTo = st.LastMissed.From, st.LastMissed.To
	}
	if j.ahead && out.BlockedReason == "" {
		out.BlockedReason = blockedClockBehind
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
