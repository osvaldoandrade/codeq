package raft

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"os"

	pebbledb "github.com/cockroachdb/pebble"
	hraft "github.com/hashicorp/raft"
)

// newSnapshotStore wraps hashicorp/raft's FileSnapshotStore for the
// SnapshotStore role. The interesting bits (capturing and restoring
// Pebble state) live in the FSM (see fsmSnapshot + fsm.Restore in
// fsm.go).
//
// retain bounds how many snapshots stay on disk; older ones are purged
// automatically after each new snapshot. Values below 1 select
// DefaultSnapshotRetain (hashicorp/raft rejects retain < 1).
func newSnapshotStore(dir string, retain int) (hraft.SnapshotStore, error) {
	if retain < 1 {
		retain = DefaultSnapshotRetain
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("snapshot dir %s: %w", dir, err)
	}
	return hraft.NewFileSnapshotStore(dir, retain, os.Stderr)
}

// Snapshot format (streamed into the raft SnapshotSink):
//
//	[4]  magic       "CDQS"
//	[4]  version     uint32 BE (currently 1)
//	loop:
//	  [1] tag         0x01=entry, 0x00=eof
//	  if entry:
//	    [4] klen     uint32 BE
//	    [klen] key   raw bytes
//	    [4] vlen     uint32 BE
//	    [vlen] val   raw bytes
//
// Compact, streamable, parse-able with stdlib only. The format is
// internal to codeq — bumping the version field is enough to break
// compatibility cleanly should we ever need to change the on-wire shape.
const (
	snapshotMagic       = "CDQS"
	snapshotVersionV1   = uint32(1)
	snapshotTagEntry    = byte(0x01)
	snapshotTagEOF      = byte(0x00)
	snapshotStatePrefix = "codeq/" // FSM-relevant key range
	snapshotStateEnd    = "codeq0" // one past 'codeq/' (0x30 > 0x2F)
)

// writeSnapshot streams the live codeq/ keys from the given Pebble
// snapshot into w. The caller passes a *pebble.Snapshot, not the raw
// DB, so the iterator sees a consistent point-in-time view even while
// new writes land concurrently.
func writeSnapshot(w io.Writer, snap *pebbledb.Snapshot) error {
	bw := bufio.NewWriter(w)

	if _, err := bw.WriteString(snapshotMagic); err != nil {
		return err
	}
	if err := binary.Write(bw, binary.BigEndian, snapshotVersionV1); err != nil {
		return err
	}

	iter, err := snap.NewIter(&pebbledb.IterOptions{
		LowerBound: []byte(snapshotStatePrefix),
		UpperBound: []byte(snapshotStateEnd),
	})
	if err != nil {
		return fmt.Errorf("snapshot iter: %w", err)
	}
	defer iter.Close()

	var lenBuf [4]byte
	for ok := iter.First(); ok; ok = iter.Next() {
		key := iter.Key()
		val := iter.Value()
		if uint64(len(key)) > uint64(^uint32(0)) { // #nosec G115 -- len is non-negative and widened.
			return fmt.Errorf("snapshot key exceeds uint32 length: %d", len(key))
		}
		if uint64(len(val)) > uint64(^uint32(0)) { // #nosec G115 -- len is non-negative and widened.
			return fmt.Errorf("snapshot value exceeds uint32 length: %d", len(val))
		}
		if err := bw.WriteByte(snapshotTagEntry); err != nil {
			return err
		}
		binary.BigEndian.PutUint32(lenBuf[:], uint32(len(key))) // #nosec G115 -- bounded above.
		if _, err := bw.Write(lenBuf[:]); err != nil {
			return err
		}
		if _, err := bw.Write(key); err != nil {
			return err
		}
		binary.BigEndian.PutUint32(lenBuf[:], uint32(len(val))) // #nosec G115 -- bounded above.
		if _, err := bw.Write(lenBuf[:]); err != nil {
			return err
		}
		if _, err := bw.Write(val); err != nil {
			return err
		}
	}
	if err := iter.Error(); err != nil {
		return fmt.Errorf("snapshot iter error: %w", err)
	}
	if err := bw.WriteByte(snapshotTagEOF); err != nil {
		return err
	}
	return bw.Flush()
}

// readSnapshot drains r and applies every (k, v) pair to db. Before
// streaming new keys in it wipes the existing codeq/ range so the
// follower's state ends up exactly matching the leader at snapshot time.
func readSnapshot(r io.Reader, db *pebbledb.DB, options ...*pebbledb.WriteOptions) error {
	writeOptions := pebbledb.NoSync
	if len(options) > 0 && options[0] != nil {
		writeOptions = options[0]
	}
	br := bufio.NewReader(r)
	if err := validateSnapshotHeader(br); err != nil {
		return err
	}
	// Replace, not merge: restore the leader's entire state range.
	if err := db.DeleteRange([]byte(snapshotStatePrefix), []byte(snapshotStateEnd), writeOptions); err != nil {
		return fmt.Errorf("snapshot wipe: %w", err)
	}
	return restoreSnapshotEntries(br, db, writeOptions)
}

func validateSnapshotHeader(br *bufio.Reader) error {
	header := make([]byte, len(snapshotMagic))
	if _, err := io.ReadFull(br, header); err != nil {
		return fmt.Errorf("snapshot header: %w", err)
	}
	if string(header) != snapshotMagic {
		return fmt.Errorf("snapshot magic mismatch: got %q, want %q", header, snapshotMagic)
	}
	var version uint32
	if err := binary.Read(br, binary.BigEndian, &version); err != nil {
		return fmt.Errorf("snapshot version: %w", err)
	}
	if version != snapshotVersionV1 {
		return fmt.Errorf("snapshot version %d not supported (this build expects %d)", version, snapshotVersionV1)
	}
	return nil
}

func restoreSnapshotEntries(br *bufio.Reader, db *pebbledb.DB, options *pebbledb.WriteOptions) error {
	batch := db.NewBatch()
	defer func() { _ = batch.Close() }()
	const flushEvery = 1024
	pending := 0
	for {
		key, value, eof, err := readSnapshotEntry(br)
		if err != nil {
			return err
		}
		if eof {
			break
		}
		if err := batch.Set(key, value, nil); err != nil {
			return fmt.Errorf("snapshot batch set: %w", err)
		}
		pending++
		if pending >= flushEvery {
			if err := batch.Commit(options); err != nil {
				return fmt.Errorf("snapshot batch commit: %w", err)
			}
			_ = batch.Close()
			batch = db.NewBatch()
			pending = 0
		}
	}
	if pending == 0 {
		return nil
	}
	if err := batch.Commit(options); err != nil {
		return fmt.Errorf("snapshot final commit: %w", err)
	}
	return nil
}

func readSnapshotEntry(br *bufio.Reader) ([]byte, []byte, bool, error) {
	tag, err := br.ReadByte()
	if err != nil {
		return nil, nil, false, fmt.Errorf("snapshot tag: %w", err)
	}
	if tag == snapshotTagEOF {
		return nil, nil, true, nil
	}
	if tag != snapshotTagEntry {
		return nil, nil, false, fmt.Errorf("snapshot: unknown tag 0x%02x", tag)
	}
	key, err := readSnapshotValue(br, "key")
	if err != nil {
		return nil, nil, false, err
	}
	value, err := readSnapshotValue(br, "val")
	return key, value, false, err
}

func readSnapshotValue(br *bufio.Reader, field string) ([]byte, error) {
	var length uint32
	if err := binary.Read(br, binary.BigEndian, &length); err != nil {
		return nil, fmt.Errorf("snapshot %s length: %w", field, err)
	}
	value := make([]byte, length)
	if _, err := io.ReadFull(br, value); err != nil {
		return nil, fmt.Errorf("snapshot %s: %w", field, err)
	}
	return value, nil
}
