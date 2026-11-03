package supervisor

import (
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/sentinel-watchdog/sentinel-watchdog/pkg/api"
)

// jobState is what is kept of one job, by name (3c-4a). Through is the
// watermark: every slot at or before it is consumed (run, skipped or
// missed) and never runs again, whatever the clock does (Q5); it only
// grows. The counters and the last result are for status.
type jobState struct {
	Digest  string    `json:"digest"`
	Through time.Time `json:"through"`
	// Runs counts the launches; Succeeded, Failed and TimedOut their ends;
	// Skipped the slots skipped by forbid; Missed the catch-ups, each of
	// which skipped every slot older than a minute (decision 3c-4a Q-A).
	Runs       uint64        `json:"runs,omitempty"`
	Succeeded  uint64        `json:"succeeded,omitempty"`
	Failed     uint64        `json:"failed,omitempty"`
	TimedOut   uint64        `json:"timed_out,omitempty"`
	Skipped    uint64        `json:"skipped,omitempty"`
	Missed     uint64        `json:"missed,omitempty"`
	LastMissed *missedRecord `json:"last_missed,omitempty"`
	Last       *runResult    `json:"last,omitempty"`
}

// missedRecord is the interval of slots the last catch-up skipped: from
// the first missed slot to the time before which every slot was missed.
type missedRecord struct {
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
}

// runResult is how a run ended.
type runResult struct {
	RunID     string    `json:"run_id"`
	Scheduled time.Time `json:"scheduled"`
	Started   time.Time `json:"started,omitzero"` // zero: never launched
	Ended     time.Time `json:"ended"`
	// Result is succeeded, failed or timed_out; Failure the fixed code of a
	// failure (Q8); Code and Signal how the process ended, when it ran.
	Result  string `json:"result"`
	Failure string `json:"failure,omitempty"`
	Code    int    `json:"code"`
	Signal  string `json:"signal,omitempty"`
}

// runRecord is one run of a job that is not over, by run id: its owner
// survives a restart of sentineld (Q14, review C6). Phase says how far it
// got: reserved (its slot is consumed, the launch not confirmed), running
// (Group is its process group), cleanup_pending (it ended and was
// reported, but its group could not be confirmed empty).
type runRecord struct {
	Job       string      `json:"job"`
	Phase     string      `json:"phase"`
	Scheduled time.Time   `json:"scheduled"`
	Started   time.Time   `json:"started,omitzero"`
	Group     groupRecord `json:"group,omitzero"`
}

// Phases of a run record.
const (
	runReserved       = "reserved"
	runRunning        = "running"
	runCleanupPending = "cleanup_pending"
)

// Results of a run.
const (
	resultSucceeded = "succeeded"
	resultFailed    = "failed"
	resultTimedOut  = "timed_out"
)

// runIDRe is a run id: model.NewEventID's 32 hex digits.
var runIDRe = regexp.MustCompile(`^[0-9a-f]{32}$`)

// validateJobs checks the jobs and run records of a state file. The run
// records share the cap of the group records: both own process groups.
func (f *fileState) validateJobs() error {
	if len(f.Jobs) > maxServices {
		return fmt.Errorf("more than %d jobs", maxServices)
	}
	for name, j := range f.Jobs {
		switch {
		case !api.ValidName(name):
			return errors.New("invalid job name")
		case !digestRe.MatchString(j.Digest):
			return fmt.Errorf("job %s: invalid digest", name)
		case j.Through.IsZero():
			return fmt.Errorf("job %s: no watermark", name)
		case j.LastMissed != nil && (j.LastMissed.From.IsZero() || j.LastMissed.To.Before(j.LastMissed.From)):
			return fmt.Errorf("job %s: invalid missed interval", name)
		case j.Last != nil && !validResult(*j.Last):
			return fmt.Errorf("job %s: invalid last result", name)
		}
	}
	if len(f.Groups)+len(f.Runs) > maxGroups {
		return fmt.Errorf("more than %d process group and run records", maxGroups)
	}
	for id, r := range f.Runs {
		switch {
		case !runIDRe.MatchString(id):
			return errors.New("invalid run id")
		case !api.ValidName(r.Job):
			return fmt.Errorf("run %s: invalid job name", id)
		case r.Scheduled.IsZero():
			return fmt.Errorf("run %s: no slot", id)
		case r.Phase == runReserved && (!r.Started.IsZero() || r.Group != groupRecord{}):
			return fmt.Errorf("run %s: a reserved run has no process", id)
		case r.Phase == runRunning && r.Started.IsZero(),
			r.Phase == runCleanupPending && r.Group == groupRecord{}:
			return fmt.Errorf("run %s: invalid phase", id)
		case r.Phase != runReserved && r.Phase != runRunning && r.Phase != runCleanupPending:
			return fmt.Errorf("run %s: unknown phase", id)
		case r.Group != groupRecord{} && !validGroup(r.Group):
			return fmt.Errorf("run %s: invalid process group", id)
		}
	}
	return nil
}

func validResult(r runResult) bool {
	switch {
	case !runIDRe.MatchString(r.RunID), r.Scheduled.IsZero(), r.Ended.IsZero():
		return false
	case r.Code < -1 || r.Code > 255, r.Signal != "" && !signalRe.MatchString(r.Signal):
		return false
	}
	switch r.Result {
	case resultSucceeded:
		return r.Failure == ""
	case resultFailed:
		return r.Failure != ""
	case resultTimedOut:
		return true
	}
	return false
}

// validGroup reports whether g is a stored process group.
func validGroup(g groupRecord) bool {
	return g.Pgid >= minPgid && g.Pgid <= maxPgid && g.Sid >= 1 && g.Sid <= maxPgid && g.Start != 0 && bootRe.MatchString(g.Boot)
}

// reconcileJobs gives each configured job the state it starts from: what
// was kept for its definition, or, for a new or changed one, a watermark
// of now, never earlier than the kept one (Q3, review C2): it runs from
// its next slot, and no slot it consumed runs again. A changed job keeps
// its counters and last result: they count by name, as forbid does, and a
// run of the old definition may still end (review 3c-4a R1). changed
// reports whether the file should be written again. Run records are not
// touched: they outlive the definitions.
func reconcileJobs(kept *fileState, jobs []*job, now time.Time) (changed bool) {
	var old map[string]jobState
	if kept != nil {
		old = kept.Jobs
	}
	for _, j := range jobs {
		st, ok := old[j.def.Name]
		if ok && st.Digest == j.digest {
			j.st = st
			continue
		}
		if st.Through.Before(now) {
			st.Through = now
		}
		st.Digest = j.digest
		j.st = st
		changed = true
	}
	return changed || len(old) != len(jobs)
}

// runsOrNil returns f's run records; nil for a missing file.
func (f *fileState) runsOrNil() map[string]runRecord {
	if f == nil {
		return nil
	}
	return f.Runs
}

// maxWatermarkAhead bounds how far a job's watermark may be ahead of the
// clock at Start or at a reload (decision 3c-4a F9, the maintainer's
// option b): further ahead, the clock was wrong forward (a bad RTC at boot,
// then NTP), not set back by a DST or NTP step, and the job would not run
// until the clock caught up. Such a watermark goes back to now; a slot of
// that day may then run a second time.
const maxWatermarkAhead = 24 * time.Hour

// resetFarWatermarks moves back to now the watermarks more than
// maxWatermarkAhead ahead of it and returns the names of those jobs.
func resetFarWatermarks(jobs []*job, now time.Time) []string {
	var reset []string
	for _, j := range jobs {
		if j.st.Through.After(now.Add(maxWatermarkAhead)) {
			j.st.Through = now
			reset = append(reset, j.def.Name)
		}
	}
	return reset
}
