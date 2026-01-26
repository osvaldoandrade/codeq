package services

import (
	"context"
	"errors"
	"net/url"

	"github.com/osvaldoandrade/codeq/pkg/domain"
	"github.com/osvaldoandrade/codeq/internal/repository"
)

type SubscriptionService interface {
	Create(ctx context.Context, callbackURL string, eventTypes []domain.Command, deliveryMode string, groupID string, ttlSeconds int, minIntervalSeconds int) (*domain.Subscription, error)
	Heartbeat(ctx context.Context, id string, ttlSeconds int) (*domain.Subscription, error)
}

type subscriptionService struct {
	repo repository.SubscriptionRepository
}

func NewSubscriptionService(repo repository.SubscriptionRepository) SubscriptionService {
	return &subscriptionService{repo: repo}
}

func (s *subscriptionService) Create(ctx context.Context, callbackURL string, eventTypes []domain.Command, deliveryMode string, groupID string, ttlSeconds int, minIntervalSeconds int) (*domain.Subscription, error) {
	if callbackURL == "" {
		return nil, errors.New("callbackUrl is required")
	}
	u, err := url.Parse(callbackURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, errors.New("invalid callback url")
	}
	if len(eventTypes) == 0 {
		eventTypes = []domain.Command{domain.CmdGenerateMaster, domain.CmdGenerateCreative}
	}
	if deliveryMode == "" {
		deliveryMode = "fanout"
	}
	switch deliveryMode {
	case "fanout", "group", "hash":
	default:
		return nil, errors.New("invalid deliveryMode")
	}
	if deliveryMode == "group" && groupID == "" {
		return nil, errors.New("groupId is required")
	}

	sub := domain.Subscription{
		CallbackURL:        callbackURL,
		EventTypes:         eventTypes,
		DeliveryMode:       deliveryMode,
		GroupID:            groupID,
		MinIntervalSeconds: minIntervalSeconds,
	}
	return s.repo.Create(ctx, sub, ttlSeconds)
}

func (s *subscriptionService) Heartbeat(ctx context.Context, id string, ttlSeconds int) (*domain.Subscription, error) {
	return s.repo.Heartbeat(ctx, id, ttlSeconds)
}
