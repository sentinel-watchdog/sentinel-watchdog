package cronexpr

import (
	"errors"
	"time"
)

// ErrNoRun reports that a schedule has no run time within the search
// horizon. Parse refuses schedules whose dates never occur, so it needs a
// Schedule not built by Parse, or zone data where every matching minute
// falls in a gap.
var ErrNoRun = errors.New("no run time within 400 years")

const (
	// horizonDays bounds the search: one Gregorian cycle of 400 years.
	horizonDays = 146097
	// lookback is how far before an instant the scan starts. Two offsets
	// of a zone differ by at most 26 hours, so 48 hours always include
	// the first occurrence of a repeated wall time, and every instant
	// whose local date is D lies after D 00:00 (as UTC) minus 16 hours.
	lookback = 48 * time.Hour
)

// Next returns the first run time strictly after after, in loc: a whole
// local minute, without a monotonic reading. The caller resolves "Local"
// and passes the location; a nil loc is an error. Daylight saving time and
// other offset changes follow the rules in the package doc. ErrNoRun is
// returned only when nothing runs within 400 years.
func (s *Schedule) Next(after time.Time, loc *time.Location) (time.Time, error) {
	return s.next(after, loc, horizonDays)
}

// next is Next with the horizon in days, so a test can reach ErrNoRun:
// it searches local dates up to maxDays after after's, and a run on the
// last of them is still returned.
//
// It scans real minutes forward and compares each one's local wall time
// with the schedule. Dates that cannot match are skipped: when the local
// date changes, the scan jumps to lookback before the next date that can
// match.
func (s *Schedule) next(after time.Time, loc *time.Location, maxDays int) (time.Time, error) {
	if loc == nil {
		return time.Time{}, errors.New("cron schedule: nil location")
	}
	after = after.Round(0) // drop the monotonic reading
	limit := dateOf(wall(after, loc)).AddDate(0, 0, maxDays)

	t := alignMinute(after.Add(-lookback), loc)
	prev := wall(t, loc)
	hi := prev // the highest wall minute seen: anything not after it is a repeat
	var day time.Time
	for {
		if d := dateOf(prev); !d.Equal(day) {
			// A later instant can carry a local date up to two days
			// earlier (a clock set back by at most 26 hours).
			match, ok := s.nextDay(d.AddDate(0, 0, -2), limit)
			if !ok {
				return time.Time{}, ErrNoRun
			}
			if target := match.Add(-lookback); target.After(t) {
				t = alignMinute(target, loc)
				prev = wall(t, loc)
			}
			day = dateOf(prev)
		}
		t = stepMinute(t, loc)
		w := wall(t, loc)
		if t.After(after) && s.runsAt(prev, w, hi) {
			return t.In(loc), nil
		}
		prev = w
		if w.After(hi) {
			hi = w
		}
	}
}

// runsAt reports whether the schedule runs at the real minute whose wall
// minute is w, the previous one being prev and the highest one seen hi.
//
// A wildcard hour follows elapsed time: it runs at every occurrence of a
// matching wall minute. A fixed hour runs at the first occurrence only
// (w after hi), and when the clock jumps forward over matching wall
// minutes it runs once at the first minute after the jump.
func (s *Schedule) runsAt(prev, w, hi time.Time) bool {
	if s.hourStar {
		return s.matches(w)
	}
	if s.matches(w) && w.After(hi) {
		return true
	}
	// Wall minutes up to hi already had their first occurrence.
	from := prev
	if hi.After(from) {
		from = hi
	}
	for skipped := from.Add(time.Minute); skipped.Before(w); skipped = skipped.Add(time.Minute) {
		if s.matches(skipped) {
			return true
		}
	}
	return false
}

// wall returns t's local wall-clock minute in loc as a UTC time: UTC has
// no DST, so wall times compare and add as plain calendar values.
func wall(t time.Time, loc *time.Location) time.Time {
	l := t.In(loc)
	return time.Date(l.Year(), l.Month(), l.Day(), l.Hour(), l.Minute(), 0, 0, time.UTC)
}

func dateOf(w time.Time) time.Time {
	return time.Date(w.Year(), w.Month(), w.Day(), 0, 0, 0, 0, time.UTC)
}

// alignMinute moves t back to the start of its local minute.
func alignMinute(t time.Time, loc *time.Location) time.Time {
	l := t.In(loc)
	return t.Add(-time.Duration(l.Second())*time.Second - time.Duration(l.Nanosecond()))
}

// stepMinute returns the first whole local minute after t, itself a whole
// local minute. Usually that is t plus one minute. When an offset change
// moved the local seconds, it is the whole minute just before that point if
// it is still after t and the change, otherwise the one just after.
func stepMinute(t time.Time, loc *time.Location) time.Time {
	next := t.Add(time.Minute)
	sec := time.Duration(next.In(loc).Second()) * time.Second
	if sec == 0 {
		return next
	}
	if before := next.Add(-sec); before.After(t) && before.In(loc).Second() == 0 {
		return before
	}
	return next.Add(time.Minute - sec)
}

// nextDay returns the first date from from up to limit whose day fields
// match.
func (s *Schedule) nextDay(from, limit time.Time) (time.Time, bool) {
	for d := from; !d.After(limit); d = d.AddDate(0, 0, 1) {
		if s.matchesDay(d) {
			return d, true
		}
	}
	return time.Time{}, false
}

// matchesDay applies the day rule of the package doc to the date of w.
func (s *Schedule) matchesDay(w time.Time) bool {
	if s.month&(1<<uint(w.Month())) == 0 {
		return false
	}
	dom := s.dayOfMonth&(1<<uint(w.Day())) != 0
	dow := s.dayOfWeek&(1<<uint(w.Weekday())) != 0
	if s.dayOfMonthStar || s.dayOfWeekStar {
		return dom && dow
	}
	return dom || dow
}

// matches reports whether the wall minute w is in the schedule.
func (s *Schedule) matches(w time.Time) bool {
	return s.minute&(1<<uint(w.Minute())) != 0 && s.hour&(1<<uint(w.Hour())) != 0 && s.matchesDay(w)
}
