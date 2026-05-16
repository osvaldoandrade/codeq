// Package pebble implements the codeq repository contracts on top of an
// embedded Pebble (CockroachDB's RocksDB-derived LSM KV) database. The
// Redis-on-the-wire data model is mapped onto a single namespace prefixed
// keyspace; queues and indexes are realized via range scans over ordered
// keys.
//
// Trade-offs vs the Redis backend:
//   - Embedded: one codeq process per Pebble directory (Pebble holds an
//     exclusive file lock). Horizontal scaling at the data layer is gone;
//     the win is no network RTT and no clock_gettime per op.
//   - No PubSub: SubscriptionRepository uses in-process channels; events
//     don't cross codeq instances.
//   - No TTL: background reapers (lease, ttl_index) replicate Redis's
//     server-side expiry.
//   - No Lua: hot paths become plain Go on top of pebble.Batch (atomic).
package pebble

import (
	"encoding/binary"
	"strings"

	"github.com/osvaldoandrade/codeq/pkg/domain"
)

// Key layout — all keys live under the "codeq/" namespace so the DB can be
// shared with unrelated state without prefix collisions. Numeric components
// that need to sort lexicographically are big-endian fixed-width bytes (8
// bytes for unix-nano / score, 1 byte for priority).
//
//	codeq/tasks/<id>                            → JSON task
//	codeq/results/<id>                          → JSON result record
//	codeq/idempo/<key>                          → task id
//	codeq/lease/<id>                            → worker id | until-unix
//	codeq/ttl/<expire_unix_be8>/<id>            → "" (range-scan reaper)
//
//	codeq/q/<cmd>/<tenant>/pending/<prio_be1>/<seq_be8>/<id> → ""
//	codeq/q/<cmd>/<tenant>/inprog/<id>                       → ""
//	codeq/q/<cmd>/<tenant>/delayed/<score_be8>/<id>          → ""
//	codeq/q/<cmd>/<tenant>/dlq/<id>                          → ""
//
// tenant=="" is encoded as the literal "_" so the key parser can split on
// "/" without losing the empty position. Commands are lowercased.

const (
	namespace = "codeq/"
	pTasks    = namespace + "tasks/"
	pResults  = namespace + "results/"
	pIdempo   = namespace + "idempo/"
	pLease    = namespace + "lease/"
	pTTL      = namespace + "ttl/"
	pQueue    = namespace + "q/"

	segPending = "/pending/"
	segInprog  = "/inprog/"
	segDelayed = "/delayed/"
	segDLQ     = "/dlq/"

	emptyTenant = "_"
)

func tenantSeg(tenantID string) string {
	if tenantID == "" {
		return emptyTenant
	}
	return tenantID
}

func cmdSeg(cmd domain.Command) string {
	return strings.ToLower(string(cmd))
}

func be8(n uint64) []byte {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], n)
	return b[:]
}

// ---------- single-key helpers ----------

func KeyTask(id string) []byte    { return []byte(pTasks + id) }
func KeyResult(id string) []byte  { return []byte(pResults + id) }
func KeyIdempo(key string) []byte { return []byte(pIdempo + key) }
func KeyLease(id string) []byte   { return []byte(pLease + id) }

// KeyTTLIndex encodes (expire_unix, id). Range-scanning over pTTL yields
// entries ordered by expiry — a reaper pops the lowest score first.
func KeyTTLIndex(expireUnix uint64, id string) []byte {
	k := make([]byte, 0, len(pTTL)+8+1+len(id))
	k = append(k, pTTL...)
	k = append(k, be8(expireUnix)...)
	k = append(k, '/')
	k = append(k, id...)
	return k
}

// ---------- queue helpers ----------

func queueBase(cmd domain.Command, tenantID string) string {
	return pQueue + cmdSeg(cmd) + "/" + tenantSeg(tenantID)
}

// KeyPending encodes (cmd, tenant, prio, seq, id). seq is a monotonic counter
// assigned at Enqueue time so a higher-prio scan returns FIFO order within a
// priority bucket. Higher seq = inserted later.
func KeyPending(cmd domain.Command, tenantID string, prio int, seq uint64, id string) []byte {
	base := queueBase(cmd, tenantID) + segPending
	k := make([]byte, 0, len(base)+1+1+8+1+len(id))
	k = append(k, base...)
	k = append(k, byte(prio))
	k = append(k, '/')
	k = append(k, be8(seq)...)
	k = append(k, '/')
	k = append(k, id...)
	return k
}

// PrefixPendingPrio yields the [lower, upper) bound for scanning every key
// at a single (cmd, tenant, prio). Caller uses NewIter with the bounds.
func PrefixPendingPrio(cmd domain.Command, tenantID string, prio int) (lower, upper []byte) {
	base := queueBase(cmd, tenantID) + segPending
	p := make([]byte, 0, len(base)+1+1)
	p = append(p, base...)
	p = append(p, byte(prio))
	p = append(p, '/')
	return p, prefixUpper(p)
}

// PrefixPendingAllPrios is the bound that walks every priority for a
// (cmd, tenant) in one scan — used by AdminQueues / QueueStats.
func PrefixPendingAllPrios(cmd domain.Command, tenantID string) (lower, upper []byte) {
	base := queueBase(cmd, tenantID) + segPending
	return []byte(base), prefixUpper([]byte(base))
}

func KeyInprog(cmd domain.Command, tenantID, id string) []byte {
	return []byte(queueBase(cmd, tenantID) + segInprog + id)
}

func PrefixInprog(cmd domain.Command, tenantID string) (lower, upper []byte) {
	p := []byte(queueBase(cmd, tenantID) + segInprog)
	return p, prefixUpper(p)
}

func KeyDelayed(cmd domain.Command, tenantID string, scoreUnix uint64, id string) []byte {
	base := queueBase(cmd, tenantID) + segDelayed
	k := make([]byte, 0, len(base)+8+1+len(id))
	k = append(k, base...)
	k = append(k, be8(scoreUnix)...)
	k = append(k, '/')
	k = append(k, id...)
	return k
}

// PrefixDelayedUpTo returns the [lower, upper) bound covering every delayed
// entry with score <= maxScoreUnix (inclusive). Used by MoveDueDelayed.
func PrefixDelayedUpTo(cmd domain.Command, tenantID string, maxScoreUnix uint64) (lower, upper []byte) {
	base := queueBase(cmd, tenantID) + segDelayed
	lower = []byte(base)
	upperPrefix := make([]byte, 0, len(base)+8+1)
	upperPrefix = append(upperPrefix, base...)
	upperPrefix = append(upperPrefix, be8(maxScoreUnix+1)...) // exclusive of next-score bucket
	return lower, upperPrefix
}

func PrefixDelayed(cmd domain.Command, tenantID string) (lower, upper []byte) {
	p := []byte(queueBase(cmd, tenantID) + segDelayed)
	return p, prefixUpper(p)
}

func KeyDLQ(cmd domain.Command, tenantID, id string) []byte {
	return []byte(queueBase(cmd, tenantID) + segDLQ + id)
}

func PrefixDLQ(cmd domain.Command, tenantID string) (lower, upper []byte) {
	p := []byte(queueBase(cmd, tenantID) + segDLQ)
	return p, prefixUpper(p)
}

// PrefixTTL covers the entire ttl_index for the reaper.
func PrefixTTL() (lower, upper []byte) {
	p := []byte(pTTL)
	return p, prefixUpper(p)
}

// PrefixLease covers every lease key for the expiry sweep.
func PrefixLease() (lower, upper []byte) {
	p := []byte(pLease)
	return p, prefixUpper(p)
}

// prefixUpper returns the smallest key strictly greater than every key
// starting with p. We rely on the fact that namespaces here always end in
// '/' so incrementing the last byte gives a clean upper bound.
func prefixUpper(p []byte) []byte {
	u := make([]byte, len(p))
	copy(u, p)
	for i := len(u) - 1; i >= 0; i-- {
		if u[i] < 0xff {
			u[i]++
			return u[:i+1]
		}
	}
	// All 0xff — no upper bound possible. Caller is expected to never hit
	// this since our prefixes are ASCII.
	return nil
}

// ParsePendingValue extracts the task ID from a pending key (the trailing
// segment after the seq). Used during scans where the value is empty.
func ParsePendingKey(key []byte) (id string, ok bool) {
	// pending/<prio_be1>/<seq_be8>/<id>
	// Locate the last '/' — id is everything after.
	for i := len(key) - 1; i >= 0; i-- {
		if key[i] == '/' {
			return string(key[i+1:]), true
		}
	}
	return "", false
}
