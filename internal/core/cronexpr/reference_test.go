package cronexpr

import (
	"testing"
	"time"
)

// referenceLookback is enough history for the reference zones (their
// clocks go back by at most two hours); Next itself uses a longer one.
const referenceLookback = 27 * time.Hour

// reference is a deliberately plain version of Next's rules for
// comparison: fed every real minute in order, with no date jumps, it says
// whether the schedule runs there. It shares the rules of the package doc,
// not their implementation: its day matching is written again from the
// doc. Its minute steps assume whole-minute offsets (true for the
// reference zones in the years tested).
type reference struct {
	s          *Schedule
	prev, seen time.Time // previous and highest wall minute fed
	started    bool
}

func (r *reference) runs(w time.Time) bool {
	if !r.started {
		r.prev, r.seen, r.started = w, w, true
		return false
	}
	run := false
	if r.s.hourStar {
		run = r.match(w)
	} else {
		run = r.match(w) && w.After(r.seen)
		for m := r.prev.Add(time.Minute); !run && m.Before(w); m = m.Add(time.Minute) {
			run = m.After(r.seen) && r.match(m)
		}
	}
	r.prev = w
	if w.After(r.seen) {
		r.seen = w
	}
	return run
}

func (r *reference) match(w time.Time) bool {
	s := r.s
	in := func(bits uint64, v int) bool { return bits&(1<<uint(v)) != 0 }
	if !in(s.minute, w.Minute()) || !in(s.hour, w.Hour()) || !in(s.month, int(w.Month())) {
		return false
	}
	dom, dow := in(s.dayOfMonth, w.Day()), in(s.dayOfWeek, int(w.Weekday()))
	if !s.dayOfMonthStar && !s.dayOfWeekStar {
		return dom || dow // both restricted: either one
	}
	return dom && dow
}

// walkMinutes calls fn with every real minute from from to to and its local
// wall minute, until fn returns false.
func walkMinutes(loc *time.Location, from, to time.Time, fn func(t, w time.Time) bool) {
	l := from.In(loc)
	for t := from.Add(-time.Duration(l.Second())*time.Second - time.Duration(l.Nanosecond())); t.Before(to); t = t.Add(time.Minute) {
		l := t.In(loc)
		if !fn(t, time.Date(l.Year(), l.Month(), l.Day(), l.Hour(), l.Minute(), 0, 0, time.UTC)) {
			return
		}
	}
}

// referenceNext returns the reference's first run after after within the
// window, if any.
func referenceNext(s *Schedule, after time.Time, loc *time.Location, window time.Duration) (time.Time, bool) {
	r := reference{s: s}
	var found time.Time
	walkMinutes(loc, after.Add(-referenceLookback), after.Add(window), func(t, w time.Time) bool {
		if r.runs(w) && t.After(after) {
			found = t
			return false
		}
		return true
	})
	return found.In(loc), !found.IsZero()
}

var referenceZones = []string{
	"Europe/Rome", "America/New_York", "Australia/Lord_Howe", "Asia/Kolkata",
	"America/Santiago", "Pacific/Apia", "America/St_Johns",
}

var referenceExprs = []string{
	"* * * * *", "30 2 * * *", "*/15 2 * * *", "0 2,3 * * *", "30 * * * *",
	"0 */2 * * *", "0 0-23/2 * * *", "45 1 * * *", "30 0 * * *", "30 23 * * *",
}

// TestNextMatchesScanNearTransitions compares Next with referenceNext from
// instants around each transition of the reference zones in 2025 (Apia:
// 2011), for schedules that run every day.
func TestNextMatchesScanNearTransitions(t *testing.T) {
	for _, zone := range referenceZones {
		t.Run(zone, func(t *testing.T) {
			t.Parallel()
			loc := loadLoc(t, zone)
			year := 2025
			if zone == "Pacific/Apia" {
				year = 2011
			}
			for _, tr := range transitions(loc, year) {
				for _, expr := range referenceExprs {
					s := mustParse(t, expr)
					// Before, inside and after the changed hour, off the minute.
					for _, d := range []time.Duration{-3 * time.Hour, -61 * time.Minute, -7 * time.Minute, 0, 29*time.Minute + 30*time.Second, 2 * time.Hour} {
						compareWithReference(t, s, tr.Add(d), loc)
					}
				}
			}
		})
	}
}

// transitions returns the instants in year where loc's offset changes.
func transitions(loc *time.Location, year int) []time.Time {
	var out []time.Time
	t := time.Date(year, 1, 1, 0, 0, 0, 0, time.UTC)
	_, prev := t.In(loc).Zone()
	for end := t.AddDate(1, 0, 0); t.Before(end); t = t.Add(15 * time.Minute) {
		if _, off := t.In(loc).Zone(); off != prev {
			out = append(out, t)
			prev = off
		}
	}
	return out
}

// compareWithReference checks Next's invariants and that it agrees with
// referenceNext over the next 26 hours.
func compareWithReference(t *testing.T, s *Schedule, after time.Time, loc *time.Location) {
	t.Helper()
	got, err := s.Next(after, loc)
	if err != nil {
		t.Fatalf("%q in %s: Next(%s): %v", s, loc, after.UTC(), err)
	}
	checkRun(t, got, after, loc)
	const window = 26 * time.Hour
	want, ok := referenceNext(s, after, loc, window)
	switch {
	case ok && !got.Equal(want):
		t.Fatalf("%q in %s: Next(%s) = %s, reference %s", s, loc, after.UTC(), got.UTC(), want.UTC())
	case !ok && !got.After(after.Add(window)):
		t.Fatalf("%q in %s: Next(%s) = %s, reference finds no run within %s", s, loc, after.UTC(), got.UTC(), window)
	}
}

// yearExprs run on a few dates only, so Next jumps over the others.
var yearExprs = []string{
	"0 0 * * 0", "30 2 1,15 * *", "0 0 1 1,7 *", "45 23 */10 * 1", "15 1 * 3,10 6",
}

// TestNextMatchesScanOverAYear chains Next through a whole year in every
// reference zone (and UTC+14) for sparse schedules, and compares the runs
// with one reference walk over the same year (acceptance criterion 5).
func TestNextMatchesScanOverAYear(t *testing.T) {
	for _, zone := range append([]string{"Pacific/Kiritimati"}, referenceZones...) {
		t.Run(zone, func(t *testing.T) {
			t.Parallel()
			loc := loadLoc(t, zone)
			year := 2025
			if zone == "Pacific/Apia" {
				year = 2011
			}
			from := time.Date(year, 1, 1, 0, 0, 0, 0, time.UTC)
			to := from.AddDate(1, 0, 0)

			refs := make([]reference, len(yearExprs))
			want := make([][]time.Time, len(yearExprs))
			for i, expr := range yearExprs {
				refs[i].s = mustParse(t, expr)
			}
			walkMinutes(loc, from.Add(-referenceLookback), to, func(tm, w time.Time) bool {
				for i := range refs {
					if refs[i].runs(w) && tm.After(from) {
						want[i] = append(want[i], tm)
					}
				}
				return true
			})

			for i, r := range refs {
				var got []time.Time
				for after := from; ; {
					run, err := r.s.Next(after, loc)
					if err != nil {
						t.Fatalf("%q: Next(%s): %v", r.s, after.UTC(), err)
					}
					if !run.Before(to) {
						break
					}
					got = append(got, run)
					after = run
				}
				if len(want[i]) == 0 {
					t.Fatalf("%q: the reference found no run in %d", r.s, year)
				}
				if len(got) != len(want[i]) {
					t.Fatalf("%q: Next gives %d runs in %d, the reference %d", r.s, len(got), year, len(want[i]))
				}
				for j := range got {
					if !got[j].Equal(want[i][j]) {
						t.Fatalf("%q: run %d = %s, reference %s", r.s, j, got[j].UTC(), want[i][j].UTC())
					}
				}
			}
		})
	}
}
