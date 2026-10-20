// Package memory provides the non-durable test persistence plugin.
package memory

import (
	"context"
	"testing"
	"time"

	"github.com/osvaldoandrade/codeq/pkg/domain"
	"github.com/osvaldoandrade/codeq/pkg/persistence"
)

func TestMemoryRejectsAbsoluteExpiryWithoutDurableAcknowledgement(t *testing.T) {
	plugin, err := NewPlugin(persistence.PluginConfig{Timezone: time.UTC})
	if err != nil {
		t.Fatal(err)
	}
	defer plugin.Close()
	deadline := time.Now().Add(time.Hour)
	task := &domain.Task{ID: "logs-ephemeral", Command: "cf-logs", Payload: `{}`, ExpiresAt: &deadline}
	if err = plugin.TaskStorage().Save(context.Background(), task); err == nil {
		t.Fatal("non-durable save acknowledged")
	}
	if err = plugin.TaskStorage().EnqueueTask(context.Background(), task); err == nil {
		t.Fatal("non-durable enqueue acknowledged")
	}
	if _, err = plugin.TaskStorage().Get(context.Background(), task.ID); err == nil {
		t.Fatal("refused record retained")
	}
}
