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
// retain=3 keeps the last three snapshots on disk; older ones are
// purged automatically. This is the hashicorp/raft default.
func newSnapshotStore(dir string) (hraft.SnapshotStore, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("snapshot dir %s: %w", dir, err)
	}
	return hraft.NewFileSnapshotStore(dir, 3, os.Stderr)
}

// Snapshot format (streamed into the raft SnapshotSink):
//
//   [4]  magic       "CDQS"
//   [4]  version     uint32 BE (currently 1)
//   loop:
//     [1] tag         0x01=entry, 0x00=eof
//     if entry:
//       [4] klen     uint32 BE
//       [klen] key   raw bytes
//       [4] vlen     uint32 BE
//       [vlen] val   raw bytes
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
		if err := bw.WriteByte(snapshotTagEntry); err != nil {
			return err
		}
		binary.BigEndian.PutUint32(lenBuf[:], uint32(len(key)))
		if _, err := bw.Write(lenBuf[:]); err != nil {
			return err
		}
		if _, err := bw.Write(key); err != nil {
			return err
		}
		binary.BigEndian.PutUint32(lenBuf[:], uint32(len(val)))
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
func readSnapshot(r io.Reader, db *pebbledb.DB) error {
	br := bufio.NewReader(r)

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

	// Replace, not merge: wipe the codeq/ range so the restored state
	// is exactly the leader's at snapshot time.
	if err := db.DeleteRange([]byte(snapshotStatePrefix), []byte(snapshotStateEnd), pebbledb.NoSync); err != nil {
		return fmt.Errorf("snapshot wipe: %w", err)
	}

	batch := db.NewBatch()
	defer batch.Close()
	const flushEvery = 1024
	pending := 0

	var lenBuf [4]byte
	for {
		tag, err := br.ReadByte()
		if err != nil {
			return fmt.Errorf("snapshot tag: %w", err)
		}
		if tag == snapshotTagEOF {
			break
		}
		if tag != snapshotTagEntry {
			return fmt.Errorf("snapshot: unknown tag 0x%02x", tag)
		}
		if _, err := io.ReadFull(br, lenBuf[:]); err != nil {
			return fmt.Errorf("snapshot klen: %w", err)
		}
		klen := binary.BigEndian.Uint32(lenBuf[:])
		key := make([]byte, klen)
		if _, err := io.ReadFull(br, key); err != nil {
			return fmt.Errorf("snapshot key: %w", err)
		}
		if _, err := io.ReadFull(br, lenBuf[:]); err != nil {
			return fmt.Errorf("snapshot vlen: %w", err)
		}
		vlen := binary.BigEndian.Uint32(lenBuf[:])
		val := make([]byte, vlen)
		if _, err := io.ReadFull(br, val); err != nil {
			return fmt.Errorf("snapshot val: %w", err)
		}
		if err := batch.Set(key, val, nil); err != nil {
			return fmt.Errorf("snapshot batch set: %w", err)
		}
		pending++
		if pending >= flushEvery {
			if err := batch.Commit(pebbledb.NoSync); err != nil {
				return fmt.Errorf("snapshot batch commit: %w", err)
			}
			batch.Close()
			batch = db.NewBatch()
			pending = 0
		}
	}
	if pending > 0 {
		if err := batch.Commit(pebbledb.NoSync); err != nil {
			return fmt.Errorf("snapshot final commit: %w", err)
		}
	}
	return nil
}
