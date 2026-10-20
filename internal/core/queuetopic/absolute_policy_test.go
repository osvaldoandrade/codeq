package queuetopic

import (
	"testing"
	"time"
)

func TestAbsoluteExpiryPolicyValidationAndEquality(t *testing.T) {
	now := time.Now()
	base := Policy{PriorityTiers: []int{0}, MaxAttempts: 100, DeadLetterTopicRef: "logs-dlq", RetentionSeconds: 3600, AbsoluteExpiry: true}
	for _, tc := range []struct {
		name   string
		change func(*Policy)
	}{
		{"missing-retention", func(p *Policy) { p.RetentionSeconds = 0 }},
		{"floor-without-opt-in", func(p *Policy) { p.AbsoluteExpiry = false; value := now.Add(-time.Minute); p.VisibilityFloor = &value }},
		{"future-floor", func(p *Policy) { value := now.Add(time.Minute); p.VisibilityFloor = &value }},
		{"empty-floor", func(p *Policy) { value := time.Time{}; p.VisibilityFloor = &value }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			policy := base
			tc.change(&policy)
			if _, err := New("tenant-a", "logs", policy, now); err == nil {
				t.Fatal("invalid policy accepted")
			}
		})
	}
	first, err := New("tenant-a", "logs", base, now)
	if err != nil {
		t.Fatal(err)
	}
	other := base
	other.AbsoluteExpiry = false
	if SamePolicy(first.Policy, other) {
		t.Fatal("expiry mode ignored")
	}
	floor := now.Add(-time.Minute)
	other = base
	other.VisibilityFloor = &floor
	if SamePolicy(base, other) {
		t.Fatal("floor ignored")
	}
	copy := floor
	equal := other
	equal.VisibilityFloor = &copy
	if !SamePolicy(other, equal) {
		t.Fatal("equal cutoff changed")
	}
}
