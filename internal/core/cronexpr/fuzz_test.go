package cronexpr

import (
	"fmt"
	"testing"
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

// rangeMask returns the bits min..max.
func rangeMask(lo, hi int) uint64 {
	var m uint64
	for v := lo; v <= hi; v++ {
		m |= 1 << uint(v)
	}
	return m
}
