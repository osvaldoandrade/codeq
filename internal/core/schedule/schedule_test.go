package schedule

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

type everyMinute struct{}

func (everyMinute) Next(t time.Time) time.Time { return t.Truncate(time.Minute).Add(time.Minute) }

func fakeParser(cron, _ string) (Expression, error) {
	if cron == badRule {
		return nil, &ValidationError{Field: fieldCron, Message: badRule}
	}
	return everyMinute{}, nil
}

const (
	badRule    = "bad"
	testTenant = "tenant-a"
)

var testNow = time.Date(2026, 10, 8, 10, 7, 30, 0, time.UTC)

func validSpec() Spec {
	return Spec{Cron: "* * * * *", Command: "SYNC", Payload: json.RawMessage(`{ "a" : 1 }`), Priority: 5}
}

func TestNewNormalizesAndComputesFirstSlot(t *testing.T) {
	s, err := New(testTenant, "nightly-sync", validSpec(), fakeParser, testNow)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if s.ScheduleID != "tenant-a.nightly-sync" || string(s.Spec.Payload) != `{"a":1}` {
		t.Fatalf("identity/payload = %q %q", s.ScheduleID, s.Spec.Payload)
	}
	if !s.NextRunAt.Equal(time.Date(2026, 10, 8, 10, 8, 0, 0, time.UTC)) {
		t.Fatalf("NextRunAt = %s", s.NextRunAt)
	}
	paused := validSpec()
	paused.Paused = true
	p, err := New(testTenant, "nightly-sync", paused, fakeParser, testNow)
	if err != nil || !p.NextRunAt.IsZero() {
		t.Fatalf("paused schedule NextRunAt = %s, %v; want zero", p.NextRunAt, err)
	}
}

func TestNewRejectsInvalidSpecs(t *testing.T) {
	mutate := func(f func(*Spec)) Spec { s := validSpec(); f(&s); return s }
	for _, tc := range []struct {
		name, tenant, schedule string
		spec                   Spec
		field                  string
	}{
		{"bad tenant", "Tenant", "x", validSpec(), fieldTenant},
		{"bad name", testTenant, "Not_A_Label", validSpec(), fieldName},
		{"long name", testTenant, strings.Repeat("a", 64), validSpec(), fieldName},
		{"no cron", testTenant, "x", mutate(func(s *Spec) { s.Cron = " " }), fieldCron},
		{"embedded tz", testTenant, "x", mutate(func(s *Spec) { s.Cron = "CRON_TZ=UTC * * * * *" }), fieldCron},
		{"parser error", testTenant, "x", mutate(func(s *Spec) { s.Cron = badRule }), fieldCron},
		{"bad timezone", testTenant, "x", mutate(func(s *Spec) { s.Timezone = "Nowhere/City" }), fieldTimezone},
		{"no command", testTenant, "x", mutate(func(s *Spec) { s.Command = "" }), fieldCommand},
		{"priority", testTenant, "x", mutate(func(s *Spec) { s.Priority = 10 }), fieldPriority},
		{"attempts", testTenant, "x", mutate(func(s *Spec) { s.MaxAttempts = 101 }), fieldAttempts},
		{"webhook", testTenant, "x", mutate(func(s *Spec) { s.Webhook = "ftp://x" }), fieldWebhook},
		{"payload", testTenant, "x", mutate(func(s *Spec) { s.Payload = json.RawMessage(`{`) }), fieldPayload},
	} {
		_, err := New(tc.tenant, tc.schedule, tc.spec, fakeParser, testNow)
		var v *ValidationError
		if !errors.As(err, &v) || v.Field != tc.field {
			t.Fatalf("%s: err %v, want a %s validation error", tc.name, err, tc.field)
		}
	}
}

func TestSameSpecAndTiming(t *testing.T) {
	a, _ := New(testTenant, "x", validSpec(), fakeParser, testNow)
	b, _ := New(testTenant, "x", validSpec(), fakeParser, testNow.Add(time.Hour))
	if !SameSpec(a.Spec, b.Spec) {
		t.Fatal("identical specs (after payload normalization) must compare equal")
	}
	c := validSpec()
	c.Priority = 1
	cs, _ := New(testTenant, "x", c, fakeParser, testNow)
	if SameSpec(a.Spec, cs.Spec) || !SameTiming(a.Spec, cs.Spec) {
		t.Fatal("a priority change is a spec change with the same timing")
	}
	d := validSpec()
	d.Timezone = "America/Sao_Paulo"
	ds, _ := New(testTenant, "x", d, fakeParser, testNow)
	if SameTiming(a.Spec, ds.Spec) {
		t.Fatal("a timezone change is a timing change")
	}
}

// Slot keys are distinct per schedule and slot, start with NUL so no client
// or binding idempotency key can equal them, and ignore the slot's zone.
func TestSlotKey(t *testing.T) {
	slot := time.Date(2026, 10, 8, 10, 8, 0, 0, time.UTC)
	k := SlotKey("tenant-a.x", slot)
	if !strings.HasPrefix(k, "\x00") {
		t.Fatalf("slot key %q must start with NUL", k)
	}
	if SlotKey("tenant-a.x", slot.In(time.FixedZone("BRT", -3*3600))) != k {
		t.Fatal("the same instant in another zone must give the same key")
	}
	if SlotKey("tenant-a.x", slot.Add(time.Minute)) == k || SlotKey("tenant-b.x", slot) == k {
		t.Fatal("another slot or schedule must give another key")
	}
	if (Spec{}).TaskPayload() != "null" || validSpec().TaskPayload() == "null" {
		t.Fatal("TaskPayload: empty → null, set → the raw JSON")
	}
}
