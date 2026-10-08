// Package cronexpr parses traditional 5-field cron expressions:
//
//	minute hour day-of-month month day-of-week
//
// Supported: '*', numbers, ranges (1-5), lists (1,3,5), steps (*/15, 1-30/5,
// 5/10 meaning 5-max/10), month names (jan-dec), weekday names (sun-sat),
// weekday 7 as Sunday, and the macros @yearly, @annually, @monthly,
// @weekly, @daily, @midnight, @hourly.
//
// Not supported on purpose: @reboot, seconds, years, 'L', 'W', '#', '?'
// and interval syntaxes like "every 5m".
//
// As in Vixie cron and cronie, when both day-of-month and day-of-week are
// restricted a day matches if EITHER field matches. A field that starts
// with '*' (such as "*" or "*/2") counts as unrestricted for this rule, so
// "0 0 1 * */2" runs on the 1st only when it falls on an even weekday.
package cronexpr

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Schedule is a parsed cron expression. Each field is a bitset of allowed
// values.
type Schedule struct {
	Minute, Hour, DayOfMonth, Month, DayOfWeek uint64
	// DayOfMonthStar / DayOfWeekStar record a field that starts with '*'
	// ("*", "*/2", ...). Such a field does not take part in the
	// day-matching OR rule, as in Vixie cron and cronie.
	DayOfMonthStar, DayOfWeekStar bool

	expr string
}

// String returns the normalised source expression.
func (s *Schedule) String() string { return s.expr }

type field struct {
	name     string
	min, max int
	names    map[string]int
}

var (
	minuteField = field{name: "minute", min: 0, max: 59}
	hourField   = field{name: "hour", min: 0, max: 23}
	domField    = field{name: "day-of-month", min: 1, max: 31}
	monthField  = field{name: "month", min: 1, max: 12, names: map[string]int{
		"jan": 1, "feb": 2, "mar": 3, "apr": 4, "may": 5, "jun": 6,
		"jul": 7, "aug": 8, "sep": 9, "oct": 10, "nov": 11, "dec": 12,
	}}
	// Day-of-week accepts 0-7 (both 0 and 7 are Sunday).
	dowField = field{name: "day-of-week", min: 0, max: 7, names: map[string]int{
		"sun": 0, "mon": 1, "tue": 2, "wed": 3, "thu": 4, "fri": 5, "sat": 6,
	}}
)

var macros = map[string]string{
	"@yearly":   "0 0 1 1 *",
	"@annually": "0 0 1 1 *",
	"@monthly":  "0 0 1 * *",
	"@weekly":   "0 0 * * 0",
	"@daily":    "0 0 * * *",
	"@midnight": "0 0 * * *",
	"@hourly":   "0 * * * *",
}

// Parse parses expr.
func Parse(expr string) (*Schedule, error) {
	src := strings.Join(strings.Fields(expr), " ")
	if src == "" {
		return nil, errors.New("cron expression is empty")
	}
	spec := src
	if strings.HasPrefix(src, "@") {
		m, ok := macros[strings.ToLower(src)]
		if !ok {
			return nil, fmt.Errorf("unsupported cron macro %q", src)
		}
		spec = m
	}
	parts := strings.Fields(spec)
	if len(parts) != 5 {
		return nil, fmt.Errorf("cron expression %q must have 5 fields (minute hour day-of-month month day-of-week), got %d", src, len(parts))
	}

	s := &Schedule{expr: src}
	var err error
	var errs []error
	if s.Minute, _, err = parseField(parts[0], minuteField); err != nil {
		errs = append(errs, err)
	}
	if s.Hour, _, err = parseField(parts[1], hourField); err != nil {
		errs = append(errs, err)
	}
	if s.DayOfMonth, s.DayOfMonthStar, err = parseField(parts[2], domField); err != nil {
		errs = append(errs, err)
	}
	if s.Month, _, err = parseField(parts[3], monthField); err != nil {
		errs = append(errs, err)
	}
	if s.DayOfWeek, s.DayOfWeekStar, err = parseField(parts[4], dowField); err != nil {
		errs = append(errs, err)
	}
	if len(errs) > 0 {
		return nil, fmt.Errorf("cron expression %q: %w", src, errors.Join(errs...))
	}
	// Fold Sunday=7 into 0.
	if s.DayOfWeek&(1<<7) != 0 {
		s.DayOfWeek = s.DayOfWeek&^(1<<7) | 1
	}
	return s, nil
}

// parseField returns the bitset for one field and whether it was a bare '*'.
func parseField(text string, f field) (uint64, bool, error) {
	var bits uint64
	for item := range strings.SplitSeq(text, ",") {
		b, err := parseItem(item, f)
		if err != nil {
			return 0, false, fmt.Errorf("%s field: %w", f.name, err)
		}
		bits |= b
	}
	return bits, strings.HasPrefix(text, "*"), nil
}

func parseItem(item string, f field) (uint64, error) {
	if item == "" {
		return 0, errors.New("empty list element")
	}
	rangePart, stepPart, hasStep := strings.Cut(item, "/")
	step := 1
	if hasStep {
		n, err := strconv.Atoi(stepPart)
		if err != nil || n <= 0 {
			return 0, fmt.Errorf("invalid step %q", stepPart)
		}
		step = n
	}

	var lo, hi int
	switch {
	case rangePart == "*":
		lo, hi = f.min, f.max
		if f.name == dowField.name {
			hi = 6 // '*' covers each weekday once
		}
	case strings.Contains(rangePart, "-"):
		a, b, _ := strings.Cut(rangePart, "-")
		var err error
		if lo, err = f.value(a); err != nil {
			return 0, err
		}
		if hi, err = f.value(b); err != nil {
			return 0, err
		}
		if lo > hi {
			return 0, fmt.Errorf("range %q is reversed", rangePart)
		}
	default:
		v, err := f.value(rangePart)
		if err != nil {
			return 0, err
		}
		lo, hi = v, v
		if hasStep {
			hi = f.max // "5/10" means 5-max/10
		}
	}
	max := f.max
	if f.name == dowField.name {
		max = 6 // Sunday=7 is an alias, not an extra weekday.
	}
	if hasStep && step > max-f.min {
		return 0, fmt.Errorf("step %d is larger than range %d-%d", step, f.min, max)
	}
	if step > hi-lo && hasStep && hi != lo {
		return 0, fmt.Errorf("step %d is larger than range %d-%d", step, lo, hi)
	}

	var bits uint64
	for v := lo; v <= hi; v += step {
		bits |= 1 << uint(v)
		// Stop before adding a step that would exceed hi or overflow.
		if step > hi-v {
			break
		}
	}
	return bits, nil
}

func (f field) value(s string) (int, error) {
	if v, ok := f.names[strings.ToLower(s)]; ok {
		return v, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("invalid value %q", s)
	}
	if n < f.min || n > f.max {
		return 0, fmt.Errorf("value %d out of range %d-%d", n, f.min, f.max)
	}
	return n, nil
}
