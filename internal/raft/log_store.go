package raft

import (
	"fmt"

	hraft "github.com/hashicorp/raft"

	pebbledb "github.com/cockroachdb/pebble"
)

// logStore implements hashicorp/raft.LogStore over Pebble. Keys live
// under the raft/log/<be-encoded-index> prefix on the same Pebble store
// used by the FSM state (different prefix → no overlap).
//
// M1 status: skeleton. T2 wires the full LogStore API and runs the
// hashicorp/raft TestLogStore compliance suite.
type logStore struct {
	pebble *pebbledb.DB
}

func newLogStore(p *pebbledb.DB) *logStore {
	return &logStore{pebble: p}
}

// FirstIndex returns the first index stored, or 0 for an empty store.
func (s *logStore) FirstIndex() (uint64, error) {
	return 0, fmt.Errorf("logStore.FirstIndex: TODO M1.T2")
}

// LastIndex returns the last index stored, or 0 for an empty store.
func (s *logStore) LastIndex() (uint64, error) {
	return 0, fmt.Errorf("logStore.LastIndex: TODO M1.T2")
}

// GetLog loads a log entry by index into the provided log struct.
func (s *logStore) GetLog(index uint64, log *hraft.Log) error {
	return fmt.Errorf("logStore.GetLog: TODO M1.T2")
}

// StoreLog appends a single log entry.
func (s *logStore) StoreLog(log *hraft.Log) error {
	return fmt.Errorf("logStore.StoreLog: TODO M1.T2")
}

// StoreLogs appends a batch of log entries atomically.
func (s *logStore) StoreLogs(logs []*hraft.Log) error {
	return fmt.Errorf("logStore.StoreLogs: TODO M1.T2")
}

// DeleteRange deletes log entries in [min, max].
func (s *logStore) DeleteRange(min, max uint64) error {
	return fmt.Errorf("logStore.DeleteRange: TODO M1.T2")
}
