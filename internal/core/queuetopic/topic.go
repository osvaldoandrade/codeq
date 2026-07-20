// Package queuetopic owns the tenant-scoped queue topic policy.
package queuetopic

import (
	"regexp"
	"slices"
	"time"
)

const (
	maxAttempts        = 100
	maxConsumers       = 10000
	maxPriority        = 10
	maxPriorityTiers   = 10
	maxRetentionSecond = 2147483647
	fieldDeadLetter    = "deadLetterTopicRef"
	fieldMaxAttempts   = "maxAttempts"
	fieldMaxConsumers  = "maxConsumers"
	fieldPriorityTiers = "priorityTiers"
	fieldRetention     = "retentionSeconds"
	fieldTopicName     = "topicName"
)

var (
	tenantPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{1,62}$`)
	namePattern   = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
)

// Policy is the provider contract stored for a QueueTopic.
type Policy struct {
	PriorityTiers      []int  `json:"priorityTiers"`
	MaxAttempts        int    `json:"maxAttempts"`
	DeadLetterTopicRef string `json:"deadLetterTopicRef"`
	RetentionSeconds   int    `json:"retentionSeconds,omitempty"`
	MaxConsumers       int    `json:"maxConsumers,omitempty"`
}

// Topic is a persisted, tenant-scoped QueueTopic realization.
type Topic struct {
	TopicID   string    `json:"topicId"`
	TenantID  string    `json:"tenantId"`
	TopicName string    `json:"topicName"`
	Policy    Policy    `json:"policy"`
	Version   int64     `json:"version"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// New validates and normalizes a desired topic.
func New(tenantID, topicName string, policy Policy, now time.Time) (Topic, error) {
	if err := ValidateIdentity(tenantID, topicName); err != nil {
		return Topic{}, err
	}
	if err := validatePolicy(topicName, policy); err != nil {
		return Topic{}, err
	}

	normalized := policy
	normalized.PriorityTiers = slices.Clone(policy.PriorityTiers)
	slices.Sort(normalized.PriorityTiers)

	return Topic{
		TopicID:   PhysicalID(tenantID, topicName),
		TenantID:  tenantID,
		TopicName: topicName,
		Policy:    normalized,
		CreatedAt: now.UTC(),
		UpdatedAt: now.UTC(),
	}, nil
}

// PhysicalID returns the provider identifier. Tenant ownership is never read
// from the request body; it is supplied by the authenticated request context.
func PhysicalID(tenantID, topicName string) string {
	return tenantID + "." + topicName
}

// SamePolicy reports semantic equality after New has normalized the priorities.
func SamePolicy(left, right Policy) bool {
	return left.MaxAttempts == right.MaxAttempts &&
		left.DeadLetterTopicRef == right.DeadLetterTopicRef &&
		left.RetentionSeconds == right.RetentionSeconds &&
		left.MaxConsumers == right.MaxConsumers &&
		slices.Equal(left.PriorityTiers, right.PriorityTiers)
}

// ValidateIdentity checks the tenant and logical provider name without
// requiring a policy payload.
func ValidateIdentity(tenantID, topicName string) error {
	if !tenantPattern.MatchString(tenantID) {
		return &ValidationError{Field: "tenantId", Message: "must match ^[a-z][a-z0-9-]{1,62}$"}
	}
	if len(topicName) > 63 || !namePattern.MatchString(topicName) {
		return &ValidationError{Field: fieldTopicName, Message: "must be a DNS label with at most 63 characters"}
	}
	return nil
}

func validatePolicy(topicName string, policy Policy) error {
	if err := validatePriorities(policy.PriorityTiers); err != nil {
		return err
	}
	if err := validateRetryAndDLQ(topicName, policy); err != nil {
		return err
	}
	return validateLimits(policy)
}

func validatePriorities(priorities []int) error {
	if len(priorities) == 0 || len(priorities) > maxPriorityTiers {
		return &ValidationError{Field: fieldPriorityTiers, Message: "must contain between 1 and 10 priorities"}
	}
	seen := make(map[int]struct{}, len(priorities))
	for _, priority := range priorities {
		if priority < 0 || priority > maxPriority {
			return &ValidationError{Field: fieldPriorityTiers, Message: "priorities must be integers from 0 through 10"}
		}
		if _, exists := seen[priority]; exists {
			return &ValidationError{Field: fieldPriorityTiers, Message: "priorities must be unique"}
		}
		seen[priority] = struct{}{}
	}
	return nil
}

func validateRetryAndDLQ(topicName string, policy Policy) error {
	if policy.MaxAttempts < 1 || policy.MaxAttempts > maxAttempts {
		return &ValidationError{Field: fieldMaxAttempts, Message: "must be between 1 and 100"}
	}
	if len(policy.DeadLetterTopicRef) > 63 || !namePattern.MatchString(policy.DeadLetterTopicRef) {
		return &ValidationError{Field: fieldDeadLetter, Message: "must be a DNS label with at most 63 characters"}
	}
	if policy.DeadLetterTopicRef == topicName {
		return &ValidationError{Field: fieldDeadLetter, Message: "must differ from topicName"}
	}
	return nil
}

func validateLimits(policy Policy) error {
	if policy.RetentionSeconds < 0 || policy.RetentionSeconds > maxRetentionSecond {
		return &ValidationError{Field: fieldRetention, Message: "must be between 0 and 2147483647"}
	}
	if policy.MaxConsumers < 0 || policy.MaxConsumers > maxConsumers {
		return &ValidationError{Field: fieldMaxConsumers, Message: "must be between 0 and 10000"}
	}
	return nil
}
