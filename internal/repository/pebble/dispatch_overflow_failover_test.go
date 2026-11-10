package pebble

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"testing"
	"time"

	raftpkg "github.com/osvaldoandrade/codeq/internal/raft"
	"github.com/osvaldoandrade/codeq/pkg/domain"
)

// overflowVoter is one in-process Raft voter: its Pebble store, the Raft
// replicator over it, the mux listener it serves on and its repository.
type overflowVoter struct {
	id     string
	db     *DB
	owner  *raftpkg.DB
	acc    *raftpkg.MuxAcceptor
	repo   *TaskRepository
	closed bool
}

func (v *overflowVoter) close() {
	if v.closed {
		return
	}
	v.closed = true
	_ = v.owner.Close()
	_ = v.db.Close()
	_ = v.acc.Close()
}

// openOverflowCluster starts three Raft voters on loopback listeners, each
// with a repository at the overflow test cap.
func openOverflowCluster(t *testing.T, ctx context.Context) []*overflowVoter {
	t.Helper()
	voters := make([]*overflowVoter, 3)
	peers := make(map[string]string, len(voters))
	for i := range voters {
		acc, err := raftpkg.NewMuxAcceptor("127.0.0.1:0", io.Discard)
		if err != nil {
			t.Fatalf("listener: %v", err)
		}
		id := fmt.Sprintf("voter-%d", i)
		voters[i] = &overflowVoter{id: id, acc: acc}
		peers[id] = acc.Addr().String()
	}
	for _, v := range voters {
		dir := filepath.Join(t.TempDir(), v.id)
		db, err := Open(Options{Path: filepath.Join(dir, "state")})
		if err != nil {
			t.Fatal(err)
		}
		layer, err := v.acc.RegisterGroup(0)
		if err != nil {
			t.Fatal(err)
		}
		owner, err := raftpkg.OpenWithPebble(ctx, raftpkg.Config{
			Path: dir, SelfID: v.id, BindAddr: peers[v.id], Bootstrap: true, PeerAddrs: peers, StreamLayer: layer,
			HeartbeatMS: 200, ElectionMS: 200, LeaderLeaseMS: 100, CommitMS: 10, ApplyTimeout: 5 * time.Second,
		}, db.Raw())
		if err != nil {
			_ = db.Close()
			t.Fatal(err)
		}
		db.AttachReplicator(owner)
		v.db, v.owner = db, owner
		v.repo = newTaskRepository(db, time.UTC, "fixed", 1, 5, overflowCap)
	}
	t.Cleanup(func() {
		for _, v := range voters {
			v.close()
		}
	})
	return voters
}

// waitOverflowLeader returns the single open voter that leads.
func waitOverflowLeader(t *testing.T, ctx context.Context, voters []*overflowVoter) *overflowVoter {
	t.Helper()
	for ctx.Err() == nil {
		for _, v := range voters {
			if !v.closed && v.owner.IsLeader() {
				return v
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("no voter became leader")
	return nil
}

func countPending(t *testing.T, db *DB, cmd domain.Command) int {
	t.Helper()
	lower, upper := PrefixPendingAllPrios(cmd, "tenant-a")
	it, err := db.Iter(lower, upper)
	if err != nil {
		t.Fatal(err)
	}
	defer it.Close()
	n := 0
	for valid := it.First(); valid; valid = it.Next() {
		n++
	}
	return n
}

// F1 end to end: a voter that never enqueued wins the election over a
// backlog deeper than the cap. Its rebuild seeds the cap and must still raise
// its seq counter past every committed key, so the task it enqueues next is
// claimed after the whole backlog, and each task exactly once.
func TestRaftFailoverOverOverflowedQueueKeepsFIFO(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	voters := openOverflowCluster(t, ctx)
	cmd := domain.Command("failover-topic")

	old := waitOverflowLeader(t, ctx, voters)
	if err := old.repo.EnsureLeaderDispatch(ctx); err != nil {
		t.Fatalf("first leader dispatch: %v", err)
	}
	want := enqueueN(t, old.repo, cmd, 10)
	for _, v := range voters {
		for countPending(t, v.db, cmd) != len(want) {
			if ctx.Err() != nil {
				t.Fatalf("%s did not apply the backlog", v.id)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	old.close()

	next := waitOverflowLeader(t, ctx, voters)
	if err := next.repo.EnsureLeaderDispatch(ctx); err != nil {
		t.Fatalf("new leader dispatch: %v", err)
	}
	if f := next.repo.DispatchFootprint(); f.Hints != overflowCap || f.Overflowed != 1 {
		t.Fatalf("new leader seeded %+v, want %d hints in one overflowed queue", f, overflowCap)
	}
	want = append(want, enqueueN(t, next.repo, cmd, 1)...)
	got := claimAll(t, next.repo, cmd)
	requireOnce(t, got)
	requireOrder(t, got, want)
	requireEmptyDispatch(t, next.repo)
}
