package raft

import (
	"bytes"
	"context"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	pebbledb "github.com/cockroachdb/pebble"
	"github.com/cockroachdb/pebble/vfs"
	"github.com/cockroachdb/pebble/vfs/errorfs"
	hraft "github.com/hashicorp/raft"
)

const durableRaftNodeID = "node-1"

const durableRaftBindAddr = "127.0.0.1:0"

type (
	syncCountingFS   struct{ vfs.FS }
	syncCountingFile struct{ vfs.File }
)

func (f syncCountingFile) SyncData() error { return f.Sync() }
func (f syncCountingFS) Create(name string) (vfs.File, error) {
	file, err := f.FS.Create(name)
	if err != nil {
		return nil, err
	}
	return syncCountingFile{file}, nil
}

func (f syncCountingFS) OpenReadWrite(name string, opts ...vfs.OpenOption) (vfs.File, error) {
	file, err := f.FS.OpenReadWrite(name, opts...)
	if err != nil {
		return nil, err
	}
	return syncCountingFile{file}, nil
}

func (f syncCountingFS) ReuseForWrite(oldName, newName string) (vfs.File, error) {
	file, err := f.FS.ReuseForWrite(oldName, newName)
	if err != nil {
		return nil, err
	}
	return syncCountingFile{file}, nil
}

func TestDurableRaftStorageLayersSyncActualWAL(t *testing.T) {
	var syncs atomic.Int64
	fs := errorfs.Wrap(vfs.NewMem(), errorfs.InjectorFunc(func(op errorfs.Op, path string) error {
		if op == errorfs.OpFileSync && strings.HasSuffix(path, ".log") {
			syncs.Add(1)
		}
		return nil
	}))
	db, err := pebbledb.Open("durability", &pebbledb.Options{FS: syncCountingFS{fs}})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	logs, stable, fsm := newLogStore(db), newStableStore(db), newFSM(db)
	logs.writeOptions, stable.writeOptions, fsm.writeOptions = pebbledb.Sync, pebbledb.Sync, pebbledb.Sync
	assertSync := func(name string, run func() error) {
		t.Helper()
		before := syncs.Load()
		if err := run(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if syncs.Load() <= before {
			t.Fatalf("%s returned without syncing WAL", name)
		}
	}
	assertSync("log", func() error {
		return logs.StoreLog(&hraft.Log{Index: 1, Term: 1, Type: hraft.LogCommand, Data: []byte("entry")})
	})
	assertSync("stable term", func() error { return stable.SetUint64([]byte("CurrentTerm"), 2) })
	repr := buildBatchRepr(t, db, func(b *pebbledb.Batch) { _ = b.Set([]byte("codeq/durable-task"), []byte("task"), nil) })
	assertSync("FSM", func() error {
		if result := fsm.Apply(&hraft.Log{Type: hraft.LogCommand, Data: repr}); result != nil {
			return result.(error)
		}
		return nil
	})
	snapshot := db.NewSnapshot()
	var buf bytes.Buffer
	if err = writeSnapshot(&buf, snapshot); err != nil {
		t.Fatal(err)
	}
	_ = snapshot.Close()
	assertSync("snapshot restore", func() error { return fsm.Restore(io.NopCloser(bytes.NewReader(buf.Bytes()))) })
}

func TestRaftFsyncSettingPropagatesAndLegacyDefaultStaysOff(t *testing.T) {
	for _, durable := range []bool{false, true} {
		t.Run(map[bool]string{false: "legacy", true: "durable"}[durable], func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			db, err := Open(ctx, Config{Path: t.TempDir(), FsyncOnCommit: durable, SelfID: durableRaftNodeID, BindAddr: durableRaftBindAddr, Bootstrap: true, HeartbeatMS: 50, ElectionMS: 50, LeaderLeaseMS: 50})
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if db.DurableWrites() != durable || db.fsm.writeOptions.Sync != durable {
				t.Fatalf("durability config lost: advertised=%v FSM=%v", db.DurableWrites(), db.fsm.writeOptions.Sync)
			}
			if err = db.WaitLeader(ctx); err != nil {
				t.Fatal(err)
			}
			if err = db.Set([]byte("codeq/task"), []byte("durable")); err != nil {
				t.Fatal(err)
			}
		})
	}
}
