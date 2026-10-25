package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/osvaldoandrade/codeq/internal/storage/diskguard"
	"github.com/osvaldoandrade/codeq/pkg/config"
	"github.com/osvaldoandrade/codeq/pkg/domain"
)

const storageGuardCommand = "storage-guard"

// simulatedVolume reports a 100 MiB volume whose used share the test sets.
type simulatedVolume struct{ usedMiB atomic.Int64 }

func (v *simulatedVolume) stat(string) (diskguard.Usage, error) {
	const total = 100 << 20
	used := uint64(v.usedMiB.Load()) << 20 // #nosec G115 -- small test value
	return diskguard.Usage{TotalBytes: total, FreeBytes: total - used}, nil
}

func useStorageStat(t *testing.T, stat diskguard.StatFunc) {
	t.Helper()
	prev := storageStat
	storageStat = stat
	t.Cleanup(func() { storageStat = prev })
}

func newStorageGuardServer(t *testing.T, path string, mutate func(*config.Config)) *httptest.Server {
	t.Helper()
	cfg := newTopicPebbleConfig(t, path)
	cfg.Storage = config.StorageConfig{AdmissionMaxUsedFraction: 0.80, CheckIntervalSeconds: 1}
	if mutate != nil {
		mutate(cfg)
	}
	a, err := NewApplication(cfg)
	if err != nil {
		t.Fatalf("NewApplication: %v", err)
	}
	SetupMappings(a)
	srv := httptest.NewServer(a.Engine)
	t.Cleanup(func() { srv.Close(); _ = a.TracingShutdown(context.Background()) })
	return srv
}

func TestStorageGuardHTTPRefusesCreatesButDrains(t *testing.T) {
	vol := &simulatedVolume{}
	vol.usedMiB.Store(50)
	useStorageStat(t, vol.stat)
	srv := newStorageGuardServer(t, t.TempDir()+"/pebble", nil)
	ctx := context.Background()
	create := func(n int) (int, string) {
		body := map[string]any{keyCommand: storageGuardCommand, keyPayload: map[string]int{"n": n}}
		return doJSON(t, ctx, http.MethodPost, srv.URL+"/v1/codeq/tasks", "dev-token", body, nil)
	}
	if status, raw := create(1); status != http.StatusAccepted {
		t.Fatalf("create below threshold: %d %s", status, raw)
	}

	vol.usedMiB.Store(85)
	time.Sleep(1100 * time.Millisecond) // let the 1s admission sample expire

	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/v1/codeq/tasks",
		strings.NewReader(`{"command":"`+storageGuardCommand+`","payload":{"n":2}}`))
	req.Header.Set("Authorization", "Bearer dev-token")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var refused map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&refused)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests || refused["error"] != domain.StorageCapacityCode || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("create above threshold: %d %v retry=%q", resp.StatusCode, refused, resp.Header.Get("Retry-After"))
	}

	batch := map[string]any{"tasks": []map[string]any{{keyCommand: storageGuardCommand, keyPayload: map[string]int{"n": 3}}}}
	var batchResp struct {
		Results []struct {
			Error string `json:"error"`
		} `json:"results"`
	}
	if status, raw := doJSON(t, ctx, http.MethodPost, srv.URL+"/v1/codeq/tasks/batch", "dev-token", batch, &batchResp); status != http.StatusOK ||
		len(batchResp.Results) != 1 || batchResp.Results[0].Error != domain.StorageCapacityCode {
		t.Fatalf("batch above threshold: %d %s", status, raw)
	}

	// Workers keep draining: claim and complete the accepted task.
	claim := map[string]any{keyCommands: []string{storageGuardCommand}, fwdLease: 60}
	var claimed domain.Task
	if status, raw := doJSON(t, ctx, http.MethodPost, srv.URL+"/v1/codeq/tasks/claim", "dev-token", claim, &claimed); status != http.StatusOK {
		t.Fatalf("claim above threshold: %d %s", status, raw)
	}
	result := map[string]any{"status": string(domain.StatusCompleted), "result": map[string]any{"ok": true}}
	if status, raw := doJSON(t, ctx, http.MethodPost, srv.URL+"/v1/codeq/tasks/"+claimed.ID+"/result", "dev-token", result, nil); status != http.StatusOK {
		t.Fatalf("complete above threshold: %d %s", status, raw)
	}
	if v, ok := gaugeValue(t, "codeq_storage_admission_blocked", ""); !ok || v != 1 {
		t.Fatal("storage_admission_blocked gauge not raised")
	}

	vol.usedMiB.Store(40)
	time.Sleep(1100 * time.Millisecond)
	if status, raw := create(4); status != http.StatusAccepted {
		t.Fatalf("create after recovery: %d %s", status, raw)
	}
}

func TestStorageGuardStartupReleasesReserveOnFullVolume(t *testing.T) {
	path := t.TempDir() + "/pebble"
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
	reserve := filepath.Join(path, diskguard.ReserveFileName)
	if err := os.WriteFile(reserve, make([]byte, 4<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	// A 100 MiB volume that is completely full while the reserve exists.
	useStorageStat(t, func(string) (diskguard.Usage, error) {
		const total = 100 << 20
		if _, err := os.Stat(reserve); err == nil {
			return diskguard.Usage{TotalBytes: total, FreeBytes: 0}, nil
		}
		return diskguard.Usage{TotalBytes: total, FreeBytes: 4 << 20}, nil
	})
	reserveBytes := int64(4 << 20)
	srv := newStorageGuardServer(t, path, func(c *config.Config) { c.Storage.ReserveBytes = &reserveBytes })
	if _, err := os.Stat(reserve); !os.IsNotExist(err) {
		t.Fatalf("reserve survived startup on a full volume: %v", err)
	}
	body := map[string]any{keyCommand: storageGuardCommand, keyPayload: map[string]int{"n": 1}}
	if status, raw := doJSON(t, context.Background(), http.MethodPost, srv.URL+"/v1/codeq/tasks", "dev-token", body, nil); status != http.StatusTooManyRequests || !strings.Contains(raw, domain.StorageCapacityCode) {
		t.Fatalf("create on a 96%%-full volume: %d %s", status, raw)
	}
}

func TestPebbleSizedFromMemoryLimitAndEngineMetricsExported(t *testing.T) {
	_ = newStorageGuardServer(t, t.TempDir()+"/pebble", func(c *config.Config) {
		c.Storage = config.StorageConfig{} // guard disabled: engine metrics still flow
		c.Memory.LimitBytes = 512 << 20
	})
	if got, _ := gaugeValue(t, "codeq_pebble_cache_capacity_bytes", "0"); got != 76<<20 {
		t.Fatalf("cache = %v, want 76 MiB", got)
	}
	if got, _ := gaugeValue(t, "codeq_pebble_memtable_bytes", "0"); got != 16<<20 {
		t.Fatalf("memtable = %v, want 16 MiB", got)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, debt := gaugeValue(t, "codeq_pebble_compaction_debt_bytes", "0")
		_, l0 := gaugeValue(t, "codeq_pebble_l0_files", "0")
		if usage, _ := gaugeValue(t, "codeq_pebble_disk_usage_bytes", "0"); debt && l0 && usage > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("engine metrics were not published")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// gaugeValue reads one gauge from the default registry; shard filters the
// "shard" label when non-empty.
func gaugeValue(t *testing.T, name, shard string) (float64, bool) {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range families {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			if shard == "" || labelValue(m.GetLabel(), "shard") == shard {
				return m.GetGauge().GetValue(), true
			}
		}
	}
	return 0, false
}

func labelValue[L interface {
	GetName() string
	GetValue() string
}](labels []L, name string) string {
	for _, l := range labels {
		if l.GetName() == name {
			return l.GetValue()
		}
	}
	return ""
}
