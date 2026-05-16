package pebble

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/osvaldoandrade/codeq/pkg/domain"
)

// TestConcurrentClaimNeverDoubleAssigns proves (or disproves) that a
// single task can be claimed by two workers simultaneously when several
// goroutines call Claim against a queue fed by Nack(delaySeconds=1).
//
// The 6.10% k6 fail-rate hypothesis is: moveDueDelayedForTenant runs
// without single-flighting, so two concurrent Claims read the same
// delayed range, both write KeyPending (different seq, same id), both
// publish hints — and both winning hints turn into independent claims
// of the same id.
//
// We don't need the *full* k6 setup; a small queue + many Claim
// goroutines is enough.
func TestConcurrentClaimNeverDoubleAssigns(t *testing.T) {
	ctx := context.Background()
	db := openTestDB(t)
	repo := NewTaskRepository(db, time.UTC, "fixed", 1, 5)
	cmd := domain.CmdGenerateMaster

	// Seed N tasks all becoming visible at the same instant via
	// Nack(delaySeconds=0): the producer enqueues, claims, then nacks
	// with 0 delay, which puts them all into the delayed bucket with
	// a "due now" score. Concurrent claims then race on the same
	// delayed range during moveDueDelayedForTenant.
	const N = 200
	ids := make([]string, 0, N)
	for range N {
		enq, err := repo.Enqueue(ctx, cmd, `{}`, 0, "", 3, "", time.Time{}, "")
		if err != nil {
			t.Fatalf("enqueue: %v", err)
		}
		got, ok, err := repo.Claim(ctx, "seed", []domain.Command{cmd}, 60, 0, 5, "")
		if err != nil || !ok {
			t.Fatalf("seed claim: ok=%v err=%v", ok, err)
		}
		if _, _, err := repo.Nack(ctx, got.ID, "seed", 0, 5, "reset"); err != nil {
			t.Fatalf("seed nack: %v", err)
		}
		ids = append(ids, enq.ID)
	}

	// Sleep just past the 0-delay so the delayed bucket entries are
	// "due" when concurrent Claims hit moveDueDelayedForTenant.
	time.Sleep(50 * time.Millisecond)

	const workers = 32
	var (
		mu          sync.Mutex
		assignments = make(map[string]int) // id → claim count
		duplicates  atomic.Int64
		totalClaims atomic.Int64
	)

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for w := range workers {
		wg.Go(func() {
			workerID := fmt.Sprintf("w%d", w)
			for {
				select {
				case <-stop:
					return
				default:
				}
				got, ok, err := repo.Claim(ctx, workerID, []domain.Command{cmd}, 60, 0, 5, "")
				if err != nil {
					t.Errorf("claim: %v", err)
					return
				}
				if !ok || got == nil {
					return
				}
				totalClaims.Add(1)
				mu.Lock()
				assignments[got.ID]++
				if assignments[got.ID] > 1 {
					duplicates.Add(1)
				}
				mu.Unlock()
			}
		})
	}

	// Give the workers up to 2 seconds to drain.
	time.Sleep(2 * time.Second)
	close(stop)
	wg.Wait()

	var (
		seen  int
		multi int
	)
	for _, n := range assignments {
		if n >= 1 {
			seen++
		}
		if n > 1 {
			multi++
		}
	}
	t.Logf("seeded=%d  unique_ids_seen=%d  ids_claimed_more_than_once=%d  total_claims=%d  duplicate_assignments=%d",
		N, seen, multi, totalClaims.Load(), duplicates.Load())
	if multi > 0 {
		t.Fatalf("BUG REPRODUCED: %d ids were claimed by more than one worker", multi)
	}
}
