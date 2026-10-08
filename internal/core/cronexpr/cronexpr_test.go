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
			minute: bits(0), hour: bits(2), dayOfMonth: rangeBits(1, 31, 1), month: rangeBits(1, 12, 1),
			dayOfWeek: rangeBits(0, 6, 1), dayOfMonthStar: true, dayOfWeekStar: true,
		}},
		{"*/15 9-17 * * mon-fri", Schedule{
			minute: bits(0, 15, 30, 45), hour: rangeBits(9, 17, 1), dayOfMonth: rangeBits(1, 31, 1),
			month: rangeBits(1, 12, 1), dayOfWeek: rangeBits(1, 5, 1), dayOfMonthStar: true,
		}},
		{"5,10 0 1,15 JAN,jul 7", Schedule{
			minute: bits(5, 10), hour: bits(0), dayOfMonth: bits(1, 15), month: bits(1, 7), dayOfWeek: bits(0),
		}},
		{"5/20 1-10/3 * * 0", Schedule{
			minute: bits(5, 25, 45), hour: bits(1, 4, 7, 10), dayOfMonth: rangeBits(1, 31, 1),
			month: rangeBits(1, 12, 1), dayOfWeek: bits(0), dayOfMonthStar: true,
		}},
		{"59/10 * * * *", Schedule{
			minute: bits(59), hour: rangeBits(0, 23, 1), dayOfMonth: rangeBits(1, 31, 1),
			month: rangeBits(1, 12, 1), dayOfWeek: rangeBits(0, 6, 1), dayOfMonthStar: true, dayOfWeekStar: true,
		}},
		{"59/59 23/23 31/30 12/11 7/6", Schedule{
			minute: bits(59), hour: bits(23), dayOfMonth: bits(31), month: bits(12), dayOfWeek: bits(0),
		}},
		{"*/59 */23 */30 */11 */6", Schedule{
			minute: bits(0, 59), hour: bits(0, 23), dayOfMonth: bits(1, 31), month: bits(1, 12),
			dayOfWeek: bits(0, 6), dayOfMonthStar: true, dayOfWeekStar: true,
		}},
		{"@hourly", Schedule{
			minute: bits(0), hour: rangeBits(0, 23, 1), dayOfMonth: rangeBits(1, 31, 1), month: rangeBits(1, 12, 1),
			dayOfWeek: rangeBits(0, 6, 1), dayOfMonthStar: true, dayOfWeekStar: true,
		}},
		{"@weekly", Schedule{
			minute: bits(0), hour: bits(0), dayOfMonth: rangeBits(1, 31, 1), month: rangeBits(1, 12, 1),
			dayOfWeek: bits(0), dayOfMonthStar: true,
		}},
		{"0 0 * * 5-7", Schedule{
			minute: bits(0), hour: bits(0), dayOfMonth: rangeBits(1, 31, 1), month: rangeBits(1, 12, 1),
			dayOfWeek: bits(0, 5, 6), dayOfMonthStar: true,
		}},
		// A field starting with '*' is unrestricted for the day-matching
		// rule even with a step (Vixie cron / cronie behaviour).
		{"0 0 1 * */2", Schedule{
			minute: bits(0), hour: bits(0), dayOfMonth: bits(1), month: rangeBits(1, 12, 1),
			dayOfWeek: rangeBits(0, 6, 2), dayOfWeekStar: true,
		}},
		{"0 0 */2 * 1", Schedule{
			minute: bits(0), hour: bits(0), dayOfMonth: rangeBits(1, 31, 2), month: rangeBits(1, 12, 1),
			dayOfWeek: bits(1), dayOfMonthStar: true,
		}},
		// An explicit full range is a restriction, not a star.
		{"0 0 1-31 * 1", Schedule{
			minute: bits(0), hour: bits(0), dayOfMonth: rangeBits(1, 31, 1), month: rangeBits(1, 12, 1),
			dayOfWeek: bits(1),
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
