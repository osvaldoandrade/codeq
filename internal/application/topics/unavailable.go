package topics

import (
	"context"

	"github.com/osvaldoandrade/codeq/internal/core/queuetopic"
)

type unavailableStore struct {
	reason string
}

// NewUnavailableService returns a service that fails closed for storage modes
// where topic writes are not replicated safely.
func NewUnavailableService(reason string) *Service {
	return NewService(&unavailableStore{reason: reason}, nil)
}

// Upsert fails closed because this store has no safe persistence path.
func (s *unavailableStore) Upsert(context.Context, queuetopic.Topic) (queuetopic.Topic, bool, bool, error) {
	return queuetopic.Topic{}, false, false, &queuetopic.UnavailableError{Reason: s.reason}
}

// Get fails closed because this store has no safe persistence path.
func (s *unavailableStore) Get(context.Context, string, string) (queuetopic.Topic, error) {
	return queuetopic.Topic{}, &queuetopic.UnavailableError{Reason: s.reason}
}

// Delete fails closed because this store has no safe persistence path.
func (s *unavailableStore) Delete(context.Context, string, string) error {
	return &queuetopic.UnavailableError{Reason: s.reason}
}
