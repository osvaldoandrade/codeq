package queuetopic

import (
	"errors"
	"strings"
	"testing"
	"time"
)

const (
	testDeadLetter       = "events-dlq"
	testEventsTopic      = "events"
	testPaymentsTenant   = "payments"
	testPhysicalTopicID  = "payments.events"
	testFieldDeadLetter  = "deadLetterTopicRef"
	testFieldMaxAttempts = "maxAttempts"
	testFieldMaxConsumer = "maxConsumers"
	testFieldPriority    = "priorityTiers"
	testFieldRetention   = "retentionSeconds"
	testFieldTopicName   = "topicName"
)

func TestNewNormalizesValidTopic(t *testing.T) {
	now := time.Date(2026, 7, 20, 12, 30, 0, 0, time.FixedZone("local", -3*60*60))
	policy := Policy{
		PriorityTiers:      []int{5, 1, 3},
		MaxAttempts:        5,
		DeadLetterTopicRef: "payments-events-dlq",
		RetentionSeconds:   3600,
		MaxConsumers:       20,
	}

	topic, err := New("payments", "payments-events", policy, now)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if topic.TopicID != "payments.payments-events" {
		t.Fatalf("TopicID = %q", topic.TopicID)
	}
	if got := topic.Policy.PriorityTiers; len(got) != 3 || got[0] != 1 || got[1] != 3 || got[2] != 5 {
		t.Fatalf("PriorityTiers = %v", got)
	}
	if !topic.CreatedAt.Equal(now) || topic.CreatedAt.Location() != time.UTC || !topic.UpdatedAt.Equal(now) {
		t.Fatalf("timestamps = %s / %s", topic.CreatedAt, topic.UpdatedAt)
	}
	if policy.PriorityTiers[0] != 5 {
		t.Fatal("New() mutated the caller's priority slice")
	}
	if !SamePolicy(topic.Policy, topic.Policy) {
		t.Fatal("SamePolicy() = false for identical policies")
	}
	changed := topic.Policy
	changed.MaxConsumers++
	if SamePolicy(topic.Policy, changed) {
		t.Fatal("SamePolicy() = true after policy change")
	}
}

func TestNewRejectsInvalidContracts(t *testing.T) {
	valid := Policy{PriorityTiers: []int{0, 3}, MaxAttempts: 5, DeadLetterTopicRef: testDeadLetter}
	tests := []struct {
		name      string
		tenantID  string
		topicName string
		policy    Policy
		field     string
	}{
		{name: "tenant", tenantID: "P", topicName: testEventsTopic, policy: valid, field: "tenantId"},
		{name: "topic pattern", tenantID: testPaymentsTenant, topicName: "bad.topic", policy: valid, field: testFieldTopicName},
		{name: "topic length", tenantID: testPaymentsTenant, topicName: strings.Repeat("a", 64), policy: valid, field: testFieldTopicName},
		{name: "no priorities", tenantID: testPaymentsTenant, topicName: testEventsTopic, policy: Policy{MaxAttempts: 5, DeadLetterTopicRef: testDeadLetter}, field: testFieldPriority},
		{name: "too many priorities", tenantID: testPaymentsTenant, topicName: testEventsTopic, policy: Policy{PriorityTiers: []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10}, MaxAttempts: 5, DeadLetterTopicRef: testDeadLetter}, field: testFieldPriority},
		{name: "priority range", tenantID: testPaymentsTenant, topicName: testEventsTopic, policy: Policy{PriorityTiers: []int{11}, MaxAttempts: 5, DeadLetterTopicRef: testDeadLetter}, field: testFieldPriority},
		{name: "duplicate priority", tenantID: testPaymentsTenant, topicName: testEventsTopic, policy: Policy{PriorityTiers: []int{1, 1}, MaxAttempts: 5, DeadLetterTopicRef: testDeadLetter}, field: testFieldPriority},
		{name: "attempts low", tenantID: testPaymentsTenant, topicName: testEventsTopic, policy: Policy{PriorityTiers: []int{1}, DeadLetterTopicRef: testDeadLetter}, field: testFieldMaxAttempts},
		{name: "attempts high", tenantID: testPaymentsTenant, topicName: testEventsTopic, policy: Policy{PriorityTiers: []int{1}, MaxAttempts: 101, DeadLetterTopicRef: testDeadLetter}, field: testFieldMaxAttempts},
		{name: "dlq pattern", tenantID: testPaymentsTenant, topicName: testEventsTopic, policy: Policy{PriorityTiers: []int{1}, MaxAttempts: 5, DeadLetterTopicRef: "bad.topic"}, field: testFieldDeadLetter},
		{name: "dlq length", tenantID: testPaymentsTenant, topicName: testEventsTopic, policy: Policy{PriorityTiers: []int{1}, MaxAttempts: 5, DeadLetterTopicRef: strings.Repeat("a", 64)}, field: testFieldDeadLetter},
		{name: "dlq self", tenantID: testPaymentsTenant, topicName: testEventsTopic, policy: Policy{PriorityTiers: []int{1}, MaxAttempts: 5, DeadLetterTopicRef: testEventsTopic}, field: testFieldDeadLetter},
		{name: "retention low", tenantID: testPaymentsTenant, topicName: testEventsTopic, policy: Policy{PriorityTiers: []int{1}, MaxAttempts: 5, DeadLetterTopicRef: testDeadLetter, RetentionSeconds: -1}, field: testFieldRetention},
		{name: "retention high", tenantID: testPaymentsTenant, topicName: testEventsTopic, policy: Policy{PriorityTiers: []int{1}, MaxAttempts: 5, DeadLetterTopicRef: testDeadLetter, RetentionSeconds: 2147483647 + 1}, field: testFieldRetention},
		{name: "consumers low", tenantID: testPaymentsTenant, topicName: testEventsTopic, policy: Policy{PriorityTiers: []int{1}, MaxAttempts: 5, DeadLetterTopicRef: testDeadLetter, MaxConsumers: -1}, field: testFieldMaxConsumer},
		{name: "consumers high", tenantID: testPaymentsTenant, topicName: testEventsTopic, policy: Policy{PriorityTiers: []int{1}, MaxAttempts: 5, DeadLetterTopicRef: testDeadLetter, MaxConsumers: 10001}, field: testFieldMaxConsumer},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := New(tt.tenantID, tt.topicName, tt.policy, time.Now())
			var validation *ValidationError
			if !errors.As(err, &validation) || validation.Field != tt.field {
				t.Fatalf("error = %#v, want ValidationError for %s", err, tt.field)
			}
			if validation.Error() == "" {
				t.Fatal("ValidationError.Error() is empty")
			}
		})
	}
}

func TestTypedErrors(t *testing.T) {
	tests := []error{
		&NotFoundError{TopicID: testPhysicalTopicID},
		&ConflictError{TopicID: testPhysicalTopicID},
		&UnavailableError{Reason: "replication disabled"},
	}
	for _, err := range tests {
		if err.Error() == "" {
			t.Fatalf("%T.Error() is empty", err)
		}
	}
	if got := PhysicalID("payments", "events"); got != "payments.events" {
		t.Fatalf("PhysicalID() = %q", got)
	}
}
