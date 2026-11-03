package cronexpr

import (
	"errors"
	"testing"
	"time"
	_ "time/tzdata" // the zones below, also where the host has no zoneinfo
)

func loadLoc(t testing.TB, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

func utc(t testing.TB, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func mustParse(t testing.TB, expr string) *Schedule {
	t.Helper()
	s, err := Parse(expr)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// checkRun fails unless got is a valid result of Next(after, loc): strictly
// after, in loc, on a whole local minute, without a monotonic reading.
func checkRun(t testing.TB, got, after time.Time, loc *time.Location) {
	t.Helper()
	if !got.After(after) {
		t.Errorf("Next(%s) = %s, not strictly after", after, got)
	}
	if got.Location() != loc {
		t.Errorf("Next(%s) location = %s, want %s", after, got.Location(), loc)
	}
	if got.Second() != 0 || got.Nanosecond() != 0 {
		t.Errorf("Next(%s) = %s, not on a whole local minute", after, got)
	}
	if got != got.Round(0) {
		t.Errorf("Next(%s) = %s carries a monotonic reading", after, got)
	}
}

func TestNextContract(t *testing.T) {
	tests := []struct {
		name, expr, zone, after, want string
	}{
		{"after on a slot", "*/15 * * * *", "UTC", "2025-01-01T10:00:00Z", "2025-01-01T10:15:00Z"},
		{"after between slots", "*/15 * * * *", "UTC", "2025-01-01T10:07:00Z", "2025-01-01T10:15:00Z"},
		{"after with fractional seconds", "*/15 * * * *", "UTC", "2025-01-01T10:14:59.999999999Z", "2025-01-01T10:15:00Z"},
		{"after one nanosecond past a slot", "*/15 * * * *", "UTC", "2025-01-01T10:15:00.000000001Z", "2025-01-01T10:30:00Z"},
		{"local time", "0 2 * * *", "Europe/Rome", "2025-01-10T00:00:00Z", "2025-01-10T01:00:00Z"},
		{"after given in another zone", "0 2 * * *", "Europe/Rome", "2025-01-09T19:00:00-05:00", "2025-01-10T01:00:00Z"},
		{"next day", "0 2 * * *", "Europe/Rome", "2025-01-10T01:00:00Z", "2025-01-11T01:00:00Z"},
		{"half-hour offset", "0 9 * * *", "Asia/Kolkata", "2025-01-10T00:00:00Z", "2025-01-10T03:30:00Z"},
		{"year end", "0 0 1 1 *", "UTC", "2025-06-01T00:00:00Z", "2026-01-01T00:00:00Z"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			loc := loadLoc(t, tt.zone)
			after := utc(t, tt.after)
			got, err := mustParse(t, tt.expr).Next(after, loc)
			if err != nil {
				t.Fatal(err)
			}
			if want := utc(t, tt.want); !got.Equal(want) {
				t.Errorf("Next(%s) = %s, want %s", tt.after, got.UTC(), want)
			}
			checkRun(t, got, after, loc)
		})
	}
}

func TestNextDropsMonotonicReading(t *testing.T) {
	after := time.Now() // only time.Now carries a monotonic reading; the checks are invariants
	got, err := mustParse(t, "* * * * *").Next(after, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	checkRun(t, got, after, time.UTC)
}

func TestNextNilLocation(t *testing.T) {
	_, err := mustParse(t, "* * * * *").Next(utc(t, "2025-01-01T00:00:00Z"), nil)
	if err == nil || errors.Is(err, ErrNoRun) {
		t.Fatalf("Next with a nil location: err = %v, want a location error", err)
	}
}

// TestNextAcrossTransitions checks run sequences across DST and other
// offset changes, computed by hand from the zones' transitions (UTC):
//
//	Europe/Rome          2025-03-30 01:00 local 02:00 → 03:00; 2025-10-26 01:00 local 03:00 → 02:00
//	America/New_York     2025-03-09 07:00 local 02:00 → 03:00; 2025-11-02 06:00 local 02:00 → 01:00
//	Australia/Lord_Howe  2025-10-04 15:30 local 02:00 → 02:30; 2025-04-05 15:00 local 02:00 → 01:30
//	America/Santiago     2025-09-07 04:00 local 00:00 → 01:00; 2025-04-06 03:00 local 00:00 → 23:00 (04-05)
//	Pacific/Apia         2011-12-30 10:00 local 12-29 24:00 → 12-31 00:00
//	America/St_Johns     1988-10-30 01:31 local 00:01 → 22:01 (10-29), a two-hour rollback
//	Pacific/Kwajalein    1969-09-30 13:00 local 23:59 → 01:00 (09-30), a 23-hour rollback
//	Africa/Monrovia      1972-01-07 00:44:30 local 00:00 → 00:44:30 (offset −0:44:30 → 0)
//
// A fixed hour runs once at the first whole local minute after a gap and
// once, at the first occurrence, in an overlap; a wildcard hour ('*' or
// '*/n') follows elapsed time.
func TestNextAcrossTransitions(t *testing.T) {
	tests := []struct {
		name, zone, expr, after string
		want                    []string
	}{
		// Rome, spring gap.
		{"rome gap fixed", "Europe/Rome", "30 2 * * *", "2025-03-29T12:00:00Z",
			[]string{"2025-03-30T01:00:00Z", "2025-03-31T00:30:00Z"}},
		{"rome gap fixed coalesces minutes", "Europe/Rome", "*/15 2 * * *", "2025-03-29T12:00:00Z",
			[]string{"2025-03-30T01:00:00Z", "2025-03-31T00:00:00Z", "2025-03-31T00:15:00Z"}},
		{"rome gap fixed coalesces with the next hour", "Europe/Rome", "0 2,3 * * *", "2025-03-29T12:00:00Z",
			[]string{"2025-03-30T01:00:00Z", "2025-03-31T00:00:00Z", "2025-03-31T01:00:00Z"}},
		{"rome gap wildcard skips missing minutes", "Europe/Rome", "30 * * * *", "2025-03-30T00:00:00Z",
			[]string{"2025-03-30T00:30:00Z", "2025-03-30T01:30:00Z", "2025-03-30T02:30:00Z"}},
		{"rome gap wildcard step", "Europe/Rome", "0 */2 * * *", "2025-03-29T22:30:00Z",
			[]string{"2025-03-29T23:00:00Z", "2025-03-30T02:00:00Z"}},
		{"rome gap range step is fixed", "Europe/Rome", "0 0-23/2 * * *", "2025-03-29T22:30:00Z",
			[]string{"2025-03-29T23:00:00Z", "2025-03-30T01:00:00Z", "2025-03-30T02:00:00Z"}},
		// Rome, autumn overlap: local 02:00-02:59 at 00:00-00:59Z and 01:00-01:59Z.
		{"rome overlap fixed runs once", "Europe/Rome", "30 2 * * *", "2025-10-25T12:00:00Z",
			[]string{"2025-10-26T00:30:00Z", "2025-10-27T01:30:00Z"}},
		{"rome overlap fixed minutes run once", "Europe/Rome", "*/15 2 * * *", "2025-10-25T12:00:00Z",
			[]string{"2025-10-26T00:00:00Z", "2025-10-26T00:15:00Z", "2025-10-26T00:30:00Z", "2025-10-26T00:45:00Z", "2025-10-27T01:00:00Z"}},
		{"rome overlap wildcard runs twice", "Europe/Rome", "30 * * * *", "2025-10-25T23:00:00Z",
			[]string{"2025-10-25T23:30:00Z", "2025-10-26T00:30:00Z", "2025-10-26T01:30:00Z", "2025-10-26T02:30:00Z"}},
		{"rome inside second occurrence fixed", "Europe/Rome", "30 2 * * *", "2025-10-26T01:10:00Z",
			[]string{"2025-10-27T01:30:00Z"}},
		{"rome inside second occurrence wildcard", "Europe/Rome", "30 * * * *", "2025-10-26T01:10:00Z",
			[]string{"2025-10-26T01:30:00Z"}},
		// New York.
		{"new york gap fixed", "America/New_York", "30 2 * * *", "2025-03-08T12:00:00Z",
			[]string{"2025-03-09T07:00:00Z", "2025-03-10T06:30:00Z"}},
		{"new york overlap fixed", "America/New_York", "30 1 * * *", "2025-11-01T12:00:00Z",
			[]string{"2025-11-02T05:30:00Z", "2025-11-03T06:30:00Z"}},
		{"new york odd range step is fixed", "America/New_York", "0 1-23/2 * * *", "2025-11-02T05:30:00Z",
			[]string{"2025-11-02T08:00:00Z"}}, // repeated 01:00 (06:00Z) skipped; 03:00 EST
		{"new york overlap wildcard", "America/New_York", "30 * * * *", "2025-11-02T04:45:00Z",
			[]string{"2025-11-02T05:30:00Z", "2025-11-02T06:30:00Z", "2025-11-02T07:30:00Z"}},
		// Lord Howe: 30-minute shifts.
		{"lord howe gap fixed", "Australia/Lord_Howe", "15 2 * * *", "2025-10-04T12:00:00Z",
			[]string{"2025-10-04T15:30:00Z", "2025-10-05T15:15:00Z"}},
		{"lord howe gap wildcard", "Australia/Lord_Howe", "15 * * * *", "2025-10-04T15:00:00Z",
			[]string{"2025-10-04T16:15:00Z"}},
		{"lord howe overlap fixed", "Australia/Lord_Howe", "45 1 * * *", "2025-04-05T12:00:00Z",
			[]string{"2025-04-05T14:45:00Z", "2025-04-06T15:15:00Z"}},
		{"lord howe overlap wildcard", "Australia/Lord_Howe", "45 * * * *", "2025-04-05T14:00:00Z",
			[]string{"2025-04-05T14:45:00Z", "2025-04-05T15:15:00Z", "2025-04-05T16:15:00Z"}},
		// Santiago: transitions at midnight.
		{"santiago midnight gap fixed", "America/Santiago", "30 0 * * *", "2025-09-06T12:00:00Z",
			[]string{"2025-09-07T04:00:00Z", "2025-09-08T03:30:00Z"}},
		{"santiago midnight gap wildcard", "America/Santiago", "30 * * * *", "2025-09-07T03:00:00Z",
			[]string{"2025-09-07T03:30:00Z", "2025-09-07T04:30:00Z"}},
		{"santiago overlap before midnight fixed", "America/Santiago", "30 23 * * *", "2025-04-05T12:00:00Z",
			[]string{"2025-04-06T02:30:00Z", "2025-04-07T03:30:00Z"}},
		{"santiago overlap before midnight wildcard", "America/Santiago", "30 * * * *", "2025-04-06T02:00:00Z",
			[]string{"2025-04-06T02:30:00Z", "2025-04-06T03:30:00Z", "2025-04-06T04:30:00Z"}},
		// Apia: 2011-12-30 does not exist.
		{"apia skipped day fixed", "Pacific/Apia", "0 12 30 12 *", "2011-12-29T00:00:00Z",
			[]string{"2011-12-30T10:00:00Z", "2012-12-29T22:00:00Z"}},
		{"apia skipped day wildcard", "Pacific/Apia", "0 * 30 12 *", "2011-12-29T00:00:00Z",
			[]string{"2012-12-29T10:00:00Z"}},
		{"apia hourly across the jump", "Pacific/Apia", "0 * * * *", "2011-12-30T08:30:00Z",
			[]string{"2011-12-30T09:00:00Z", "2011-12-30T10:00:00Z", "2011-12-30T11:00:00Z"}},
		{"apia day before the jump", "Pacific/Apia", "0 12 * * *", "2011-12-29T00:00:00Z",
			[]string{"2011-12-29T22:00:00Z", "2011-12-30T10:00:00Z", "2011-12-30T22:00:00Z"}},
		// St John's: a rollback that crosses midnight; local 00:00 of
		// 10-30 comes first, then 10-29 22:01-23:59 again.
		{"st johns first midnight", "America/St_Johns", "0 * * * *", "1988-10-30T01:29:00Z",
			[]string{"1988-10-30T01:30:00Z", "1988-10-30T02:30:00Z"}},
		{"st johns previous date stays searchable", "America/St_Johns", "0 * * * *", "1988-10-30T01:30:00Z",
			[]string{"1988-10-30T02:30:00Z"}},
		{"st johns previous date only", "America/St_Johns", "0 * 29 10 *", "1988-10-30T01:29:00Z",
			[]string{"1988-10-30T02:30:00Z"}},
		// Kwajalein: 13:00 on 09-30 repeats 23 hours later.
		{"kwajalein long rollback fixed", "Pacific/Kwajalein", "0 13 * * *", "1969-10-01T00:00:00Z",
			[]string{"1969-10-02T01:00:00Z"}},
		// Rome 1893-10-31 23:00Z: local mean time +0:49:56 → +1:00, local
		// 23:49:04 → 00:00:00. The whole local minute 00:00 exists at the
		// transition instant itself (review F2).
		{"rome 1893 seconds phase wildcard", "Europe/Rome", "* * * * *", "1893-10-31T22:59:04Z",
			[]string{"1893-10-31T23:00:00Z", "1893-10-31T23:01:00Z"}},
		{"rome 1893 seconds phase hourly", "Europe/Rome", "0 * * * *", "1893-10-31T22:59:04Z",
			[]string{"1893-10-31T23:00:00Z"}},
		{"rome 1893 seconds phase fixed", "Europe/Rome", "0 0 * * *", "1893-10-31T22:59:04Z",
			[]string{"1893-10-31T23:00:00Z"}},
		// Monrovia: a gap ending at 00:44:30 runs at the next whole local minute.
		{"monrovia gap rounds up", "Africa/Monrovia", "0 0 * * *", "1972-01-06T12:00:00Z",
			[]string{"1972-01-07T00:45:00Z", "1972-01-08T00:00:00Z"}},
		// 31 December of a leap year (time.Time.ZoneBounds is wrong there,
		// B-041).
		{"leap year end", "Europe/Rome", "0 0 1 1 *", "2024-12-31T12:00:00Z",
			[]string{"2024-12-31T23:00:00Z"}},
		// UTC+14: local midnight of a sparse date is 14 hours before its
		// UTC midnight, so the date jump must land early enough (review F1).
		{"kiritimati sparse date", "Pacific/Kiritimati", "0 0 1 1 *", "2024-12-30T00:00:00Z",
			[]string{"2024-12-31T10:00:00Z"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			loc := loadLoc(t, tt.zone)
			s := mustParse(t, tt.expr)
			after := utc(t, tt.after)
			for i, w := range tt.want {
				got, err := s.Next(after, loc)
				if err != nil {
					t.Fatalf("run %d: %v", i, err)
				}
				if want := utc(t, w); !got.Equal(want) {
					t.Fatalf("run %d: Next(%s) = %s, want %s", i, after.UTC(), got.UTC(), want)
				}
				checkRun(t, got, after, loc)
				after = got
			}
		})
	}
}

func TestNextSecondsOffset(t *testing.T) {
	loc := time.FixedZone("plus30s", 30)
	after := utc(t, "1970-01-01T00:00:00Z") // local 00:00:30
	got, err := mustParse(t, "* * * * *").Next(after, loc)
	if err != nil {
		t.Fatal(err)
	}
	if want := utc(t, "1970-01-01T00:00:30Z"); !got.Equal(want) {
		t.Errorf("Next = %s, want %s", got.UTC(), want)
	}
	checkRun(t, got, after, loc)
}

// TestNextHourClassification shows, through Next, which hour fields follow
// elapsed time ('*', '*/n') and which are fixed local times (everything
// else, a range step included). From 2025-10-26T00:30Z (02:30 CEST in
// Rome) the wall clock repeats 02:00-02:59 from 01:00Z: a wildcard hour
// runs on the repeated 02:00, a fixed one skips it.
func TestNextHourClassification(t *testing.T) {
	tests := []struct {
		expr, want string
	}{
		{"0 * * * *", "2025-10-26T01:00:00Z"},      // wildcard: repeated 02:00
		{"0 */2 * * *", "2025-10-26T01:00:00Z"},    // wildcard: repeated 02:00
		{"0 0-23 * * *", "2025-10-26T02:00:00Z"},   // fixed: 03:00 CET
		{"0 0-23/2 * * *", "2025-10-26T03:00:00Z"}, // fixed: 04:00 CET
		{"0 1-3 * * *", "2025-10-26T02:00:00Z"},    // fixed: 03:00 CET
		{"0 1,2 * * *", "2025-10-27T00:00:00Z"},    // fixed: 01:00 CET next day
		{"0 2 * * *", "2025-10-27T01:00:00Z"},      // fixed: 02:00 CET next day
		{"@hourly", "2025-10-26T01:00:00Z"},        // "0 * * * *"
		{"@daily", "2025-10-26T23:00:00Z"},         // 00:00 CET on 10-27
		{"@midnight", "2025-10-26T23:00:00Z"},      // 00:00 CET on 10-27
		{"@weekly", "2025-11-01T23:00:00Z"},        // Sunday 11-02 00:00 CET
		{"@monthly", "2025-10-31T23:00:00Z"},       // 11-01 00:00 CET
		{"@yearly", "2025-12-31T23:00:00Z"},        // 2026-01-01 00:00 CET
		{"@annually", "2025-12-31T23:00:00Z"},      // 2026-01-01 00:00 CET
	}
	loc := loadLoc(t, "Europe/Rome")
	after := utc(t, "2025-10-26T00:30:00Z")
	for _, tt := range tests {
		t.Run(tt.expr, func(t *testing.T) {
			got, err := mustParse(t, tt.expr).Next(after, loc)
			if err != nil {
				t.Fatal(err)
			}
			if want := utc(t, tt.want); !got.Equal(want) {
				t.Errorf("Next = %s, want %s", got.UTC(), want)
			}
		})
	}
}

// TestNextDayMatching checks the day rule with dates computed by hand
// (2025-01-01 is a Wednesday).
func TestNextDayMatching(t *testing.T) {
	tests := []struct {
		name, expr string
		want       []string
	}{
		{"both restricted: either matches", "0 0 15 * 5",
			[]string{"2025-01-03T00:00:00Z", "2025-01-10T00:00:00Z", "2025-01-15T00:00:00Z", "2025-01-17T00:00:00Z"}},
		{"day-of-week star step: both match", "0 0 15 * */2",
			[]string{"2025-02-15T00:00:00Z", "2025-03-15T00:00:00Z", "2025-04-15T00:00:00Z"}},
		{"day-of-month star step: both match", "0 0 */10 * 1",
			[]string{"2025-03-31T00:00:00Z", "2025-04-21T00:00:00Z"}},
		{"month names", "0 0 1 jun,dec *",
			[]string{"2025-06-01T00:00:00Z", "2025-12-01T00:00:00Z", "2026-06-01T00:00:00Z"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := mustParse(t, tt.expr)
			after := utc(t, "2025-01-01T00:00:00Z")
			for i, w := range tt.want {
				got, err := s.Next(after, time.UTC)
				if err != nil {
					t.Fatal(err)
				}
				if want := utc(t, w); !got.Equal(want) {
					t.Fatalf("run %d: Next(%s) = %s, want %s", i, after, got, want)
				}
				after = got
			}
		})
	}
}

// TestNextRareSchedules covers long waits: 2100 is not a leap year, and
// 29 February falls on a weekend only every 12 or 28 years.
func TestNextRareSchedules(t *testing.T) {
	tests := []struct {
		expr, after, want string
	}{
		{"0 0 29 2 *", "2096-02-29T00:00:00Z", "2104-02-29T00:00:00Z"},
		{"0 0 29 2 */6", "2020-02-29T00:00:00Z", "2032-02-29T00:00:00Z"},
		{"0 0 29 2 */6", "2088-02-29T00:00:00Z", "2116-02-29T00:00:00Z"},
	}
	for _, tt := range tests {
		t.Run(tt.expr+" after "+tt.after, func(t *testing.T) {
			got, err := mustParse(t, tt.expr).Next(utc(t, tt.after), time.UTC)
			if err != nil {
				t.Fatal(err)
			}
			if want := utc(t, tt.want); !got.Equal(want) {
				t.Errorf("Next = %s, want %s", got, want)
			}
		})
	}
}

// TestNextHorizon reaches ErrNoRun through the internal horizon: Parse
// refuses every schedule that could reach it through Next.
func TestNextHorizon(t *testing.T) {
	s := mustParse(t, "0 0 29 2 *")
	after := utc(t, "2097-03-01T00:00:00Z")
	if _, err := s.next(after, time.UTC, 365); !errors.Is(err, ErrNoRun) {
		t.Errorf("next within a year of 2097-03-01: err = %v, want ErrNoRun", err)
	}
	got, err := s.next(after, time.UTC, 7*366)
	if err != nil {
		t.Fatal(err)
	}
	if want := utc(t, "2104-02-29T00:00:00Z"); !got.Equal(want) {
		t.Errorf("next within seven years = %s, want %s", got, want)
	}
}

func BenchmarkNext(b *testing.B) {
	loc := loadLoc(b, "Europe/Rome")
	after := time.Date(2025, 6, 1, 12, 0, 30, 0, time.UTC)
	for _, expr := range []string{"* * * * *", "30 2 * * *", "@yearly", "0 0 29 2 */6"} {
		s := mustParse(b, expr)
		b.Run(expr, func(b *testing.B) {
			for b.Loop() {
				if _, err := s.Next(after, loc); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
