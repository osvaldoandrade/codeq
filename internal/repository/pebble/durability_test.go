package pebble

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pebbledb "github.com/cockroachdb/pebble"
	"github.com/cockroachdb/pebble/vfs"
	"github.com/cockroachdb/pebble/vfs/errorfs"

	"github.com/osvaldoandrade/codeq/pkg/domain"
)

// errorfs v1.1.5 does not inject SyncData failures. Route only this test
// filesystem's durability barrier through Sync, which has that fault hook.
type (
	syncDataFaultFS   struct{ vfs.FS }
	syncDataFaultFile struct{ vfs.File }
)

func (f syncDataFaultFile) SyncData() error { return f.Sync() }
func (fs syncDataFaultFS) Create(name string) (vfs.File, error) {
	file, err := fs.FS.Create(name)
	if err != nil {
		return nil, err
	}
	return syncDataFaultFile{file}, nil
}

func (fs syncDataFaultFS) ReuseForWrite(oldName, newName string) (vfs.File, error) {
	file, err := fs.FS.ReuseForWrite(oldName, newName)
	if err != nil {
		return nil, err
	}
	return syncDataFaultFile{file}, nil
}

func (fs syncDataFaultFS) OpenReadWrite(name string, opts ...vfs.OpenOption) (vfs.File, error) {
	file, err := fs.FS.OpenReadWrite(name, opts...)
	if err != nil {
		return nil, err
	}
	return syncDataFaultFile{file}, nil
}

func testRawDurableDB(t *testing.T, fs vfs.FS, path string) (*DB, func()) {
	t.Helper()
	raw, err := pebbledb.Open(path, &pebbledb.Options{FS: fs})
	if err != nil {
		t.Fatal(err)
	}
	db := &DB{db: raw, commitCh: make(chan *commitReq, commitChanBuf), stopCh: make(chan struct{}), stopped: make(chan struct{}), Leases: newLeaseTable(), fsyncOnCommit: true}
	go db.commitLoop()
	var once sync.Once
	closeDB := func() { once.Do(func() { _ = db.Close() }) }
	t.Cleanup(closeDB)
	return db, closeDB
}

func TestAbsoluteExpiryFailedSyncProcessCannotAcknowledge(t *testing.T) {
	if os.Getenv("CODEQ_TEST_ABSOLUTE_SYNC_HELPER") == "1" {
		var fail atomic.Bool
		fs := errorfs.Wrap(vfs.Default, errorfs.InjectorFunc(func(op errorfs.Op, path string) error {
			if op == errorfs.OpFileSync && strings.HasSuffix(path, ".log") && fail.Load() {
				return errorfs.ErrInjected
			}
			return nil
		}))
		db, _ := testRawDurableDB(t, syncDataFaultFS{fs}, os.Getenv("CODEQ_TEST_ABSOLUTE_PATH"))
		attachAbsolutePolicy(db, 7200, nil, time.Now())
		repo := NewTaskRepository(db, time.UTC, "fixed", 1, 5)
		deadline, err := time.Parse(time.RFC3339Nano, os.Getenv("CODEQ_TEST_ABSOLUTE_DEADLINE"))
		if err != nil {
			t.Fatal(err)
		}
		fail.Store(true)
		task, err := repo.Enqueue(domain.WithTaskExpiry(context.Background(), deadline), absoluteTestTopic, `{"batch":"sync-failure"}`, 0, "", 100, "", "", "logs-sync", time.Time{}, "tenant-a")
		if err == nil && task != nil {
			fmt.Println("ABSOLUTE_ACCEPTED")
			return
		}
		t.Fatalf("commit returned without acceptance: %v", err)
	}
	path := t.TempDir()
	deadline := time.Now().Add(time.Hour).UTC()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, exe, "-test.run=^TestAbsoluteExpiryFailedSyncProcessCannotAcknowledge$")
	command.Env = append(os.Environ(), "CODEQ_TEST_ABSOLUTE_SYNC_HELPER=1", "CODEQ_TEST_ABSOLUTE_PATH="+path, "CODEQ_TEST_ABSOLUTE_DEADLINE="+deadline.Format(time.RFC3339Nano))
	output, err := command.CombinedOutput()
	if err == nil || ctx.Err() != nil || !bytes.Contains(output, []byte("injected error")) || bytes.Contains(output, []byte("ABSOLUTE_ACCEPTED")) {
		t.Fatalf("Sync failure crossed ACK boundary: err=%v output=%s", err, output)
	}
	// A separate OS process performed the failed write and exited. Recovery may
	// retain an unacknowledged WAL record, but must preserve its original expiry.
	db, err := Open(Options{Path: path, FsyncOnCommit: true})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	attachAbsolutePolicy(db, 7200, nil, time.Now())
	repo := NewTaskRepository(db, time.UTC, "fixed", 1, 5)
	var before domain.Task
	if body, err := db.Get(KeyTask("logs-sync")); err == nil {
		if err = json.Unmarshal(body, &before); err != nil {
			t.Fatal(err)
		}
	}
	task, err := repo.Enqueue(domain.WithTaskExpiry(context.Background(), deadline), absoluteTestTopic, `{"batch":"sync-failure"}`, 0, "", 100, "", "", "logs-sync", time.Time{}, "tenant-a")
	if err != nil || task == nil || task.ExpiresAt == nil || !task.ExpiresAt.Equal(deadline) {
		t.Fatalf("recovered retry=%+v %v", task, err)
	}
	if !before.CreatedAt.IsZero() && !task.CreatedAt.Equal(before.CreatedAt) {
		t.Fatal("restart renewed original metadata")
	}
}

type ambiguousAbsoluteReplicator struct {
	db   *pebbledb.DB
	fail atomic.Bool
}

func (r *ambiguousAbsoluteReplicator) IsLeader() bool         { return true }
func (r *ambiguousAbsoluteReplicator) LeaderHTTPAddr() string { return "" }
func (r *ambiguousAbsoluteReplicator) DurableWrites() bool    { return true }
func (r *ambiguousAbsoluteReplicator) Replicate(repr []byte) error {
	b := r.db.NewBatch()
	defer b.Close()
	if err := b.SetRepr(bytes.Clone(repr)); err != nil {
		return err
	}
	if r.fail.Load() {
		if err := b.Commit(pebbledb.NoSync); err != nil {
			return err
		}
		return errors.New("ambiguous replication failure")
	}
	return b.Commit(pebbledb.Sync)
}

func TestAbsoluteExpiryAmbiguousReplayNeedsDurableFence(t *testing.T) {
	db, repo, _, clock := absoluteFixture(t)
	owner := &ambiguousAbsoluteReplicator{db: db.Raw()}
	db.AttachReplicator(owner)
	owner.fail.Store(true)
	deadline := time.Unix(0, clock.Load()).Add(time.Hour)
	ctx := domain.WithTaskExpiry(context.Background(), deadline)
	for attempt := 0; attempt < 2; attempt++ {
		task, err := repo.Enqueue(ctx, absoluteTestTopic, `{"batch":"ambiguous"}`, 0, "", 100, "", "", "logs-ambiguous", time.Time{}, "tenant-a")
		if err == nil || task != nil {
			t.Fatalf("attempt%d falsely confirmed memory state=%+v %v", attempt, task, err)
		}
	}
	before, err := repo.Get(context.Background(), "logs-ambiguous")
	if err != nil {
		t.Fatal(err)
	}
	owner.fail.Store(false)
	replay, err := repo.Enqueue(ctx, absoluteTestTopic, `{"batch":"ambiguous"}`, 0, "", 100, "", "", "logs-ambiguous", time.Time{}, "tenant-a")
	if err != nil || replay == nil || !replay.CreatedAt.Equal(before.CreatedAt) || !replay.ExpiresAt.Equal(*before.ExpiresAt) {
		t.Fatalf("fenced replay=%+v %v", replay, err)
	}
	if task, ok, err := repo.Claim(context.Background(), "worker", []domain.Command{absoluteTestTopic}, 60, 100, 100, "tenant-a"); err != nil || !ok || task.ID != replay.ID {
		t.Fatalf("lost ACK stranded queue=%+v %v %v", task, ok, err)
	}
}

func TestAbsoluteExpiryRestartAndMetadataExpiry(t *testing.T) {
	path := t.TempDir()
	db, err := Open(Options{Path: path, FsyncOnCommit: true})
	if err != nil {
		t.Fatal(err)
	}
	attachAbsolutePolicy(db, 7200, nil, time.Now())
	repo := NewTaskRepository(db, time.UTC, "fixed", 1, 5)
	clock := &atomic.Int64{}
	clock.Store(time.Now().UnixNano())
	task := enqueueAbsolute(t, repo, clock, "logs-restart", "", time.Time{}, 100)
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open(Options{Path: path, FsyncOnCommit: true})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.expiryNow = func() time.Time { return time.Unix(0, clock.Load()).UTC() }
	attachAbsolutePolicy(db, 7200, nil, time.Now())
	repo = NewTaskRepository(db, time.UTC, "fixed", 1, 5)
	got, err := repo.Get(context.Background(), task.ID)
	if err != nil || !got.CreatedAt.Equal(task.CreatedAt) || !got.ExpiresAt.Equal(*task.ExpiresAt) {
		t.Fatalf("restart renewed=%+v %v", got, err)
	}
	clock.Store(task.ExpiresAt.Add(time.Second).UnixNano())
	if _, err = repo.CleanupExpired(context.Background(), 100, db.expiryTime()); err != nil {
		t.Fatal(err)
	}
	assertNoPayloadIndexes(t, db, task.ID)
	identity, err := db.readAbsoluteIdentity(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	clock.Store(identity.ReplayUntil.Add(time.Second).UnixNano())
	if _, err = repo.CleanupExpired(context.Background(), 100, db.expiryTime()); err != nil {
		t.Fatal(err)
	}
	if active, err := db.HasAbsoluteTopicCustody(context.Background(), task.TenantID, absoluteTestTopic); err != nil || active {
		t.Fatalf("custody exceeded replay window=%v %v", active, err)
	}
	if count, err := db.absoluteCount(); err != nil || count != 0 {
		t.Fatalf("metadata budget not released=%d %v", count, err)
	}
}
