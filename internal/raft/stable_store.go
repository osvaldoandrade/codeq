package raft

import (
	"fmt"

	pebbledb "github.com/cockroachdb/pebble"
)

// stableStore implements hashicorp/raft.StableStore over Pebble. Stores
// term/vote/candidate state under raft/stable/<key>.
//
// M1 status: skeleton (T3).
type stableStore struct {
	pebble *pebbledb.DB
}

func newStableStore(p *pebbledb.DB) *stableStore {
	return &stableStore{pebble: p}
}

func (s *stableStore) Set(key, val []byte) error {
	return fmt.Errorf("stableStore.Set: TODO M1.T3")
}

func (s *stableStore) Get(key []byte) ([]byte, error) {
	return nil, fmt.Errorf("stableStore.Get: TODO M1.T3")
}

func (s *stableStore) SetUint64(key []byte, val uint64) error {
	return fmt.Errorf("stableStore.SetUint64: TODO M1.T3")
}

func (s *stableStore) GetUint64(key []byte) (uint64, error) {
	return 0, fmt.Errorf("stableStore.GetUint64: TODO M1.T3")
}
