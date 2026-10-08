// Package schedules implements recurring schedule administration and firing.
package schedules

import (
	"time"

	"github.com/robfig/cron/v3"

	"github.com/osvaldoandrade/codeq/internal/core/schedule"
)

const (
	fieldCron     = "cron"
	fieldTimezone = "timezone"
)

// cronParser accepts the five standard fields and the descriptors
// (@hourly, @daily, @every 30s, ...), the dialect Kubernetes CronJob uses.
var cronParser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)

type cronExpression struct {
	rule cron.Schedule
	loc  *time.Location
}

// Next returns the first slot strictly after t, evaluated in the
// expression's timezone so wall-clock rules follow its DST transitions.
func (e cronExpression) Next(t time.Time) time.Time {
	return e.rule.Next(t.In(e.loc))
}

// ParseCron is the schedule.Parser of the server: a cron rule evaluated in
// an IANA timezone (UTC when empty).
func ParseCron(rule, timezone string) (schedule.Expression, error) {
	loc := time.UTC
	if timezone != "" {
		l, err := time.LoadLocation(timezone)
		if err != nil {
			return nil, &schedule.ValidationError{Field: fieldTimezone, Message: "must be an IANA timezone name"}
		}
		loc = l
	}
	parsed, err := cronParser.Parse(rule)
	if err != nil {
		return nil, &schedule.ValidationError{Field: fieldCron, Message: err.Error()}
	}
	return cronExpression{rule: parsed, loc: loc}, nil
}
