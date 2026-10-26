package pebble

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/osvaldoandrade/codeq/pkg/domain"
)

// retireOne accepts one absolute task, moves the clock past its deadline and
// sweeps until it is retired, returning the identity body after retirement.
func retireOne(t *testing.T, id string) (*DB, *TaskRepository, func(time.Duration), []byte) {
	t.Helper()
	db, repo, _, clock := absoluteFixture(t)
	enqueueAbsolute(t, repo, clock, id, "", time.Time{}, 3)
	advance := func(d time.Duration) { clock.Add(int64(d)) }
	advance(2 * time.Hour)
	if n, err := db.sweepAbsolute(context.Background(), 512); err != nil || n != 1 {
		t.Fatalf("retire sweep = %d, %v", n, err)
	}
	body, err := db.Get(absoluteKey(id))
	if err != nil {
		t.Fatalf("tombstone missing after retirement: %v", err)
	}
	identity, err := db.readAbsoluteIdentity(id)
	if err != nil || identity == nil || identity.RetiredAt == nil {
		t.Fatalf("identity not retired: %+v %v", identity, err)
	}
	assertNoPayloadIndexes(t, db, id)
	return db, repo, advance, body
}

func TestAbsoluteSweepLeavesSettledTombstoneUnwritten(t *testing.T) {
	const id = "logs-settled"
	db, repo, advance, retired := retireOne(t, id)

	for visit := range 3 {
		before := db.Raw().Metrics().WAL.BytesIn
		if n, err := db.sweepAbsolute(context.Background(), 512); err != nil || n != 0 {
			t.Fatalf("visit %d: sweep = %d, %v", visit, n, err)
		}
		if written := db.Raw().Metrics().WAL.BytesIn - before; written != 0 {
			t.Fatalf("visit %d rewrote a settled tombstone: %d WAL bytes", visit, written)
		}
	}
	body, err := db.Get(absoluteKey(id))
	if err != nil || !bytes.Equal(body, retired) {
		t.Fatalf("tombstone changed: %s, %v", body, err)
	}

	// The replay guarantee is unchanged while the window is open.
	ctx := domain.WithTaskExpiry(context.Background(), time.Now().Add(time.Hour))
	if _, err = repo.Enqueue(ctx, absoluteTestTopic, `{"batch":"sensitive-payload"}`, 3, "", 3, "", "", id, time.Time{}, dedupeTenant); !errors.Is(err, domain.ErrTaskExpired) {
		t.Fatalf("replay inside window = %v, want ErrTaskExpired", err)
	}
	if count, err := db.absoluteCount(); err != nil || count != 1 {
		t.Fatalf("count inside window = %d, %v", count, err)
	}

	// After the window the same sweep still removes the tombstone.
	advance(absoluteReplayWindow + time.Minute)
	if _, err = db.sweepAbsolute(context.Background(), 512); err != nil {
		t.Fatal(err)
	}
	if yes, err := db.Has(absoluteKey(id)); err != nil || yes {
		t.Fatalf("tombstone survived its window: %v %v", yes, err)
	}
	if count, err := db.absoluteCount(); err != nil || count != 0 {
		t.Fatalf("count after window = %d, %v", count, err)
	}
	if active, err := db.HasAbsoluteTopicCustody(context.Background(), dedupeTenant, absoluteTestTopic); err != nil || active {
		t.Fatalf("custody after window = %v, %v", active, err)
	}
}

func TestAbsoluteSweepStillPurgesRetiredTombstoneWithResidue(t *testing.T) {
	const id = "logs-residue"
	db, _, _, _ := retireOne(t, id)

	// A reverse reference left behind (e.g. restored from an older snapshot)
	// keeps the tombstone eligible for purge, which removes both keys.
	stray := KeyTTLIndex(1, id)
	if err := db.Set(stray, nil); err != nil {
		t.Fatal(err)
	}
	if err := db.Set(absoluteRefKey(id, stray), stray); err != nil {
		t.Fatal(err)
	}
	if _, err := db.sweepAbsolute(context.Background(), 512); err != nil {
		t.Fatal(err)
	}
	for _, key := range [][]byte{stray, absoluteRefKey(id, stray)} {
		if yes, err := db.Has(key); err != nil || yes {
			t.Fatalf("residue %q survived: %v %v", key, yes, err)
		}
	}
	if yes, err := db.Has(absoluteKey(id)); err != nil || !yes {
		t.Fatalf("tombstone lost inside its window: %v %v", yes, err)
	}
}

func TestAbsoluteSweepStillPurgesRetiredTombstoneWithRestoredRow(t *testing.T) {
	const id = "logs-restored"
	db, repo, _, _ := retireOne(t, id)

	// A task row reappearing under a retired identity (restore of an older
	// snapshot) is expired by its own deadline and must be removed again.
	row := domain.Task{ID: id, Command: absoluteTestTopic, Payload: `{"batch":"sensitive-payload"}`, TenantID: dedupeTenant, Status: domain.StatusPending, CreatedAt: time.Now().Add(-3 * time.Hour).UTC()}
	deadline := time.Now().Add(-time.Hour).UTC()
	row.ExpiresAt = &deadline
	body, err := json.Marshal(&row)
	if err != nil {
		t.Fatal(err)
	}
	if err = db.Set(KeyTask(id), body); err != nil {
		t.Fatal(err)
	}
	if _, err = db.sweepAbsolute(context.Background(), 512); err != nil {
		t.Fatal(err)
	}
	if yes, err := db.Has(KeyTask(id)); err != nil || yes {
		t.Fatalf("restored row survived: %v %v", yes, err)
	}
	if got, err := repo.Get(context.Background(), id); err == nil || got != nil {
		t.Fatalf("restored row visible: %+v %v", got, err)
	}
}
