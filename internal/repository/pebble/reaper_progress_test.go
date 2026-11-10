package pebble

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/osvaldoandrade/codeq/pkg/domain"
)

// reaperTicks reads codeq_reaper_ticks_total{sweep,outcome}.
func reaperTicks(t *testing.T, sweep, outcome string) float64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() != "codeq_reaper_ticks_total" {
			continue
		}
		for _, m := range family.GetMetric() {
			labels := map[string]string{}
			for _, l := range m.GetLabel() {
				labels[l.GetName()] = l.GetValue()
			}
			if labels["sweep"] == sweep && labels["outcome"] == outcome {
				return m.GetCounter().GetValue()
			}
		}
	}
	return 0
}

func TestStallWatchWarnsAfterSustainedStallAndRateLimits(t *testing.T) {
	var w stallWatch
	t0 := time.Unix(1_000_000, 0)
	if w.observe(t0, outcomeStalled) {
		t.Fatal("first stalled sweep warned")
	}
	if w.observe(t0.Add(30*time.Second), outcomeStalled) {
		t.Fatal("warned before the stall lasted stallWarnAfter")
	}
	if !w.observe(t0.Add(stallWarnAfter), outcomeStalled) {
		t.Fatal("sustained stall did not warn")
	}
	if w.observe(t0.Add(stallWarnAfter+time.Minute), outcomeStalled) {
		t.Fatal("stall warning not rate limited")
	}
	if w.observe(t0.Add(stallWarnAfter+2*time.Minute), outcomeProgress) {
		t.Fatal("progress warned")
	}
	if w.observe(t0.Add(stallWarnAfter+3*time.Minute), outcomeIdle) || !w.since.IsZero() {
		t.Fatal("idle sweep did not reset the stall")
	}
}

func TestSweepResultOutcome(t *testing.T) {
	for _, tc := range []struct {
		res  sweepResult
		want string
	}{
		{sweepResult{}, outcomeIdle},
		{sweepResult{due: 3}, outcomeStalled},
		{sweepResult{due: 3, done: 1}, outcomeProgress},
		{sweepResult{done: 2}, outcomeProgress},
	} {
		if got := tc.res.outcome(); got != tc.want {
			t.Errorf("%+v outcome=%s want %s", tc.res, got, tc.want)
		}
	}
}

// A due TTL entry the sweep cannot drop (its body does not decode) stays at
// the head of the index and is re-read by every sweep. The reaper counts each
// such sweep as stalled and, once the stall lasts, logs one rate-limited
// warning naming the sweep and the work it keeps re-reading.
func TestReaperReportsStalledSweep(t *testing.T) {
	db := openTestDB(t)
	stuck := "stuck-body"
	if err := db.Set(KeyTask(stuck), []byte("{not json")); err != nil {
		t.Fatal(err)
	}
	if err := db.Set(KeyTTLIndex(uint64(time.Now().Add(-time.Hour).Unix()), stuck), nil); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	clock := &atomic.Int64{}
	clock.Store(time.Now().UnixNano())
	r := NewReaper(db, time.UTC, slog.New(slog.NewTextHandler(&logs, nil)), ReaperOptions{})
	r.now = func() time.Time { return time.Unix(0, clock.Load()) }
	before := reaperTicks(t, sweepTTLLabel, outcomeStalled)

	var stall stallWatch
	r.runTick(context.Background(), r.ttlSweep, sweepTTLLabel, &stall)
	if got := reaperTicks(t, sweepTTLLabel, outcomeStalled) - before; got != 1 {
		t.Fatalf("stalled ticks=%v after one sweep, want 1", got)
	}
	if strings.Contains(logs.String(), "codeq_reaper_stalled") {
		t.Fatal("warned on the first stalled sweep")
	}
	clock.Add(int64(stallWarnAfter))
	r.runTick(context.Background(), r.ttlSweep, sweepTTLLabel, &stall)
	out := logs.String()
	if !strings.Contains(out, "codeq_reaper_stalled") || !strings.Contains(out, "sweep=ttl") || !strings.Contains(out, "due=1") {
		t.Fatalf("sustained stall not reported: %s", out)
	}
	if exists, err := db.Has(KeyTTLIndex(uint64(time.Now().Add(-time.Hour).Unix()), stuck)); err != nil || !exists {
		t.Fatalf("the stalled entry must be left in place: %v %v", exists, err)
	}
}

// A lease sweep that drops stale in-memory entries makes progress even though
// it moves no task: the table shrinks.
func TestLeaseSweepCountsDroppedEntriesAsProgress(t *testing.T) {
	db := openTestDB(t)
	db.Leases.Set("gone", "w", domain.CmdGenerateMaster, "", 1)
	r := NewReaper(db, time.UTC, slog.Default(), ReaperOptions{})
	res, err := r.leaseSweep(context.Background())
	if err != nil || res.due != 1 || res.done != 1 || res.outcome() != outcomeProgress {
		t.Fatalf("stale lease sweep=%+v err=%v", res, err)
	}
	if db.Leases.Len() != 0 {
		t.Fatal("stale lease entry kept")
	}
}
