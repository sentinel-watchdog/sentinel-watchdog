package cronexpr

import (
	"strings"
	"testing"
)

func bits(vals ...int) uint64 {
	var b uint64
	for _, v := range vals {
		b |= 1 << uint(v)
	}
	return b
}

func rangeBits(lo, hi, step int) uint64 {
	var b uint64
	for v := lo; v <= hi; v += step {
		b |= 1 << uint(v)
	}
	return b
}

func TestParse(t *testing.T) {
	tests := []struct {
		expr string
		want Schedule
	}{
		{"0 2 * * *", Schedule{
			Minute: bits(0), Hour: bits(2), DayOfMonth: rangeBits(1, 31, 1), Month: rangeBits(1, 12, 1),
			DayOfWeek: rangeBits(0, 6, 1), DayOfMonthStar: true, DayOfWeekStar: true,
		}},
		{"*/15 9-17 * * mon-fri", Schedule{
			Minute: bits(0, 15, 30, 45), Hour: rangeBits(9, 17, 1), DayOfMonth: rangeBits(1, 31, 1),
			Month: rangeBits(1, 12, 1), DayOfWeek: rangeBits(1, 5, 1), DayOfMonthStar: true,
		}},
		{"5,10 0 1,15 JAN,jul 7", Schedule{
			Minute: bits(5, 10), Hour: bits(0), DayOfMonth: bits(1, 15), Month: bits(1, 7), DayOfWeek: bits(0),
		}},
		{"5/20 1-10/3 * * 0", Schedule{
			Minute: bits(5, 25, 45), Hour: bits(1, 4, 7, 10), DayOfMonth: rangeBits(1, 31, 1),
			Month: rangeBits(1, 12, 1), DayOfWeek: bits(0), DayOfMonthStar: true,
		}},
		{"59/10 * * * *", Schedule{
			Minute: bits(59), Hour: rangeBits(0, 23, 1), DayOfMonth: rangeBits(1, 31, 1),
			Month: rangeBits(1, 12, 1), DayOfWeek: rangeBits(0, 6, 1), DayOfMonthStar: true, DayOfWeekStar: true,
		}},
		{"59/59 23/23 31/30 12/11 7/6", Schedule{
			Minute: bits(59), Hour: bits(23), DayOfMonth: bits(31), Month: bits(12), DayOfWeek: bits(0),
		}},
		{"*/59 */23 */30 */11 */6", Schedule{
			Minute: bits(0, 59), Hour: bits(0, 23), DayOfMonth: bits(1, 31), Month: bits(1, 12),
			DayOfWeek: bits(0, 6), DayOfMonthStar: true, DayOfWeekStar: true,
		}},
		{"@hourly", Schedule{
			Minute: bits(0), Hour: rangeBits(0, 23, 1), DayOfMonth: rangeBits(1, 31, 1), Month: rangeBits(1, 12, 1),
			DayOfWeek: rangeBits(0, 6, 1), DayOfMonthStar: true, DayOfWeekStar: true,
		}},
		{"@weekly", Schedule{
			Minute: bits(0), Hour: bits(0), DayOfMonth: rangeBits(1, 31, 1), Month: rangeBits(1, 12, 1),
			DayOfWeek: bits(0), DayOfMonthStar: true,
		}},
		{"0 0 * * 5-7", Schedule{
			Minute: bits(0), Hour: bits(0), DayOfMonth: rangeBits(1, 31, 1), Month: rangeBits(1, 12, 1),
			DayOfWeek: bits(0, 5, 6), DayOfMonthStar: true,
		}},
		// A field starting with '*' is unrestricted for the day-matching
		// rule even with a step (Vixie cron / cronie behaviour).
		{"0 0 1 * */2", Schedule{
			Minute: bits(0), Hour: bits(0), DayOfMonth: bits(1), Month: rangeBits(1, 12, 1),
			DayOfWeek: rangeBits(0, 6, 2), DayOfWeekStar: true,
		}},
		{"0 0 */2 * 1", Schedule{
			Minute: bits(0), Hour: bits(0), DayOfMonth: rangeBits(1, 31, 2), Month: rangeBits(1, 12, 1),
			DayOfWeek: bits(1), DayOfMonthStar: true,
		}},
		// An explicit full range is a restriction, not a star.
		{"0 0 1-31 * 1", Schedule{
			Minute: bits(0), Hour: bits(0), DayOfMonth: rangeBits(1, 31, 1), Month: rangeBits(1, 12, 1),
			DayOfWeek: bits(1),
		}},
	}
	for _, tt := range tests {
		t.Run(tt.expr, func(t *testing.T) {
			got, err := Parse(tt.expr)
			if err != nil {
				t.Fatal(err)
			}
			tt.want.expr = got.expr
			if *got != tt.want {
				t.Errorf("Parse(%q)\n got  %+v\n want %+v", tt.expr, *got, tt.want)
			}
		})
	}
}

func TestParseNormalisesWhitespace(t *testing.T) {
	s, err := Parse("  0   2 *  * * ")
	if err != nil {
		t.Fatal(err)
	}
	if s.String() != "0 2 * * *" {
		t.Errorf("String() = %q", s.String())
	}
}

func TestParseErrors(t *testing.T) {
	tests := []struct {
		expr, want string
	}{
		{"", "empty"},
		{"* * * *", "5 fields"},
		{"* * * * * *", "5 fields"},
		{"every 5m", "5 fields"},
		{"@reboot", "unsupported cron macro"},
		{"@every 5m", "unsupported cron macro"},
		{"60 * * * *", "minute field: value 60 out of range"},
		{"* 24 * * *", "hour field"},
		{"* * 0 * *", "day-of-month field"},
		{"* * * 13 *", "month field"},
		{"* * * * 8", "day-of-week field"},
		{"10-5 * * * *", "reversed"},
		{"*/0 * * * *", "invalid step"},
		{"*/-1 * * * *", "invalid step"},
		{"*/60 * * * *", "step 60 is larger"},
		{"59/9223372036854775807 * * * *", "minute field: step 9223372036854775807 is larger than range"},
		{"59/60 * * * *", "minute field: step 60 is larger than range"},
		{"59-59/60 * * * *", "minute field: step 60 is larger than range"},
		{"* 23/24 * * *", "hour field: step 24 is larger than range"},
		{"* * 31/31 * *", "day-of-month field: step 31 is larger than range"},
		{"* * * 12/12 *", "month field: step 12 is larger than range"},
		{"* * * * 7/7", "day-of-week field: step 7 is larger than range"},
		{"* * * * 0-7/7", "day-of-week field: step 7 is larger than range"},
		{"1,,2 * * * *", "empty list element"},
		{"a * * * *", `invalid value "a"`},
		{"* * L * *", `invalid value "L"`},
		{"* * * * mon#2", `invalid value`},
		{"* * ? * *", `invalid value "?"`},
	}
	for _, tt := range tests {
		t.Run(tt.expr, func(t *testing.T) {
			_, err := Parse(tt.expr)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Parse(%q) err = %v, want %q", tt.expr, err, tt.want)
			}
		})
	}
}
