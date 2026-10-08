// Package schedule owns the tenant-scoped recurring schedule: a cron rule
// that enqueues one task per slot.
package schedule

import (
	"bytes"
	"encoding/json"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const (
	maxPriority     = 9
	maxAttempts     = 100
	maxNameLength   = 63
	fieldCron       = "cron"
	fieldTimezone   = "timezone"
	fieldCommand    = "command"
	fieldPayload    = "payload"
	fieldPriority   = "priority"
	fieldAttempts   = "maxAttempts"
	fieldWebhook    = "webhook"
	fieldName       = "name"
	fieldTenant     = "tenantId"
	nullPayload     = "null"
	cronTZPrefix    = "CRON_TZ="
	legacyTZPrefix  = "TZ="
	idSeparator     = "."
	slotKeyNullByte = "\x00"
)

var (
	tenantPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{1,62}$`)
	namePattern   = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
)

// Spec is the desired schedule, as a client writes it.
type Spec struct {
	// Cron is a standard five-field expression or a descriptor such as
	// "@hourly" or "@every 30s", evaluated in Timezone.
	Cron string `json:"cron"`
	// Timezone is an IANA name; empty means UTC.
	Timezone    string          `json:"timezone,omitempty"`
	Command     string          `json:"command"`
	Payload     json.RawMessage `json:"payload,omitempty"`
	Priority    int             `json:"priority,omitempty"`
	MaxAttempts int             `json:"maxAttempts,omitempty"`
	Webhook     string          `json:"webhook,omitempty"`
	// Paused keeps the schedule without firing; resuming starts from the
	// next slot after the resume, never catching up the paused period.
	Paused bool `json:"paused,omitempty"`
}

// Schedule is a persisted schedule: its spec, identity and firing state.
type Schedule struct {
	ScheduleID string `json:"scheduleId"`
	TenantID   string `json:"tenantId"`
	Name       string `json:"name"`
	Spec       Spec   `json:"spec"`
	// Version changes only when the spec changes; firing never bumps it.
	Version int64 `json:"version"`
	// NextRunAt is the next slot to fire; zero while paused.
	NextRunAt  time.Time  `json:"nextRunAt"`
	LastRunAt  *time.Time `json:"lastRunAt,omitempty"`
	LastTaskID string     `json:"lastTaskId,omitempty"`
	CreatedAt  time.Time  `json:"createdAt"`
	UpdatedAt  time.Time  `json:"updatedAt"`
}

// Expression is a parsed cron rule bound to its timezone.
type Expression interface {
	// Next returns the first slot strictly after t.
	Next(t time.Time) time.Time
}

// Parser turns a spec's cron rule and timezone into an Expression, or
// returns a *ValidationError naming the faulty field.
type Parser func(cron, timezone string) (Expression, error)

// New validates a desired spec and returns the schedule it describes, with
// its first slot computed from now (none while paused).
func New(tenantID, name string, spec Spec, parse Parser, now time.Time) (Schedule, error) {
	if err := ValidateIdentity(tenantID, name); err != nil {
		return Schedule{}, err
	}
	normalized, err := normalize(spec)
	if err != nil {
		return Schedule{}, err
	}
	expr, err := parse(normalized.Cron, normalized.Timezone)
	if err != nil {
		return Schedule{}, err
	}
	s := Schedule{
		ScheduleID: PhysicalID(tenantID, name),
		TenantID:   tenantID,
		Name:       name,
		Spec:       normalized,
		CreatedAt:  now.UTC(),
		UpdatedAt:  now.UTC(),
	}
	if !normalized.Paused {
		s.NextRunAt = expr.Next(now).UTC()
	}
	return s, nil
}

// PhysicalID is the catalog identifier. The tenant always comes from the
// authenticated request, never from the body.
func PhysicalID(tenantID, name string) string {
	return tenantID + idSeparator + name
}

// SameSpec reports semantic equality of two normalized specs.
func SameSpec(a, b Spec) bool {
	return a.Cron == b.Cron && a.Timezone == b.Timezone && a.Command == b.Command &&
		bytes.Equal(a.Payload, b.Payload) && a.Priority == b.Priority &&
		a.MaxAttempts == b.MaxAttempts && a.Webhook == b.Webhook && a.Paused == b.Paused
}

// SameTiming reports whether two specs fire on the same slots.
func SameTiming(a, b Spec) bool {
	return a.Cron == b.Cron && a.Timezone == b.Timezone && a.Paused == b.Paused
}

// SlotKey is the idempotency key of the task a schedule enqueues for one
// slot. Client keys may not contain NUL and binding keys never start with
// it, so a slot key cannot collide with either; a replay of the same slot,
// on any leader, resolves to the task already enqueued.
func SlotKey(scheduleID string, slot time.Time) string {
	return slotKeyNullByte + "schedule" + slotKeyNullByte + scheduleID + slotKeyNullByte + slot.UTC().Format(time.RFC3339)
}

// TaskPayload is the payload string handed to the scheduler: the spec's raw
// JSON, or JSON null when the spec has none.
func (s Spec) TaskPayload() string {
	if len(s.Payload) == 0 {
		return nullPayload
	}
	return string(s.Payload)
}

// ValidateTenant checks a tenant identifier.
func ValidateTenant(tenantID string) error {
	if !tenantPattern.MatchString(tenantID) {
		return &ValidationError{Field: fieldTenant, Message: "must match ^[a-z][a-z0-9-]{1,62}$"}
	}
	return nil
}

// ValidateIdentity checks the tenant and the schedule name.
func ValidateIdentity(tenantID, name string) error {
	if err := ValidateTenant(tenantID); err != nil {
		return err
	}
	if len(name) > maxNameLength || !namePattern.MatchString(name) {
		return &ValidationError{Field: fieldName, Message: "must be a DNS label with at most 63 characters"}
	}
	return nil
}

func normalize(spec Spec) (Spec, error) {
	spec.Cron = strings.TrimSpace(spec.Cron)
	spec.Timezone = strings.TrimSpace(spec.Timezone)
	spec.Command = strings.TrimSpace(spec.Command)
	if err := validateRule(spec); err != nil {
		return Spec{}, err
	}
	if err := validateTask(spec); err != nil {
		return Spec{}, err
	}
	if len(spec.Payload) > 0 {
		var compact bytes.Buffer
		if err := json.Compact(&compact, spec.Payload); err != nil {
			return Spec{}, &ValidationError{Field: fieldPayload, Message: "must be valid JSON"}
		}
		spec.Payload = compact.Bytes()
	}
	return spec, nil
}

func validateRule(spec Spec) error {
	if spec.Cron == "" {
		return &ValidationError{Field: fieldCron, Message: "is required"}
	}
	if strings.HasPrefix(spec.Cron, cronTZPrefix) || strings.HasPrefix(spec.Cron, legacyTZPrefix) {
		return &ValidationError{Field: fieldCron, Message: "must not embed a timezone; use the timezone field"}
	}
	if spec.Timezone != "" {
		if _, err := time.LoadLocation(spec.Timezone); err != nil {
			return &ValidationError{Field: fieldTimezone, Message: "must be an IANA timezone name"}
		}
	}
	return nil
}

func validateTask(spec Spec) error {
	if spec.Command == "" {
		return &ValidationError{Field: fieldCommand, Message: "is required"}
	}
	if spec.Priority < 0 || spec.Priority > maxPriority {
		return &ValidationError{Field: fieldPriority, Message: "must be between 0 and 9"}
	}
	if spec.MaxAttempts < 0 || spec.MaxAttempts > maxAttempts {
		return &ValidationError{Field: fieldAttempts, Message: "must be between 0 (server default) and 100"}
	}
	return validateWebhook(spec.Webhook)
}

func validateWebhook(webhook string) error {
	if webhook == "" {
		return nil
	}
	u, err := url.Parse(webhook)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return &ValidationError{Field: fieldWebhook, Message: "must be an absolute http(s) URL"}
	}
	return nil
}
