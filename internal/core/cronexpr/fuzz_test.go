package cronexpr

import (
	"fmt"
	"testing"
	"time"
)

// FuzzParse checks that every accepted expression only sets values inside
// each field's range: the property the step overflow broke.
func FuzzParse(f *testing.F) {
	for _, seed := range []string{
		"* * * * *", "*/15 1-10/3 1,15 jan-mar mon-fri", "0 0 1 * */2", "@hourly",
		"59/10 * * * *", "59/9223372036854775807 * * * *", "0-7/7 * * * *", "* * * * 7",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, expr string) {
		s, err := Parse(expr)
		if err != nil {
			return
		}
		fields := []struct {
			name     string
			bits     uint64
			min, max int
		}{
			{"minute", s.minute, 0, 59},
			{"hour", s.hour, 0, 23},
			{"day-of-month", s.dayOfMonth, 1, 31},
			{"month", s.month, 1, 12},
			{"day-of-week", s.dayOfWeek, 0, 6}, // 7 is folded into 0
		}
		for _, f := range fields {
			if f.bits == 0 {
				t.Fatalf("Parse(%q): %s allows no value", expr, f.name)
			}
			if outside := f.bits &^ rangeMask(f.min, f.max); outside != 0 {
				t.Fatalf("Parse(%q): %s has values outside %d-%d: %#x", expr, f.name, f.min, f.max, outside)
			}
		}
	})
}

// FuzzParseStep compares "lo/step" in the minute field with an
// independent oracle: lo, lo+step, ... up to 59, or an error when the step
// is larger than the field's range. The range property of FuzzParse alone
// would not catch an overflow that adds values inside the range.
func FuzzParseStep(f *testing.F) {
	f.Add(uint8(5), int64(10))
	f.Add(uint8(59), int64(9223372036854775807))
	f.Add(uint8(0), int64(59))
	f.Fuzz(func(t *testing.T, lo uint8, step int64) {
		if lo > 59 || step <= 0 {
			t.Skip()
		}
		s, err := Parse(fmt.Sprintf("%d/%d * * * *", lo, step))
		if step > 59 {
			if err == nil {
				t.Fatalf("%d/%d accepted, want a step error", lo, step)
			}
			return
		}
		if err != nil {
			t.Fatalf("%d/%d: %v", lo, step, err)
		}
		var want uint64
		for v := int64(lo); v <= 59; v += step {
			want |= 1 << uint(v)
		}
		if s.minute != want {
			t.Fatalf("%d/%d: minute bits %#x, want %#x", lo, step, s.minute, want)
		}
	})
}

// FuzzNext compares Next with referenceNext for random expressions,
// instants between 1980 and 2030 and the reference zones.
func FuzzNext(f *testing.F) {
	f.Add("30 2 * * *", int64(0), uint8(0))
	f.Add("*/15 2 * * *", int64(13_000_000), uint8(1))
	f.Add("45 * * * *", int64(23_000_000), uint8(2))
	f.Add("0 0 29 2 */6", int64(5_000_000), uint8(3))
	locs := make([]*time.Location, len(referenceZones))
	for i, z := range referenceZones {
		locs[i] = loadLoc(f, z)
	}
	start := time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC)
	span := int64(time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC).Sub(start) / time.Minute)
	f.Fuzz(func(t *testing.T, expr string, minute int64, zone uint8) {
		s, err := Parse(expr)
		if err != nil {
			return
		}
		if minute < 0 {
			minute = -(minute + 1)
		}
		after := start.Add(time.Duration(minute%span)*time.Minute + 17*time.Second)
		compareWithReference(t, s, after, locs[int(zone)%len(locs)])
	})
}
