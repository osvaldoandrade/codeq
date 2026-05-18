package raft

import (
	"fmt"
	"io"

	hraft "github.com/hashicorp/raft"
)

// snapshotStore implements hashicorp/raft.SnapshotStore using Pebble's
// Checkpoint() to take consistent point-in-time snapshots of the state
// directory. The snapshot is a tar stream over the checkpoint directory.
//
// M1 status: skeleton (T4).
type snapshotStore struct {
	dir string
}

func newSnapshotStore(dir string) *snapshotStore {
	return &snapshotStore{dir: dir}
}

func (s *snapshotStore) Create(
	version hraft.SnapshotVersion,
	index, term uint64,
	configuration hraft.Configuration,
	configurationIndex uint64,
	trans hraft.Transport,
) (hraft.SnapshotSink, error) {
	return nil, fmt.Errorf("snapshotStore.Create: TODO M1.T4")
}

func (s *snapshotStore) List() ([]*hraft.SnapshotMeta, error) {
	return nil, fmt.Errorf("snapshotStore.List: TODO M1.T4")
}

func (s *snapshotStore) Open(id string) (*hraft.SnapshotMeta, io.ReadCloser, error) {
	return nil, nil, fmt.Errorf("snapshotStore.Open: TODO M1.T4")
}
