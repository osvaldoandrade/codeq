package schedules

import (
	"errors"
	"testing"
	"time"

	"github.com/osvaldoandrade/codeq/internal/core/schedule"
)

func mustTime(t *testing.T, value string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t.Fatalf("parse %q: %v", value, err)
	}
	return v
}

func TestParseCronStandardAndDescriptors(t *testing.T) {
	base := mustTime(t, "2026-10-08T10:07:30Z")
	for _, tc := range []struct {
		rule, want string
	}{
		{"*/5 * * * *", "2026-10-08T10:10:00Z"},
		{"2-59/5 * * * *", "2026-10-08T10:12:00Z"},
		{"0 * * * *", "2026-10-08T11:00:00Z"},
		{"15 3 * * *", "2026-10-09T03:15:00Z"},
		{"@hourly", "2026-10-08T11:00:00Z"},
		{"@every 30s", "2026-10-08T10:08:00Z"},
	} {
		expr, err := ParseCron(tc.rule, "")
		if err != nil {
			t.Fatalf("%q: %v", tc.rule, err)
		}
		if got := expr.Next(base).UTC().Format(time.RFC3339); got != tc.want {
			t.Fatalf("%q: next = %s, want %s", tc.rule, got, tc.want)
		}
	}
}

// Wall-clock rules follow the schedule's timezone, including DST: 09:00 in
// New York is 14:00 UTC before the 2026-03-08 spring-forward and 13:00 UTC
// from that day on.
func TestParseCronTimezoneAndDST(t *testing.T) {
	expr, err := ParseCron("0 9 * * *", "America/New_York")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	winter := expr.Next(mustTime(t, "2026-02-28T15:00:00Z")).UTC().Format(time.RFC3339)
	summer := expr.Next(mustTime(t, "2026-03-07T15:00:00Z")).UTC().Format(time.RFC3339)
	if winter != "2026-03-01T14:00:00Z" || summer != "2026-03-08T13:00:00Z" {
		t.Fatalf("9:00 New York around DST: %s, %s", winter, summer)
	}
	expr, err = ParseCron("0 9 * * *", "America/Sao_Paulo")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := expr.Next(mustTime(t, "2026-10-08T13:00:00Z")).UTC().Format(time.RFC3339); got != "2026-10-09T12:00:00Z" {
		t.Fatalf("9:00 Sao Paulo = %s, want 2026-10-09T12:00:00Z", got)
	}
}

func TestParseCronRejectsBadInput(t *testing.T) {
	for _, tc := range []struct{ rule, tz, field string }{
		{"* * *", "", fieldCron},
		{"61 * * * *", "", fieldCron},
		{"* * * * * *", "", fieldCron}, // seconds field is not part of the dialect
		{"@sometimes", "", fieldCron},
		{"* * * * *", "Mars/Olympus", fieldTimezone},
	} {
		_, err := ParseCron(tc.rule, tc.tz)
		var v *schedule.ValidationError
		if !errors.As(err, &v) || v.Field != tc.field {
			t.Fatalf("ParseCron(%q, %q) = %v, want a %s validation error", tc.rule, tc.tz, err, tc.field)
		}
	}
}
