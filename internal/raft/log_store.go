package raft

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"

	pebbledb "github.com/cockroachdb/pebble"
	codec "github.com/hashicorp/go-msgpack/v2/codec"
	hraft "github.com/hashicorp/raft"
)

// logStore implements hashicorp/raft.LogStore over Pebble. Entries live
// under keys of the form `raft/log/<be8 index>`. Values are msgpack-
// encoded hraft.Log structs (same encoding hashicorp/raft-boltdb uses,
// so the wire shape is unsurprising).
//
// Multi-write atomicity uses a Pebble batch; single-writes use Set with
// the default no-sync write options. The hashicorp/raft engine calls
// StoreLogs with batches of new entries and never interleaves writers
// with itself, so we don't need internal locking.
type logStore struct {
	pebble *pebbledb.DB
}

var (
	// logKeyPrefix scopes all raft log entries inside the same Pebble
	// store the FSM uses for state. The trailing slash keeps the prefix
	// distinguishable from any sibling raft/ prefixes (stable/, snap/).
	logKeyPrefix    = []byte("raft/log/")
	logKeyPrefixEnd = []byte("raft/log0") // one past '/' (0x2F → 0x30)

	// errLogNotFound is returned by GetLog for missing indices. raft
	// requires the exact sentinel hraft.ErrLogNotFound so callers can
	// distinguish from other errors.
	errLogNotFound = hraft.ErrLogNotFound

	msgpackHandle = &codec.MsgpackHandle{}
)

func newLogStore(p *pebbledb.DB) *logStore { return &logStore{pebble: p} }

func logKey(index uint64) []byte {
	out := make([]byte, len(logKeyPrefix)+8)
	copy(out, logKeyPrefix)
	binary.BigEndian.PutUint64(out[len(logKeyPrefix):], index)
	return out
}

func indexFromKey(key []byte) (uint64, bool) {
	if !bytes.HasPrefix(key, logKeyPrefix) {
		return 0, false
	}
	suffix := key[len(logKeyPrefix):]
	if len(suffix) != 8 {
		return 0, false
	}
	return binary.BigEndian.Uint64(suffix), true
}

func encodeLog(log *hraft.Log) ([]byte, error) {
	var buf bytes.Buffer
	enc := codec.NewEncoder(&buf, msgpackHandle)
	if err := enc.Encode(log); err != nil {
		return nil, fmt.Errorf("encode log: %w", err)
	}
	return buf.Bytes(), nil
}

func decodeLog(value []byte, out *hraft.Log) error {
	dec := codec.NewDecoderBytes(value, msgpackHandle)
	if err := dec.Decode(out); err != nil {
		return fmt.Errorf("decode log: %w", err)
	}
	return nil
}

// FirstIndex returns the first index stored, or 0 for an empty store.
func (s *logStore) FirstIndex() (uint64, error) {
	iter, err := s.pebble.NewIter(&pebbledb.IterOptions{LowerBound: logKeyPrefix, UpperBound: logKeyPrefixEnd})
	if err != nil {
		return 0, err
	}
	defer iter.Close()
	if !iter.First() {
		return 0, iter.Error()
	}
	idx, ok := indexFromKey(iter.Key())
	if !ok {
		return 0, fmt.Errorf("logStore: malformed key %q", iter.Key())
	}
	return idx, iter.Error()
}

// LastIndex returns the last index stored, or 0 for an empty store.
func (s *logStore) LastIndex() (uint64, error) {
	iter, err := s.pebble.NewIter(&pebbledb.IterOptions{LowerBound: logKeyPrefix, UpperBound: logKeyPrefixEnd})
	if err != nil {
		return 0, err
	}
	defer iter.Close()
	if !iter.Last() {
		return 0, iter.Error()
	}
	idx, ok := indexFromKey(iter.Key())
	if !ok {
		return 0, fmt.Errorf("logStore: malformed key %q", iter.Key())
	}
	return idx, iter.Error()
}

// GetLog loads a log entry by index into the provided log struct. Returns
// hraft.ErrLogNotFound when the index is missing — raft uses this sentinel
// to decide whether to send a snapshot vs. catch-up entries.
func (s *logStore) GetLog(index uint64, log *hraft.Log) error {
	val, closer, err := s.pebble.Get(logKey(index))
	if errors.Is(err, pebbledb.ErrNotFound) {
		return errLogNotFound
	}
	if err != nil {
		return err
	}
	defer closer.Close()
	return decodeLog(val, log)
}

// StoreLog appends a single log entry.
func (s *logStore) StoreLog(log *hraft.Log) error {
	enc, err := encodeLog(log)
	if err != nil {
		return err
	}
	return s.pebble.Set(logKey(log.Index), enc, pebbledb.NoSync)
}

// StoreLogs appends a batch of log entries atomically. raft calls this
// for newly-committed entries; the batch keeps them consistent under a
// crash mid-write.
func (s *logStore) StoreLogs(logs []*hraft.Log) error {
	if len(logs) == 0 {
		return nil
	}
	b := s.pebble.NewBatch()
	defer b.Close()
	for _, log := range logs {
		enc, err := encodeLog(log)
		if err != nil {
			return err
		}
		if err := b.Set(logKey(log.Index), enc, nil); err != nil {
			return err
		}
	}
	return b.Commit(pebbledb.NoSync)
}

// DeleteRange removes log entries in [min, max] inclusive. raft calls
// this for log compaction (after a snapshot) and to discard
// uncommitted-but-stored entries on a stale follower.
func (s *logStore) DeleteRange(min, max uint64) error {
	if min > max {
		return fmt.Errorf("logStore.DeleteRange: min=%d > max=%d", min, max)
	}
	// Pebble DeleteRange end-key is exclusive; pass max+1 to make the
	// range inclusive on both ends.
	end := logKey(max)
	endNext := make([]byte, len(end))
	copy(endNext, end)
	// Increment the 8-byte suffix by 1. Carry through if it overflows;
	// math/big would be overkill since the indices are uint64.
	for i := len(endNext) - 1; i >= len(logKeyPrefix); i-- {
		endNext[i]++
		if endNext[i] != 0 {
			break
		}
	}
	return s.pebble.DeleteRange(logKey(min), endNext, pebbledb.NoSync)
}
