package raft

import (
	"encoding/binary"
	"errors"
	"fmt"

	pebbledb "github.com/cockroachdb/pebble"
)

// stableStore implements hashicorp/raft.StableStore over Pebble. Term,
// vote, and any other small durable state raft needs lives under the
// `raft/stable/` prefix on the same Pebble store the log and FSM share.
//
// Per the StableStore docstring (raft@v1.7.3/stable.go:16-17), missing
// keys return zero values (not errors): Get → (nil, nil),
// GetUint64 → (0, nil). raft itself is permissive in api.go but the
// documented contract is the safer one to honor.
type stableStore struct {
	pebble *pebbledb.DB
}

var stableKeyPrefix = []byte("raft/stable/")

func newStableStore(p *pebbledb.DB) *stableStore { return &stableStore{pebble: p} }

func stableKey(key []byte) []byte {
	out := make([]byte, len(stableKeyPrefix)+len(key))
	copy(out, stableKeyPrefix)
	copy(out[len(stableKeyPrefix):], key)
	return out
}

func (s *stableStore) Set(key, val []byte) error {
	return s.pebble.Set(stableKey(key), val, pebbledb.NoSync)
}

func (s *stableStore) Get(key []byte) ([]byte, error) {
	v, closer, err := s.pebble.Get(stableKey(key))
	if errors.Is(err, pebbledb.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer closer.Close()
	out := make([]byte, len(v))
	copy(out, v)
	return out, nil
}

func (s *stableStore) SetUint64(key []byte, val uint64) error {
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], val)
	return s.Set(key, buf[:])
}

func (s *stableStore) GetUint64(key []byte) (uint64, error) {
	v, err := s.Get(key)
	if err != nil {
		return 0, err
	}
	if v == nil {
		return 0, nil
	}
	if len(v) != 8 {
		return 0, fmt.Errorf("stableStore.GetUint64: key %q value len=%d, want 8", key, len(v))
	}
	return binary.BigEndian.Uint64(v), nil
}
