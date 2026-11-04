package supervisor

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/audit"
	"github.com/sentinel-watchdog/sentinel-watchdog/internal/core/cronexpr"
	"github.com/sentinel-watchdog/sentinel-watchdog/pkg/model"
)

// slotAction is what a due slot of a job calls for.
type slotAction int

const (
	slotLaunch   slotAction = iota + 1
	slotReplace             // replace: stop the runs of the name, then launch
	slotBusy                // forbid: a run of the name is active
	slotHeld                // the name is held by what no policy may touch (Q17, Q18, O3)
	slotRunsFull            // allow: settings.max_concurrent_runs is reached
)

// slotSkipText says why slots are skipped, in the log.
var slotSkipText = map[slotAction]string{
	slotBusy:     "supervisor skips the slots of the job while its previous run is still active",
	slotHeld:     "supervisor skips the slots of the job while a record or a process of its name is not over",
	slotRunsFull: "supervisor skips the slots of the job while settings.max_concurrent_runs runs are active",
}

const (
	// maxReplacesListed is how many run ids a replace's intent names: each
	// takes 33 bytes, and a longer audit value would refuse the record, and
	// with it every launch of the job, after allow → replace (review O2).
	maxReplacesListed = 15
	// replaceSlack is what the wait for a replaced run allows beyond its
	// stop and its group's cleanup: the saves of its end.
	replaceSlack = 10 * time.Second
)

// launches reports whether the slot goes on to an admission.
func (s slotAction) launches() bool { return s == slotLaunch || s == slotReplace }

// The audit trigger of a launch.
const (
	triggerSchedule = "schedule" // a slot launched on time
	triggerCatchUp  = "catch_up" // the latest missed slot, under run_once (Q6)
)

// runAdmission is one slot of a job on its way to a launch.
type runAdmission struct {
	def     Job
	id      string
	slot    time.Time
	trigger string
	action  slotAction
	first   time.Time // catch_up: the first slot missed
	stopped bool      // replace: the stop began, so the slot stays consumed (Q15, Q20)
	claimed bool      // allow: holds a place under the cap, until its record does
	// What the reservation replaced, to give back.
	through    time.Time
	missed     uint64
	lastMissed *missedRecord
}

// dueSlot decides slot, due and on time. It returns false when the audit
// log, the state file or the record cap refused the launch: the caller
// retries while the slot is on time (Q-B).
func (in *instance) dueSlot(ctx context.Context, j *job, slot time.Time) bool {
	in.retryPending(ctx, j.def.Name)
	action := in.claimSlot(j)
	if !action.launches() {
		in.skipSlot(j, slot, action)
		return true
	}
	return in.admit(ctx, j, &runAdmission{def: j.def, id: model.NewEventID(), slot: slot, trigger: triggerSchedule, action: action})
}

// lateSlot decides the slots from first on under run_once, when first is
// more than a minute late. If the latest of them is on time, it stands for
// the others: they are consumed, counted once, and it is decided as usual
// (review A1); that is also how a regular slot absorbs a catch-up still
// waiting (Q7). Otherwise the latest runs as a catch-up. A catch-up that
// cannot start writes nothing, so the wait survives a restart and counts
// once (Q7, decision question 5), and it returns false: the caller retries.
func (in *instance) lateSlot(ctx context.Context, j *job, first, now time.Time) bool {
	latest := lastSlot(j.def.Schedule, j.def.Location, first, now)
	if now.Sub(latest) <= jobOnTime {
		in.absorbSlots(j, first, latest)
		return true
	}
	in.retryPending(ctx, j.def.Name)
	action := in.claimSlot(j)
	if !action.launches() {
		in.mu.Lock()
		logged := j.waitLog
		j.waitLog = true
		in.mu.Unlock()
		if !logged {
			in.log.Warn("supervisor catch-up of the job waits, at most until its next slot: "+slotSkipText[action], "job", j.def.Name, "scheduled", latest)
		}
		return false
	}
	return in.admit(ctx, j, &runAdmission{def: j.def, id: model.NewEventID(), slot: latest, trigger: triggerCatchUp, first: first, action: action})
}

// absorbSlots consumes the slots from first up to latest, not latest
// itself, and counts them as one catch-up.
func (in *instance) absorbSlots(j *job, first, latest time.Time) {
	in.mu.Lock()
	j.st.Through = latest.Add(-time.Nanosecond)
	j.st.Missed++
	j.st.LastMissed = &missedRecord{From: first, To: latest}
	in.gen++
	gen := in.gen
	in.mu.Unlock()
	in.save(gen)
	in.log.Warn("supervisor job missed its slots: sentineld was not running, or the clock jumped forward; the latest is on time and stands for them",
		"job", j.def.Name, "from", first, "to", latest)
}

// lastSlot returns the last run time of s in [first, target], where first
// is a run time and target is not before it. It bisects over Next rather
// than scanning forward, which months of downtime on a minute schedule would
// make slow, and needs no cronexpr.Latest (D-096): Next is the first run
// time after its argument, so it only grows with it. lo is always a run
// time. Under a minute apart there is at most a run time or two left, as
// an offset change with seconds can bring two within a minute: those are
// walked. An error of Next means no run time follows, so nothing fits.
func lastSlot(s *cronexpr.Schedule, loc *time.Location, first, target time.Time) time.Time {
	lo, hi := first, target
	for hi.Sub(lo) >= time.Minute {
		mid := lo.Add(hi.Sub(lo) / 2)
		if n, err := s.Next(mid, loc); err == nil && !n.After(target) {
			lo = n
		} else {
			hi = mid
		}
	}
	for {
		n, err := s.Next(lo, loc)
		if err != nil || n.After(target) {
			return lo
		}
		lo = n
	}
}

// claimSlot decides what a due slot of j calls for and, for a launch under
// allow, takes its place under the cap.
func (in *instance) claimSlot(j *job) slotAction {
	in.mu.Lock()
	defer in.mu.Unlock()
	action := in.decideLocked(j)
	j.runsFull = action == slotRunsFull
	if action == slotLaunch && j.def.Concurrency == concurrencyAllow {
		in.overlapReserved++
	}
	return action
}

// decideLocked is the table of the policies, read by name, whatever
// definition launched the runs (Q11, review C3). What nothing may stop
// comes first; the caller holds in.mu.
func (in *instance) decideLocked(j *job) slotAction {
	name := j.def.Name
	if _, ok := in.groups[name]; ok || in.retired[name] != nil { // a service of that name still owns a process
		return slotHeld
	}
	if in.retiredJobs[name] != nil { // a stuck loop of an older definition may still launch
		return slotHeld
	}
	allow := j.def.Concurrency == concurrencyAllow
	busy, overlapping := false, in.overlapReserved
	for id, rec := range in.runRecs {
		if rec.Overlap {
			overlapping++
		}
		if rec.Job != name || allow {
			continue
		}
		if rec.Phase == runCleanupPending || in.runs[id] == nil { // no process of ours to stop: a previous sentineld's, or not launched yet
			return slotHeld
		}
		busy = true
	}
	switch {
	case busy && j.def.Concurrency == concurrencyReplace:
		return slotReplace
	case busy:
		return slotBusy
	case allow && overlapping >= in.settings.MaxConcurrentRuns:
		return slotRunsFull
	}
	return slotLaunch
}

// skipSlot consumes slot without a run. Logged once per streak and
// counted, no event (Q5, Q6, review 3c-4a R6).
func (in *instance) skipSlot(j *job, slot time.Time, why slotAction) {
	in.mu.Lock()
	if slot.After(j.st.Through) {
		j.st.Through = slot
	}
	j.st.Skipped++
	first := j.skipping != why
	j.skipping = why
	in.gen++
	gen := in.gen
	in.mu.Unlock()
	in.save(gen)
	if first {
		in.log.Warn(slotSkipText[why], "job", j.def.Name, "scheduled", slot)
	}
}

// releaseClaim gives up the place claimSlot took under the cap, if the
// admission still holds it.
func (in *instance) releaseClaim(a *runAdmission) {
	in.mu.Lock()
	defer in.mu.Unlock()
	if a.claimed {
		a.claimed = false
		in.overlapReserved--
	}
}

// admit launches the run of a: the intent in the audit log, then the
// reservation saved with the slot consumed, then the eligibility checked
// again, then the launch (Q18, review C6, R4). It returns false when the
// audit log, the state file or the record cap refused it: nothing was
// consumed, and the caller retries.
func (in *instance) admit(ctx context.Context, j *job, a *runAdmission) bool {
	a.claimed = a.def.Concurrency == concurrencyAllow
	defer in.releaseClaim(a) // on every exit, a panic included: a lost place stays lost until a restart
	if err := in.auditRun(ctx, a, audit.ResultIntent, in.replacesDetail(a)); err != nil {
		if ctx.Err() == nil {
			in.log.Error("supervisor cannot record the run in the audit log; the job is not run", "job", a.def.Name, "error", err)
			in.setJobBlocked(j, blockedAudit)
		}
		return false
	}
	if !in.reserveRun(ctx, j, a) {
		return false
	}
	if why := in.recheck(ctx, j, a); why != "" {
		in.dropReservation(ctx, a, why)
		return why != failureCanceled // under run_once the slot is back: no busy loop while the cancellation lasts
	}
	if a.action == slotReplace {
		if why := in.replaceRuns(ctx, j, a); why != "" {
			in.dropReservation(ctx, a, why)
			return true
		}
	}
	in.launchRun(ctx, j, a)
	return true
}

// replacesDetail names in an intent the runs a replace is about to stop,
// nil for any other launch.
func (in *instance) replacesDetail(a *runAdmission) map[string]string {
	if a.action != slotReplace {
		return nil
	}
	olds := in.runsNamed(a.def.Name)
	ids := make([]string, 0, len(olds))
	for _, r := range olds {
		ids = append(ids, r.id)
	}
	slices.Sort(ids)
	return map[string]string{"replaces": strings.Join(ids[:min(len(ids), maxReplacesListed)], ","), "replaces_count": strconv.Itoa(len(ids))}
}

// runsNamed returns the runs of the job name this instance owns. They are
// the only ones replace may signal: never a pid or a group read from the
// state file.
func (in *instance) runsNamed(name string) []*jobRun {
	in.mu.Lock()
	defer in.mu.Unlock()
	var out []*jobRun
	for _, r := range in.runs {
		if r.def.Name == name {
			out = append(out, r)
		}
	}
	return out
}

// replaceRuns stops the runs of a's name, each with its own stop signal
// (Q11), waits for their ends, and says why a must not launch, "" when it
// may. From the first stop on the slot's age no longer counts: its launch
// stays valid past the minute (Q1).
func (in *instance) replaceRuns(ctx context.Context, j *job, a *runAdmission) string {
	a.stopped = true
	in.stopAndWait(ctx, in.runsNamed(a.def.Name))
	return in.settleStop(ctx, j, a)
}

// stopAndWait asks runs to stop and waits for their ends. The loop waits at
// most what the stops and cleanups may take: a process that never ends
// stays owned by its supervise, and its record says so to settleStop (Q17).
func (in *instance) stopAndWait(ctx context.Context, runs []*jobRun) {
	var bound time.Duration
	for _, r := range runs {
		r.requestStopFor(failureReplaced)
		bound = max(bound, r.def.Program.StopTimeout)
	}
	timer := in.rt.Clock.NewTimer(bound + 2*killWait + cleanupTimeout + replaceSlack)
	defer timer.Stop()
	for _, r := range runs {
		select {
		case <-r.done:
		case <-timer.C():
			return
		case <-ctx.Done():
			return
		}
	}
}

// settleStop says how a stop ended. Cancellation comes first: a canceled
// wait also leaves the old record. Whatever the end, the slots that passed
// are consumed and count once in skipped, not in missed (Q15, review C5).
func (in *instance) settleStop(ctx context.Context, j *job, a *runAdmission) string {
	now := in.rt.Clock.Now()
	passed := a.slot
	if now.After(a.slot) { // a clock set back during the stop consumes nothing more
		passed = lastSlot(a.def.Schedule, a.def.Location, a.slot, now)
	}
	in.mu.Lock()
	why := ""
	switch {
	case in.canceledLocked(ctx, j):
		why = failureCanceled
	case in.othersOfNameLocked(a):
		why = failureStopFailed
		in.log.Error("supervisor does not launch the job: what it replaces has not ended, or its group is not confirmed empty", "job", a.def.Name)
	}
	if cur := in.jobByName[a.def.Name]; cur != nil {
		if passed.After(cur.st.Through) {
			cur.st.Through = passed
		}
		if passed.After(a.slot) || why == failureStopFailed {
			cur.st.Skipped++
		}
	}
	in.gen++
	gen := in.gen
	in.mu.Unlock()
	in.save(gen)
	return why
}

// canceledLocked reports whether a launch of j must give way: the loop was
// ended (a reload, a disable, Stop) or programs were disarmed. The one
// test, at each step of an admission; the caller holds in.mu.
func (in *instance) canceledLocked(ctx context.Context, j *job) bool {
	return ctx.Err() != nil || !in.processes || j.ending || in.runsCollected
}

// othersOfNameLocked reports whether a record of a's job other than its
// own is left; the caller holds in.mu.
func (in *instance) othersOfNameLocked(a *runAdmission) bool {
	for id, rec := range in.runRecs {
		if rec.Job == a.def.Name && id != a.id {
			return true
		}
	}
	return false
}

// reserveRun records the run of a and saves it with its slot consumed; the
// place claimed under the cap becomes the record's. On failure nothing
// stays and the intent is answered. A loop already ended reserves nothing:
// a reload may have copied j's state, and the increment would be lost while
// its undo reached the new job.
func (in *instance) reserveRun(ctx context.Context, j *job, a *runAdmission) bool {
	name := a.def.Name
	in.mu.Lock()
	if in.canceledLocked(ctx, j) {
		in.mu.Unlock()
		in.runOutcome(ctx, a, audit.ResultFailed, map[string]string{"failure": failureCanceled})
		return false
	}
	if len(in.groups)+len(in.runRecs)+in.reserved >= maxGroups {
		in.mu.Unlock()
		in.log.Error("supervisor keeps too many process group and run records; the job is not run", "job", name)
		in.runOutcome(ctx, a, audit.ResultFailed, map[string]string{"failure": blockedRecords})
		in.setJobBlocked(j, blockedRecords)
		return false
	}
	if a.claimed {
		a.claimed = false
		in.overlapReserved--
	}
	a.through, a.missed, a.lastMissed = j.st.Through, j.st.Missed, j.st.LastMissed
	j.st.Through = a.slot
	j.st.Runs++
	if a.trigger == triggerCatchUp {
		j.st.Missed++
		j.st.LastMissed = &missedRecord{From: a.first, To: a.slot}
	}
	in.runRecs[a.id] = runRecord{Job: name, Phase: runReserved, Scheduled: a.slot, Overlap: a.def.Concurrency == concurrencyAllow}
	in.gen++
	gen := in.gen
	in.mu.Unlock()
	if err := in.write(gen); err != nil {
		in.undoReservation(a, true)
		in.log.Error("supervisor cannot save the run of the job; it is not run", "job", name, "error", err)
		in.runOutcome(ctx, a, audit.ResultFailed, map[string]string{"failure": blockedState})
		in.setJobBlocked(j, blockedState)
		return false
	}
	return true
}

// recheck says why the reserved run of a must not launch now, "" when it
// may: what changed while the audit log and the state file were written.
func (in *instance) recheck(ctx context.Context, j *job, a *runAdmission) string {
	now := in.rt.Clock.Now()
	due := false // a catch-up: the next slot came due, and stands for it (review A3)
	if a.trigger == triggerCatchUp {
		n, err := j.def.Schedule.Next(a.slot, j.def.Location)
		due = err == nil && !n.After(now)
	}
	in.mu.Lock()
	defer in.mu.Unlock()
	j.blocked, j.skipping = "", 0
	switch {
	case in.canceledLocked(ctx, j):
		return failureCanceled
	case a.slot.After(now): // the clock went back meanwhile: the slot is not due (review 3c-4a R7)
		return failureNotDue
	case due, a.trigger == triggerSchedule && now.Sub(a.slot) > jobOnTime: // the audit log or the save took the slot's minute
		return failureMissed
	}
	return ""
}

// dropReservation gives up the reserved run of a before its launch and
// answers its intent with why. On time and expired under skip, the slot is
// missed.
func (in *instance) dropReservation(ctx context.Context, a *runAdmission, why string) {
	in.mu.Lock()
	if cur := in.jobByName[a.def.Name]; cur != nil && why == failureMissed && a.trigger == triggerSchedule && a.def.Missed == missedSkip {
		cur.st.Missed++
		cur.st.LastMissed = &missedRecord{From: a.slot, To: a.slot}
	}
	in.mu.Unlock()
	in.undoReservation(a, a.returnsSlot(why))
	in.runOutcome(ctx, a, audit.ResultFailed, map[string]string{"failure": why})
}

// returnsSlot reports whether a dropped reservation gives its slot back.
// Never once a stop began (Q15, Q20). A slot no longer due returns. Under
// run_once so does a canceled one, as a shutdown in that instant must not
// lose what run_once promises, and an expired one, which is then caught up
// (Q8). A catch-up found missed was absorbed by the next slot (review A3).
func (a *runAdmission) returnsSlot(why string) bool {
	switch {
	case a.stopped:
		return false
	case why == failureNotDue:
		return true
	case a.def.Missed != missedRunOnce:
		return false
	}
	return why == failureCanceled || why == failureMissed && a.trigger == triggerSchedule
}

// undoReservation removes the record of a and its launch from the counters
// and, with giveBack, returns its slot, and saves. It goes through the name,
// as recordResultLocked: a reload may have replaced the job, and its state,
// while a stuck loop was saving (review A5). The new job's watermark moves
// back only if it is still the slot's.
func (in *instance) undoReservation(a *runAdmission, giveBack bool) {
	in.mu.Lock()
	delete(in.runRecs, a.id)
	if cur := in.jobByName[a.def.Name]; cur != nil {
		if cur.st.Runs > 0 {
			cur.st.Runs--
		}
		if giveBack && cur.st.Through.Equal(a.slot) {
			cur.st.Through, cur.st.Missed, cur.st.LastMissed = a.through, a.missed, a.lastMissed
		}
	}
	in.gen++ // the file may hold the reservation (a failed directory sync): write the rollback (review 3c-4a R6)
	gen := in.gen
	in.mu.Unlock()
	in.save(gen)
}
