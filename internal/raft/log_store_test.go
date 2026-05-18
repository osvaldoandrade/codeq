package raft

import (
	"errors"
	"testing"

	pebbledb "github.com/cockroachdb/pebble"
	hraft "github.com/hashicorp/raft"
)

func openLogStoreTestDB(t *testing.T) (*pebbledb.DB, *logStore) {
	t.Helper()
	dir := t.TempDir()
	db, err := pebbledb.Open(dir, &pebbledb.Options{})
	if err != nil {
		t.Fatalf("pebble open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, newLogStore(db)
}

func TestLogStore_EmptyIndices(t *testing.T) {
	_, s := openLogStoreTestDB(t)
	first, err := s.FirstIndex()
	if err != nil {
		t.Fatalf("FirstIndex: %v", err)
	}
	if first != 0 {
		t.Errorf("FirstIndex empty: want 0, got %d", first)
	}
	last, err := s.LastIndex()
	if err != nil {
		t.Fatalf("LastIndex: %v", err)
	}
	if last != 0 {
		t.Errorf("LastIndex empty: want 0, got %d", last)
	}
}

func TestLogStore_StoreAndGet(t *testing.T) {
	_, s := openLogStoreTestDB(t)

	src := &hraft.Log{
		Index: 42,
		Term:  7,
		Type:  hraft.LogCommand,
		Data:  []byte("hello-raft"),
	}
	if err := s.StoreLog(src); err != nil {
		t.Fatalf("StoreLog: %v", err)
	}

	var got hraft.Log
	if err := s.GetLog(42, &got); err != nil {
		t.Fatalf("GetLog: %v", err)
	}
	if got.Index != 42 || got.Term != 7 || got.Type != hraft.LogCommand || string(got.Data) != "hello-raft" {
		t.Errorf("round-trip mismatch: %+v", got)
	}
}

func TestLogStore_GetLog_NotFound(t *testing.T) {
	_, s := openLogStoreTestDB(t)
	var dst hraft.Log
	err := s.GetLog(99, &dst)
	if !errors.Is(err, hraft.ErrLogNotFound) {
		t.Errorf("want hraft.ErrLogNotFound, got %v", err)
	}
}

func TestLogStore_StoreLogs_Batch(t *testing.T) {
	_, s := openLogStoreTestDB(t)

	batch := []*hraft.Log{
		{Index: 10, Term: 2, Type: hraft.LogCommand, Data: []byte("a")},
		{Index: 11, Term: 2, Type: hraft.LogCommand, Data: []byte("b")},
		{Index: 12, Term: 2, Type: hraft.LogCommand, Data: []byte("c")},
	}
	if err := s.StoreLogs(batch); err != nil {
		t.Fatalf("StoreLogs: %v", err)
	}
	first, _ := s.FirstIndex()
	last, _ := s.LastIndex()
	if first != 10 || last != 12 {
		t.Errorf("range: first=%d last=%d, want 10/12", first, last)
	}
	var middle hraft.Log
	if err := s.GetLog(11, &middle); err != nil {
		t.Fatalf("GetLog 11: %v", err)
	}
	if string(middle.Data) != "b" {
		t.Errorf("middle.Data: want b, got %q", middle.Data)
	}
}

func TestLogStore_StoreLogs_Empty(t *testing.T) {
	_, s := openLogStoreTestDB(t)
	if err := s.StoreLogs(nil); err != nil {
		t.Errorf("StoreLogs(nil): %v", err)
	}
	last, _ := s.LastIndex()
	if last != 0 {
		t.Errorf("LastIndex after empty StoreLogs: want 0, got %d", last)
	}
}

func TestLogStore_DeleteRange(t *testing.T) {
	_, s := openLogStoreTestDB(t)
	for i := uint64(1); i <= 10; i++ {
		if err := s.StoreLog(&hraft.Log{Index: i, Term: 1, Data: []byte{byte(i)}}); err != nil {
			t.Fatalf("StoreLog %d: %v", i, err)
		}
	}
	// Delete the middle band [4, 7].
	if err := s.DeleteRange(4, 7); err != nil {
		t.Fatalf("DeleteRange: %v", err)
	}
	first, _ := s.FirstIndex()
	last, _ := s.LastIndex()
	if first != 1 {
		t.Errorf("FirstIndex after delete: want 1, got %d", first)
	}
	if last != 10 {
		t.Errorf("LastIndex after delete: want 10, got %d", last)
	}
	for i := uint64(4); i <= 7; i++ {
		var dst hraft.Log
		if err := s.GetLog(i, &dst); !errors.Is(err, hraft.ErrLogNotFound) {
			t.Errorf("GetLog %d after delete: want ErrLogNotFound, got %v", i, err)
		}
	}
	// Survivors still readable.
	for _, i := range []uint64{1, 3, 8, 10} {
		var dst hraft.Log
		if err := s.GetLog(i, &dst); err != nil {
			t.Errorf("GetLog %d after delete: %v", i, err)
		}
	}
}

func TestLogStore_DeleteRange_SingleEntry(t *testing.T) {
	_, s := openLogStoreTestDB(t)
	_ = s.StoreLog(&hraft.Log{Index: 5, Term: 1, Data: []byte("solo")})
	if err := s.DeleteRange(5, 5); err != nil {
		t.Fatalf("DeleteRange 5,5: %v", err)
	}
	first, _ := s.FirstIndex()
	if first != 0 {
		t.Errorf("FirstIndex after single-delete: want 0, got %d", first)
	}
}

func TestLogStore_DeleteRange_PrefixAndSuffix(t *testing.T) {
	_, s := openLogStoreTestDB(t)
	for i := uint64(1); i <= 5; i++ {
		_ = s.StoreLog(&hraft.Log{Index: i, Term: 1})
	}
	// Drop the prefix [1, 2].
	if err := s.DeleteRange(1, 2); err != nil {
		t.Fatalf("DeleteRange prefix: %v", err)
	}
	first, _ := s.FirstIndex()
	if first != 3 {
		t.Errorf("FirstIndex after prefix-delete: want 3, got %d", first)
	}
	// Drop the suffix [5, 5].
	if err := s.DeleteRange(5, 5); err != nil {
		t.Fatalf("DeleteRange suffix: %v", err)
	}
	last, _ := s.LastIndex()
	if last != 4 {
		t.Errorf("LastIndex after suffix-delete: want 4, got %d", last)
	}
}

func TestLogStore_DeleteRange_InverseBounds(t *testing.T) {
	_, s := openLogStoreTestDB(t)
	if err := s.DeleteRange(10, 5); err == nil {
		t.Error("DeleteRange(10,5): want error, got nil")
	}
}
