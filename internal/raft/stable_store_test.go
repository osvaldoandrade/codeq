package raft

import (
	"testing"

	pebbledb "github.com/cockroachdb/pebble"
)

func openStableStoreTestDB(t *testing.T) (*pebbledb.DB, *stableStore) {
	t.Helper()
	dir := t.TempDir()
	db, err := pebbledb.Open(dir, &pebbledb.Options{})
	if err != nil {
		t.Fatalf("pebble open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, newStableStore(db)
}

func TestStableStore_GetMissingReturnsNil(t *testing.T) {
	_, s := openStableStoreTestDB(t)
	got, err := s.Get([]byte("missing"))
	if err != nil {
		t.Errorf("Get missing: want nil error, got %v", err)
	}
	if got != nil {
		t.Errorf("Get missing: want nil bytes, got %q", got)
	}
}

func TestStableStore_GetUint64MissingReturnsZero(t *testing.T) {
	_, s := openStableStoreTestDB(t)
	got, err := s.GetUint64([]byte("term"))
	if err != nil {
		t.Errorf("GetUint64 missing: want nil error, got %v", err)
	}
	if got != 0 {
		t.Errorf("GetUint64 missing: want 0, got %d", got)
	}
}

func TestStableStore_SetGetRoundTrip(t *testing.T) {
	_, s := openStableStoreTestDB(t)
	if err := s.Set([]byte("vote"), []byte("node-1")); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := s.Get([]byte("vote"))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got) != "node-1" {
		t.Errorf("round-trip: want node-1, got %q", got)
	}
}

func TestStableStore_SetGetUint64RoundTrip(t *testing.T) {
	_, s := openStableStoreTestDB(t)
	if err := s.SetUint64([]byte("term"), 12345); err != nil {
		t.Fatalf("SetUint64: %v", err)
	}
	got, err := s.GetUint64([]byte("term"))
	if err != nil {
		t.Fatalf("GetUint64: %v", err)
	}
	if got != 12345 {
		t.Errorf("uint64 round-trip: want 12345, got %d", got)
	}
}

func TestStableStore_Overwrite(t *testing.T) {
	_, s := openStableStoreTestDB(t)
	_ = s.SetUint64([]byte("k"), 1)
	_ = s.SetUint64([]byte("k"), 2)
	got, _ := s.GetUint64([]byte("k"))
	if got != 2 {
		t.Errorf("overwrite: want 2, got %d", got)
	}
}

func TestStableStore_PrefixIsolation(t *testing.T) {
	// A value written outside the stable/ prefix must NOT be visible
	// through stableStore.Get — confirms the prefix scoping is wired.
	db, s := openStableStoreTestDB(t)
	if err := db.Set([]byte("not-stable-key"), []byte("intruder"), pebbledb.NoSync); err != nil {
		t.Fatalf("raw Set: %v", err)
	}
	got, err := s.Get([]byte("not-stable-key"))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != nil {
		t.Errorf("prefix leak: stableStore saw foreign key, value=%q", got)
	}
}
