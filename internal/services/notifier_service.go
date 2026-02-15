package services

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/osvaldoandrade/codeq/internal/metrics"
	"github.com/osvaldoandrade/codeq/internal/repository"
	"github.com/osvaldoandrade/codeq/pkg/domain"
)

type NotifierService interface {
	NotifyQueueReady(ctx context.Context, cmd domain.Command)
}

type notifierService struct {
	repo      repository.SubscriptionRepository
	logger    *slog.Logger
	secret    string
	minNotify int
}

func NewNotifierService(repo repository.SubscriptionRepository, logger *slog.Logger, secret string, minNotify int) NotifierService {
	if minNotify <= 0 {
		minNotify = 5
	}
	return &notifierService{repo: repo, logger: logger, secret: secret, minNotify: minNotify}
}

func (n *notifierService) NotifyQueueReady(ctx context.Context, cmd domain.Command) {
	subs, err := n.repo.ListActive(ctx, cmd, time.Now())
	if err != nil || len(subs) == 0 {
		return
	}

	fanout := []domain.Subscription{}
	groups := map[string][]domain.Subscription{}
	hashMode := []domain.Subscription{}

	for _, s := range subs {
		switch s.DeliveryMode {
		case "fanout":
			fanout = append(fanout, s)
		case "group":
			groups[s.GroupID] = append(groups[s.GroupID], s)
		case "hash":
			hashMode = append(hashMode, s)
		default:
			fanout = append(fanout, s)
		}
	}

	for _, s := range fanout {
		if ok, _ := n.repo.AllowNotify(ctx, s.ID, s.MinIntervalSeconds); ok {
			n.dispatch(ctx, s, cmd)
		}
	}

	for groupID, list := range groups {
		if len(list) == 0 {
			continue
		}
		idx, err := n.repo.NextGroupIndex(ctx, cmd, groupID, len(list))
		if err != nil {
			continue
		}
		s := list[idx]
		if ok, _ := n.repo.AllowNotify(ctx, s.ID, s.MinIntervalSeconds); ok {
			n.dispatch(ctx, s, cmd)
		}
	}

	if len(hashMode) > 0 {
		idx := int(time.Now().UTC().Unix() / 60 % int64(len(hashMode)))
		s := hashMode[idx]
		if ok, _ := n.repo.AllowNotify(ctx, s.ID, s.MinIntervalSeconds); ok {
			n.dispatch(ctx, s, cmd)
		}
	}
}

func (n *notifierService) dispatch(ctx context.Context, sub domain.Subscription, cmd domain.Command) {
	payload := map[string]any{
		"eventType":      string(cmd),
		"available":      true,
		"queueDepth":     0,
		"claimUrl":       "/v1/codeq/tasks/claim",
		"sentAt":         time.Now().UTC().Format(time.RFC3339),
		"notificationId": "ntf-" + sub.ID,
	}
	b, _ := json.Marshal(payload)

	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, sub.CallbackURL, bytes.NewBuffer(b))
	req.Header.Set("Content-Type", "application/json")
	n.addSignature(req, b)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		metrics.WebhookDeliveriesTotal.WithLabelValues("queue_ready", string(cmd), "failure").Inc()
		n.logger.Warn("notify failed", "err", err)
		return
	}
	_ = resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		metrics.WebhookDeliveriesTotal.WithLabelValues("queue_ready", string(cmd), "success").Inc()
		return
	}
	metrics.WebhookDeliveriesTotal.WithLabelValues("queue_ready", string(cmd), "failure").Inc()
}

func (n *notifierService) addSignature(req *http.Request, body []byte) {
	if strings.TrimSpace(n.secret) == "" {
		return
	}
	ts := time.Now().UTC().Unix()
	mac := hmac.New(sha256.New, []byte(n.secret))
	_, _ = mac.Write([]byte(fmt.Sprintf("%d.", ts)))
	_, _ = mac.Write(body)
	sig := hex.EncodeToString(mac.Sum(nil))
	req.Header.Set("X-CodeQ-Timestamp", fmt.Sprintf("%d", ts))
	req.Header.Set("X-CodeQ-Signature", sig)
}
