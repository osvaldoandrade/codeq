package memory

import (
	"testing"
	"time"

	"github.com/osvaldoandrade/codeq/pkg/persistence"
	"github.com/osvaldoandrade/codeq/pkg/persistence/persistencetest"
)

// TestMemoryPluginContractTests runs the shared contract test suite against the
// Memory plugin to ensure it satisfies the same behavioral contract as all other
// persistence backends.
func TestMemoryPluginContractTests(t *testing.T) {
	cfg := persistence.PluginConfig{
		Config:             []byte("{}"),
		Timezone:           time.UTC,
		BackoffPolicy:      "exp_full_jitter",
		BackoffBaseSeconds: 5,
		BackoffMaxSeconds:  900,
	}

	plugin, err := NewPlugin(cfg)
	if err != nil {
		t.Fatalf("Failed to create memory plugin: %v", err)
	}
	defer plugin.Close()

	persistencetest.RunPluginContractTests(t, plugin)
}
