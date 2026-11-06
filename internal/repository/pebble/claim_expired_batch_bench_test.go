package pebble

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/osvaldoandrade/codeq/pkg/domain"
)

// latencyReplicator models a replicated commit: every Replicate pays one
// round trip before the batch is applied locally, like a Raft leader whose
// followers acknowledge after delay.
type latencyReplicator struct {
	*ambiguousAbsoluteReplicator
	delay time.Duration
}

func (r *latencyReplicator) Replicate(repr []byte) error {
	if r.delay > 0 {
		time.Sleep(r.delay)
	}
	return r.ambiguousAbsoluteReplicator.Replicate(repr)
}

// seedExpiredHeads accepts expired absolute tasks ahead of one live task in
// the same FIFO, then moves the clock past the expired deadlines.
func seedExpiredHeads(tb testing.TB, repo *TaskRepository, clock *atomic.Int64, expired int) *domain.Task {
	tb.Helper()
	base := time.Unix(0, clock.Load()).UTC()
	enqueue := func(id string, deadline time.Time) *domain.Task {
		ctx := domain.WithTaskExpiry(context.Background(), deadline)
		task, err := repo.Enqueue(ctx, absoluteTestTopic, `{"batch":1}`, 3, "", 100, "", "", id, time.Time{}, "tenant-a")
		if err != nil {
			tb.Fatal(err)
		}
		return task
	}
	for i := range expired {
		enqueue(fmt.Sprintf("expired-%04d", i), base.Add(time.Minute))
	}
	clock.Store(base.Add(61 * time.Second).UnixNano())
	return enqueue("live", base.Add(10*time.Minute))
}

// BenchmarkClaimBehindExpiredHeads measures one claim that meets 200 expired
// absolute heads before a live task (the CFP-070 Conveste incident shape).
// "pebble-fsync" is standalone Pebble with fsyncOnCommit; "replicated-2ms"
// charges every replicated commit a 2 ms round trip. commits/op reports the
// writes one claim submits (v0.5.12: 201, one per expired head plus the
// lease; v0.5.13: 2).
//
// Run: go test -run '^$' -bench BenchmarkClaimBehindExpiredHeads -benchtime 10x ./internal/repository/pebble/
func BenchmarkClaimBehindExpiredHeads(b *testing.B) {
	const expired = 200
	for _, mode := range []struct {
		name  string
		repl  bool
		delay time.Duration
	}{{"pebble-fsync", false, 0}, {"replicated-2ms", true, 2 * time.Millisecond}} {
		b.Run(mode.name, func(b *testing.B) {
			var commits int64
			for range b.N {
				b.StopTimer()
				db, repo, _, clock := absoluteFixture(b)
				repo.reconcile.interval = time.Hour
				live := seedExpiredHeads(b, repo, clock, expired)
				if mode.repl {
					db.AttachReplicator(&latencyReplicator{ambiguousAbsoluteReplicator: &ambiguousAbsoluteReplicator{db: db.Raw()}, delay: mode.delay})
				}
				writes := countWrites(db)
				b.StartTimer()
				got, ok, err := repo.Claim(context.Background(), "worker", []domain.Command{absoluteTestTopic}, 60, defaultInspectLimit+1, 100, "tenant-a")
				b.StopTimer()
				if err != nil || !ok || got.ID != live.ID {
					b.Fatalf("claim=%+v ok=%v err=%v", got, ok, err)
				}
				commits += writes.Load()
			}
			b.ReportMetric(float64(commits)/float64(b.N), "commits/op")
		})
	}
}
