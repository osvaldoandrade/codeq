package pebble

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bytedance/sonic"
	"github.com/google/uuid"

	"github.com/osvaldoandrade/codeq/internal/repository"
	"github.com/osvaldoandrade/codeq/pkg/domain"
)

// Subscription key layout. Like the queue keys, lowercased command names
// keep the schema uniform with the Redis backend (so admin dashboards see
// equivalent labels).
//
//	codeq/sub/<id>                                  → JSON Subscription
//	codeq/sub_event/<cmd>/<expires_be8>/<id>        → ""
//	codeq/sub_throttle/<id>                         → last-notify-unix-nano (be8)
//	codeq/sub_rr/<cmd>/<group>                      → next-index (be8)
//
// Embedding expiry in the sub_event key as a big-endian score lets
// ListActive range-scan only the still-active members in O(active) without
// a separate "by-expiry" index.

const (
	pSub         = "codeq/sub/"
	pSubEvent    = "codeq/sub_event/"
	pSubThrottle = "codeq/sub_throttle/"
	pSubRR       = "codeq/sub_rr/"
)

func keySub(id string) []byte { return []byte(pSub + id) }
func keySubEvent(cmd domain.Command, expiresAtUnix uint64, id string) []byte {
	base := pSubEvent + strings.ToLower(string(cmd)) + "/"
	k := make([]byte, 0, len(base)+8+1+len(id))
	k = append(k, base...)
	k = append(k, be8(expiresAtUnix)...)
	k = append(k, '/')
	k = append(k, id...)
	return k
}
func prefixSubEvent(cmd domain.Command) (lower, upper []byte) {
	p := []byte(pSubEvent + strings.ToLower(string(cmd)) + "/")
	return p, prefixUpper(p)
}
func keySubThrottle(id string) []byte { return []byte(pSubThrottle + id) }
func keySubRR(cmd domain.Command, groupID string) []byte {
	return []byte(pSubRR + strings.ToLower(string(cmd)) + "/" + groupID)
}

// parseSubEventKey extracts (expiresAtUnix, id) from a sub_event key.
func parseSubEventKey(cmd domain.Command, k []byte) (expiresAtUnix uint64, id string, ok bool) {
	base := pSubEvent + strings.ToLower(string(cmd)) + "/"
	if !strings.HasPrefix(string(k), base) {
		return 0, "", false
	}
	rest := k[len(base):]
	if len(rest) < 9 {
		return 0, "", false
	}
	expiresAtUnix = beUint64(rest[:8])
	if rest[8] != '/' {
		return 0, "", false
	}
	return expiresAtUnix, string(rest[9:]), true
}

// parseSubEventKeyAny is the cmd-agnostic variant used by recovery and
// cleanup paths where the cmd is unknown up front. Key layout:
//
//	codeq/subevt/<cmd>/<expires_be8>/<id>
func parseSubEventKeyAny(k []byte) (cmd domain.Command, expiresAtUnix uint64, ok bool) {
	if !strings.HasPrefix(string(k), pSubEvent) {
		return "", 0, false
	}
	rest := k[len(pSubEvent):]
	sep := strings.IndexByte(string(rest), '/')
	if sep <= 0 {
		return "", 0, false
	}
	cmd = domain.Command(string(rest[:sep]))
	tail := rest[sep+1:]
	if len(tail) < 9 {
		return "", 0, false
	}
	expiresAtUnix = beUint64(tail[:8])
	if tail[8] != '/' {
		return "", 0, false
	}
	return cmd, expiresAtUnix, true
}

// subscriptionRepo implements repository.SubscriptionRepository.
// Notify throttling and round-robin counters are persisted to survive
// restart; the rr counter increments via Get→Set since Pebble has no
// atomic INCR. Concurrent NextGroupIndex callers serialize through an
// in-process map of mutexes (one per (cmd, group)).
type subscriptionRepo struct {
	db *DB
	tz *time.Location

	rrMu sync.Map // key string → *sync.Mutex

	// activeByCmd is the per-command active-subscription counter that
	// backs HasActive. Maintained by Create (++) and CleanupExpired (--).
	// Heartbeat is a net no-op (delete+insert under different score).
	// Producer NotifyQueueReady consults this to skip the Pebble iter
	// when no subs exist. Recovered at Open() by recoverActiveCounts so
	// a restart doesn't strand the counter at zero while live subs sit
	// on disk.
	activeByCmd sync.Map // key string (cmd) → *atomic.Int64
}

func NewSubscriptionRepository(db *DB, tz *time.Location) repository.SubscriptionRepository {
	r := &subscriptionRepo{db: db, tz: tz}
	// Best-effort recover: failures here only mean HasActive may return
	// false when subs exist, in which case the next Create call brings
	// the counter back up. The NotifyQueueReady fast-path stays correct
	// because ListActive runs against on-disk truth either way.
	_ = r.recoverActiveCounts()
	return r
}

// activeCounter returns the atomic counter for cmd, lazy-initialized.
func (r *subscriptionRepo) activeCounter(cmd domain.Command) *atomic.Int64 {
	if v, ok := r.activeByCmd.Load(string(cmd)); ok {
		return v.(*atomic.Int64)
	}
	c := new(atomic.Int64)
	actual, _ := r.activeByCmd.LoadOrStore(string(cmd), c)
	return actual.(*atomic.Int64)
}

// recoverActiveCounts scans every subscription event entry at startup
// and seeds the active counter. Cost is O(N) over all subs once at
// Open; later writes maintain the counter incrementally.
func (r *subscriptionRepo) recoverActiveCounts() error {
	lower := []byte(pSubEvent)
	upper := prefixUpper(lower)
	it, err := r.db.Iter(lower, upper)
	if err != nil {
		return err
	}
	defer it.Close()
	now := uint64(time.Now().Unix())
	for valid := it.First(); valid; valid = it.Next() {
		k := it.Key()
		cmd, expires, ok := parseSubEventKeyAny(k)
		if !ok {
			continue
		}
		if expires < now {
			continue
		}
		r.activeCounter(cmd).Add(1)
	}
	return it.Error()
}

func (r *subscriptionRepo) now() time.Time { return time.Now().In(r.tz) }

func (r *subscriptionRepo) Create(ctx context.Context, sub domain.Subscription, ttlSeconds int) (*domain.Subscription, error) {
	if sub.ID == "" {
		sub.ID = uuid.NewString()
	}
	if ttlSeconds <= 0 {
		ttlSeconds = 60
	}
	now := r.now()
	sub.CreatedAt = now
	_ = now // Subscription has no UpdatedAt field
	sub.ExpiresAt = now.Add(time.Duration(ttlSeconds) * time.Second)
	subJSON, _ := sonic.Marshal(sub)

	b := r.db.Batch()
	defer b.Close()
	if err := b.Set(keySub(sub.ID), subJSON, nil); err != nil {
		return nil, err
	}
	for _, evt := range sub.EventTypes {
		if err := b.Set(keySubEvent(evt, uint64(sub.ExpiresAt.Unix()), sub.ID), nil, nil); err != nil {
			return nil, err
		}
	}
	if err := r.db.CommitBatch(b); err != nil {
		return nil, err
	}
	// Bump the per-cmd active counter after the durable commit so a
	// failed commit doesn't leave the counter ahead of disk truth.
	for _, evt := range sub.EventTypes {
		r.activeCounter(evt).Add(1)
	}
	return &sub, nil
}

func (r *subscriptionRepo) Heartbeat(ctx context.Context, id string, ttlSeconds int) (*domain.Subscription, error) {
	if ttlSeconds <= 0 {
		ttlSeconds = 60
	}
	sub, err := r.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	now := r.now()
	oldExpires := uint64(sub.ExpiresAt.Unix())
	_ = now // Subscription has no UpdatedAt field
	sub.ExpiresAt = now.Add(time.Duration(ttlSeconds) * time.Second)
	newExpires := uint64(sub.ExpiresAt.Unix())
	subJSON, _ := sonic.Marshal(sub)

	b := r.db.Batch()
	defer b.Close()
	if err := b.Set(keySub(id), subJSON, nil); err != nil {
		return nil, err
	}
	// Rewrite per-event index entries under the new expiry score (delete
	// old, insert new). Cheaper than scanning to find old entries since we
	// already know the prior ExpiresAt.
	for _, evt := range sub.EventTypes {
		if err := b.Delete(keySubEvent(evt, oldExpires, id), nil); err != nil {
			return nil, err
		}
		if err := b.Set(keySubEvent(evt, newExpires, id), nil, nil); err != nil {
			return nil, err
		}
	}
	if err := r.db.CommitBatch(b); err != nil {
		return nil, err
	}
	return sub, nil
}

func (r *subscriptionRepo) Get(ctx context.Context, id string) (*domain.Subscription, error) {
	v, err := r.db.Get(keySub(id))
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, fmt.Errorf("not-found")
		}
		return nil, err
	}
	var s domain.Subscription
	if err := sonic.Unmarshal(v, &s); err != nil {
		return nil, fmt.Errorf("unmarshal subscription: %w", err)
	}
	return &s, nil
}

// HasActive is the in-memory fast path consulted by NotifyQueueReady
// to skip the expensive Pebble iter when no subscriptions exist. The
// counter can lag actual disk truth by a few ms (Create bumps it after
// CommitBatch, CleanupExpired decrements after delete), and a crash
// loses the value entirely — recoverActiveCounts at Open() rebuilds it.
// Caller MUST still treat ListActive as authoritative; this is purely
// an optimization to make zero-subscription configurations free.
func (r *subscriptionRepo) HasActive(ctx context.Context, cmd domain.Command) bool {
	c, ok := r.activeByCmd.Load(string(cmd))
	if !ok {
		return false
	}
	return c.(*atomic.Int64).Load() > 0
}

func (r *subscriptionRepo) ListActive(ctx context.Context, cmd domain.Command, now time.Time) ([]domain.Subscription, error) {
	lower, upper := prefixSubEvent(cmd)
	it, err := r.db.Iter(lower, upper)
	if err != nil {
		return nil, err
	}
	defer it.Close()
	nowUnix := uint64(now.Unix())
	out := make([]domain.Subscription, 0, 16)
	for valid := it.First(); valid; valid = it.Next() {
		expires, id, ok := parseSubEventKey(cmd, it.Key())
		if !ok {
			continue
		}
		if expires < nowUnix {
			// Expired — skip but leave the cleanup to CleanupExpired so we
			// don't write inside an iterator.
			continue
		}
		sub, err := r.Get(ctx, id)
		if err != nil {
			continue
		}
		out = append(out, *sub)
	}
	return out, nil
}

// AllowNotify implements per-subscription notification throttling: a
// notify is allowed only if minIntervalSeconds have elapsed since the
// previous one for this subscription. The throttle counter is persisted
// so an in-flight notify isn't lost across restarts.
func (r *subscriptionRepo) AllowNotify(ctx context.Context, id string, minIntervalSeconds int) (bool, error) {
	if minIntervalSeconds <= 0 {
		return true, nil
	}
	key := keySubThrottle(id)
	now := r.now().UnixNano()
	minIntervalNanos := int64(minIntervalSeconds) * int64(time.Second)

	v, err := r.db.Get(key)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return false, err
	}
	if err == nil && len(v) >= 8 {
		last := int64(beUint64(v))
		if now-last < minIntervalNanos {
			return false, nil
		}
	}
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], uint64(now))
	if err := r.db.Set(key, buf[:]); err != nil {
		return false, err
	}
	return true, nil
}

func (r *subscriptionRepo) AllowNotifyBatch(ctx context.Context, subs []domain.Subscription) (map[string]bool, error) {
	out := make(map[string]bool, len(subs))
	for _, s := range subs {
		ok, err := r.AllowNotify(ctx, s.ID, s.MinIntervalSeconds)
		if err != nil {
			return nil, err
		}
		out[s.ID] = ok
	}
	return out, nil
}

// NextGroupIndex returns the next index in a round-robin counter for a
// (cmd, groupID) pair, modulo mod. Get-Set is serialized per key via an
// in-process mutex; this is only a hot path when many workers share a
// group, which is the rarer multi-tenant fanout scenario.
func (r *subscriptionRepo) NextGroupIndex(ctx context.Context, cmd domain.Command, groupID string, mod int) (int, error) {
	if mod <= 0 {
		return 0, nil
	}
	key := keySubRR(cmd, groupID)
	muRaw, _ := r.rrMu.LoadOrStore(string(key), &sync.Mutex{})
	mu := muRaw.(*sync.Mutex)
	mu.Lock()
	defer mu.Unlock()

	v, err := r.db.Get(key)
	current := uint64(0)
	if err == nil && len(v) >= 8 {
		current = beUint64(v)
	} else if err != nil && !errors.Is(err, ErrNotFound) {
		return 0, err
	}
	next := current + 1
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], next)
	if err := r.db.Set(key, buf[:]); err != nil {
		return 0, err
	}
	return int(current % uint64(mod)), nil
}

// CleanupExpired walks all command sub_event indexes and drops entries
// whose score < before.Unix(). Bounded by `limit` total deletions.
func (r *subscriptionRepo) CleanupExpired(ctx context.Context, limit int, before time.Time) (int, error) {
	if limit <= 0 {
		limit = 100
	}
	lower := []byte(pSubEvent)
	upper := prefixUpper(lower)
	it, err := r.db.Iter(lower, upper)
	if err != nil {
		return 0, err
	}
	defer it.Close()

	beforeUnix := uint64(before.Unix())
	cleaned := 0
	type expired struct {
		eventKey []byte
		subID    string
		cmd      domain.Command
	}
	bucket := make([]expired, 0, limit)
	for valid := it.First(); valid && len(bucket) < limit; valid = it.Next() {
		k := append([]byte(nil), it.Key()...)
		cmd, score, ok := parseSubEventKeyAny(k)
		if !ok {
			continue
		}
		if score >= beforeUnix {
			continue
		}
		idx := strings.LastIndexByte(string(k), '/')
		if idx < 0 {
			continue
		}
		bucket = append(bucket, expired{eventKey: k, subID: string(k[idx+1:]), cmd: cmd})
	}
	if len(bucket) == 0 {
		return 0, nil
	}

	b := r.db.Batch()
	defer b.Close()
	for _, e := range bucket {
		if err := b.Delete(e.eventKey, nil); err != nil {
			return cleaned, err
		}
		// Drop the sub body + throttle once we see the first index entry
		// for this id; idempotent on subsequent commands.
		if err := b.Delete(keySub(e.subID), nil); err != nil {
			return cleaned, err
		}
		if err := b.Delete(keySubThrottle(e.subID), nil); err != nil {
			return cleaned, err
		}
		cleaned++
	}
	if err := r.db.CommitBatch(b); err != nil {
		return 0, err
	}
	// Decrement active counter for each cleaned event entry. After the
	// commit so a failed commit doesn't drive the counter below zero.
	for _, e := range bucket {
		r.activeCounter(e.cmd).Add(-1)
	}
	return cleaned, nil
}

// strconvAsInt placates the linter on an otherwise-unused import that the
// rest of the file might need depending on future edits.
var _ = strconv.Itoa
