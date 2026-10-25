package raft

import (
	"bytes"
	"testing"

	pebbledb "github.com/cockroachdb/pebble"
	hraft "github.com/hashicorp/raft"
)

func openSnapshotTestDB(t *testing.T) *pebbledb.DB {
	t.Helper()
	dir := t.TempDir()
	db, err := pebbledb.Open(dir, &pebbledb.Options{})
	if err != nil {
		t.Fatalf("pebble open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestSnapshot_RoundTrip(t *testing.T) {
	src := openSnapshotTestDB(t)
	// Populate codeq/ keys + a non-FSM key that must NOT be captured.
	for _, kv := range [][2]string{
		{"codeq/tasks/abc", `{"id":"abc","status":"PENDING"}`},
		{"codeq/q/GENERATE_MASTER/t1/pending/0/0000/abc", ""},
		{"codeq/r/abc", `{"taskId":"abc","status":"COMPLETED"}`},
		{"codeq/admin/topics/payments.events", `{"topicId":"payments.events","version":2}`},
	} {
		if err := src.Set([]byte(kv[0]), []byte(kv[1]), pebbledb.NoSync); err != nil {
			t.Fatalf("seed %s: %v", kv[0], err)
		}
	}
	// Out-of-prefix key that must be skipped.
	if err := src.Set([]byte("raft/log/bogus"), []byte("ignored"), pebbledb.NoSync); err != nil {
		t.Fatalf("seed raft key: %v", err)
	}

	var buf bytes.Buffer
	snap := src.NewSnapshot()
	if err := writeSnapshot(&buf, snap); err != nil {
		_ = snap.Close()
		t.Fatalf("writeSnapshot: %v", err)
	}
	_ = snap.Close()

	dst := openSnapshotTestDB(t)
	if err := readSnapshot(&buf, dst); err != nil {
		t.Fatalf("readSnapshot: %v", err)
	}

	for _, kv := range [][2]string{
		{"codeq/tasks/abc", `{"id":"abc","status":"PENDING"}`},
		{"codeq/q/GENERATE_MASTER/t1/pending/0/0000/abc", ""},
		{"codeq/r/abc", `{"taskId":"abc","status":"COMPLETED"}`},
		{"codeq/admin/topics/payments.events", `{"topicId":"payments.events","version":2}`},
	} {
		v, closer, err := dst.Get([]byte(kv[0]))
		if err != nil {
			t.Errorf("Get %s: %v", kv[0], err)
			continue
		}
		if string(v) != kv[1] {
			t.Errorf("Get %s: want %q, got %q", kv[0], kv[1], v)
		}
		closer.Close()
	}
	// Non-prefix key from the source must NOT have leaked into dst.
	_, _, err := dst.Get([]byte("raft/log/bogus"))
	if err != pebbledb.ErrNotFound {
		t.Errorf("non-prefix key should not appear in restored db (got err=%v)", err)
	}
}

func TestSnapshot_RestoreWipesExistingState(t *testing.T) {
	dst := openSnapshotTestDB(t)
	// Seed dst with stale state that should be wiped on restore.
	_ = dst.Set([]byte("codeq/tasks/stale-1"), []byte("old"), pebbledb.NoSync)
	_ = dst.Set([]byte("codeq/tasks/stale-2"), []byte("older"), pebbledb.NoSync)

	src := openSnapshotTestDB(t)
	_ = src.Set([]byte("codeq/tasks/fresh"), []byte("new"), pebbledb.NoSync)

	var buf bytes.Buffer
	snap := src.NewSnapshot()
	_ = writeSnapshot(&buf, snap)
	_ = snap.Close()

	if err := readSnapshot(&buf, dst); err != nil {
		t.Fatalf("readSnapshot: %v", err)
	}
	for _, key := range []string{"codeq/tasks/stale-1", "codeq/tasks/stale-2"} {
		_, _, err := dst.Get([]byte(key))
		if err != pebbledb.ErrNotFound {
			t.Errorf("stale key %s should be wiped (err=%v)", key, err)
		}
	}
	v, closer, err := dst.Get([]byte("codeq/tasks/fresh"))
	if err != nil {
		t.Fatalf("Get fresh: %v", err)
	}
	defer closer.Close()
	if string(v) != "new" {
		t.Errorf("fresh value: want new, got %q", v)
	}
}

func TestSnapshot_EmptySource(t *testing.T) {
	src := openSnapshotTestDB(t)
	dst := openSnapshotTestDB(t)
	_ = dst.Set([]byte("codeq/tasks/should-be-wiped"), []byte("x"), pebbledb.NoSync)

	var buf bytes.Buffer
	snap := src.NewSnapshot()
	if err := writeSnapshot(&buf, snap); err != nil {
		_ = snap.Close()
		t.Fatalf("writeSnapshot: %v", err)
	}
	_ = snap.Close()

	if err := readSnapshot(&buf, dst); err != nil {
		t.Fatalf("readSnapshot: %v", err)
	}
	_, _, err := dst.Get([]byte("codeq/tasks/should-be-wiped"))
	if err != pebbledb.ErrNotFound {
		t.Errorf("dst should be empty after restoring an empty snapshot (err=%v)", err)
	}
}

func TestSnapshot_BadMagic(t *testing.T) {
	dst := openSnapshotTestDB(t)
	buf := bytes.NewReader([]byte("XXXX\x00\x00\x00\x01\x00"))
	err := readSnapshot(buf, dst)
	if err == nil {
		t.Fatal("readSnapshot: want error on bad magic, got nil")
	}
}

func TestSnapshot_UnsupportedVersion(t *testing.T) {
	dst := openSnapshotTestDB(t)
	// CDQS + version 99 + eof tag
	bad := []byte{'C', 'D', 'Q', 'S', 0x00, 0x00, 0x00, 0x63, snapshotTagEOF}
	err := readSnapshot(bytes.NewReader(bad), dst)
	if err == nil {
		t.Fatal("readSnapshot: want error on unsupported version, got nil")
	}
}

func TestSnapshot_TruncatedStream(t *testing.T) {
	src := openSnapshotTestDB(t)
	_ = src.Set([]byte("codeq/tasks/a"), []byte("hello"), pebbledb.NoSync)
	var buf bytes.Buffer
	snap := src.NewSnapshot()
	_ = writeSnapshot(&buf, snap)
	_ = snap.Close()

	// Lop off the trailing EOF byte to simulate truncation.
	truncated := buf.Bytes()
	truncated = truncated[:len(truncated)-1]

	dst := openSnapshotTestDB(t)
	err := readSnapshot(bytes.NewReader(truncated), dst)
	if err == nil {
		t.Fatal("readSnapshot: want error on truncated stream, got nil")
	}
}

func TestSnapshot_NewSnapshotStoreCreatesDir(t *testing.T) {
	dir := t.TempDir() + "/snaps"
	store, err := newSnapshotStore(dir, 0)
	if err != nil {
		t.Fatalf("newSnapshotStore: %v", err)
	}
	if store == nil {
		t.Fatal("newSnapshotStore returned nil store")
	}
	list, err := store.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 0 {
		t.Errorf("fresh store should be empty, got %d entries", len(list))
	}
}

func TestSnapshot_StoreRetainsConfiguredCount(t *testing.T) {
	for _, tc := range []struct{ retain, want int }{{0, DefaultSnapshotRetain}, {1, 1}, {2, 2}, {3, 3}} {
		store, err := newSnapshotStore(t.TempDir(), tc.retain)
		if err != nil {
			t.Fatalf("newSnapshotStore(%d): %v", tc.retain, err)
		}
		for i := uint64(1); i <= 5; i++ {
			sink, err := store.Create(hraft.SnapshotVersionMax, i*10, 1, hraft.Configuration{}, 1, nil)
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			if _, err := sink.Write([]byte("state")); err != nil {
				t.Fatalf("Write: %v", err)
			}
			if err := sink.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}
		}
		list, err := store.List()
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(list) != tc.want {
			t.Fatalf("retain=%d kept %d snapshots, want %d", tc.retain, len(list), tc.want)
		}
		if list[0].Index != 50 {
			t.Fatalf("newest snapshot index = %d, want 50", list[0].Index)
		}
	}
}

func TestConfig_RetentionDefaults(t *testing.T) {
	if got := (Config{}).snapshotRetain(); got != 2 {
		t.Fatalf("default snapshot retain = %d", got)
	}
	if got := (Config{}).trailingLogs(); got != 2048 {
		t.Fatalf("default trailing logs = %d", got)
	}
	c := Config{SnapshotRetain: 4, TrailingLogs: 512}
	if c.snapshotRetain() != 4 || c.trailingLogs() != 512 {
		t.Fatalf("explicit retention ignored: %d %d", c.snapshotRetain(), c.trailingLogs())
	}
}
