package pebble

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	pebbledb "github.com/cockroachdb/pebble"

	"github.com/osvaldoandrade/codeq/internal/core/queuetopic"
	"github.com/osvaldoandrade/codeq/internal/metrics"
	"github.com/osvaldoandrade/codeq/pkg/domain"
)

const (
	pAbsolute            = namespace + "absolute/"
	pAbsoluteRefs        = namespace + "absolute-refs/"
	pAbsoluteTopics      = namespace + "absolute-topics/"
	absoluteCountKey     = namespace + "absolute-count"
	absoluteReplayWindow = 48 * time.Hour
	// Metadata slots are reserved at acceptance, so cleanup never has to retain
	// payload because the tombstone capacity is full. No slots for legacy tasks.
	maxAbsoluteIdentities = 1_000_000
)

// TopicPolicyReader is the consumer-owned canonical policy boundary. One
// catalog is shared by local shards; no task payload can select its authority.
type TopicPolicyReader interface {
	Get(context.Context, string, string) (queuetopic.Topic, error)
}

// SetTopicPolicyReader must run before requests/reapers start, like AttachReplicator.
func (d *DB) SetTopicPolicyReader(reader TopicPolicyReader) { d.policyReader = reader }

func (d *DB) expiryTime() time.Time {
	if d.expiryNow != nil {
		return d.expiryNow().UTC()
	}
	return time.Now().UTC()
}
func absoluteKey(id string) []byte  { return []byte(pAbsolute + id) }
func absoluteRefs(id string) []byte { return []byte(pAbsoluteRefs + id + "/") }
func absoluteRefKey(id string, key []byte) []byte {
	return append(absoluteRefs(id), base64.RawURLEncoding.EncodeToString(key)...)
}

// No payload, result, webhook, progress, credential or arbitrary metadata.
type absoluteIdentity struct {
	ID          string     `json:"id"`
	TenantID    string     `json:"tenantId"`
	TopicID     string     `json:"topicId"`
	Hash        string     `json:"hash"`
	CreatedAt   time.Time  `json:"createdAt"`
	ExpiresAt   time.Time  `json:"expiresAt"`
	RetiredAt   *time.Time `json:"retiredAt,omitempty"`
	ReplayUntil time.Time  `json:"replayUntil"`
}

// SetAbsoluteExpiryCapacity configures an operational metadata budget, not a
// product quota. It must run once before requests/reapers start. The existing
// installation owns any resource/capacity change.
func (d *DB) SetAbsoluteExpiryCapacity(capacity int, shard string) error {
	if capacity < 0 || shard == "" {
		return errors.New("invalid absolute expiry budget")
	}
	if capacity == 0 {
		capacity = maxAbsoluteIdentities
	}
	d.absoluteCapacity = int64(capacity)
	d.absoluteShard = shard
	if _, err := d.absoluteCount(); err != nil {
		return err
	}
	d.observeAbsoluteCount()
	return nil
}

func (d *DB) identityCapacity() int64 {
	if d.absoluteCapacity > 0 {
		return d.absoluteCapacity
	}
	return maxAbsoluteIdentities
}

func (d *DB) observeAbsoluteCount() {
	if d.absoluteShard == "" {
		return
	}
	if count, err := d.absoluteCount(); err == nil {
		metrics.AbsoluteIdentityUsage.WithLabelValues(d.absoluteShard).Set(float64(count))
		metrics.AbsoluteIdentityCapacity.WithLabelValues(d.absoluteShard).Set(float64(d.identityCapacity()))
	}
}

func taskTopicID(task *domain.Task) string {
	return queuetopic.PhysicalID(task.TenantID, strings.TrimPrefix(string(task.Command), task.TenantID+"."))
}
func absoluteTopicPrefix(topicID string) []byte  { return []byte(pAbsoluteTopics + topicID + "/") }
func absoluteTopicKey(topicID, id string) []byte { return append(absoluteTopicPrefix(topicID), id...) }

// HasAbsoluteTopicCustody includes active tasks and the explicit replay window.
// Policy disable/delete is safe only after the retained identity set is empty.
func (d *DB) HasAbsoluteTopicCustody(ctx context.Context, tenant, name string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if src, ok := d.repl.(leadershipSource); ok {
		if !src.IsLeader() {
			return true, nil
		}
		if err := src.Barrier(ctx); err != nil {
			return false, err
		}
		if !src.IsLeader() {
			return true, nil
		}
	}
	prefix := absoluteTopicPrefix(queuetopic.PhysicalID(tenant, name))
	it, err := d.Iter(prefix, prefixUpper(prefix))
	if err != nil {
		return false, err
	}
	found := it.First()
	err = it.Error()
	if ce := it.Close(); err == nil {
		err = ce
	}
	return found, err
}

func taskContentHash(cmd domain.Command, payload string) string {
	h := sha256.Sum256([]byte(string(cmd) + "\x00" + payload))
	return hex.EncodeToString(h[:])
}

func (d *DB) topicPolicy(ctx context.Context, cmd domain.Command, tenant string) (queuetopic.Topic, bool, error) {
	if d.policyReader == nil {
		return queuetopic.Topic{}, false, nil
	}
	name := strings.TrimPrefix(string(cmd), tenant+".")
	topic, err := d.policyReader.Get(ctx, tenant, name)
	var missing *queuetopic.NotFoundError
	if errors.As(err, &missing) {
		return queuetopic.Topic{}, false, nil
	}
	if err != nil {
		return queuetopic.Topic{}, false, err
	}
	if topic.TenantID != tenant || topic.TopicName != name {
		return queuetopic.Topic{}, false, errors.New("topic policy identity mismatch")
	}
	return topic, true, nil
}

func (d *DB) acceptTaskExpiry(ctx context.Context, task *domain.Task) error {
	topic, exists, err := d.topicPolicy(ctx, task.Command, task.TenantID)
	if err != nil {
		return err
	}
	requested := domain.RequestedTaskExpiry(ctx)
	if !exists || !topic.Policy.AbsoluteExpiry {
		if !requested.IsZero() {
			return errors.New("'expiresAt' requires an absoluteExpiry topic")
		}
		return nil
	}
	if requested.IsZero() {
		if !d.DurableWrites() {
			return errors.New("absoluteExpiry requires fsyncOnCommit=true on every storage member")
		}
		return errors.New("'expiresAt' is required for this topic")
	}
	if !d.DurableWrites() {
		return errors.New("absoluteExpiry requires fsyncOnCommit=true on every storage member")
	}
	if !requested.After(task.CreatedAt) {
		return domain.ErrTaskExpired
	}
	if requested.After(task.CreatedAt.Add(time.Duration(topic.Policy.RetentionSeconds) * time.Second)) {
		return errors.New("'expiresAt' exceeds topic retention")
	}
	value := requested.UTC()
	task.ExpiresAt = &value
	return d.checkTaskExpiry(task)
}

func (d *DB) validateExpiryIntent(ctx context.Context, cmd domain.Command, tenant string) (bool, error) {
	topic, exists, err := d.topicPolicy(ctx, cmd, tenant)
	if err != nil {
		return false, err
	}
	absolute := exists && topic.Policy.AbsoluteExpiry
	if absolute && !d.DurableWrites() {
		return false, errors.New("absoluteExpiry requires fsyncOnCommit=true on every storage member")
	}
	if absolute && domain.RequestedTaskExpiry(ctx).IsZero() {
		return false, errors.New("'expiresAt' is required for this topic")
	}
	if !absolute && !domain.RequestedTaskExpiry(ctx).IsZero() {
		return false, errors.New("'expiresAt' requires an absoluteExpiry topic")
	}
	return absolute, nil
}

func (d *DB) taskDeadline(task *domain.Task) (time.Time, error) {
	if task.ExpiresAt == nil {
		return time.Time{}, nil
	}
	deadline := *task.ExpiresAt
	if deadline.IsZero() {
		return time.Time{}, domain.ErrTaskExpired
	}
	topic, exists, err := d.topicPolicy(context.Background(), task.Command, task.TenantID)
	if err != nil {
		return time.Time{}, err
	}
	if d.policyReader != nil && (!exists || !topic.Policy.AbsoluteExpiry) {
		return task.CreatedAt, nil
	}
	if exists {
		deadline = applyAbsolutePolicyDeadline(task, topic.Policy, deadline)
	}
	return deadline, nil
}

func applyAbsolutePolicyDeadline(task *domain.Task, policy queuetopic.Policy, deadline time.Time) time.Time {
	cutoff := task.CreatedAt.Add(time.Duration(policy.RetentionSeconds) * time.Second)
	if cutoff.Before(deadline) {
		deadline = cutoff
	}
	if policy.VisibilityFloor != nil && !task.CreatedAt.After(*policy.VisibilityFloor) {
		return task.CreatedAt
	}
	return deadline
}

func (d *DB) checkTaskExpiry(task *domain.Task) error {
	if task.ExpiresAt == nil {
		return nil
	}
	deadline, err := d.taskDeadline(task)
	if err != nil {
		return err
	}
	if !d.expiryTime().Before(deadline) {
		return domain.ErrTaskExpired
	}
	return nil
}

func taskTTLScore(task *domain.Task, fallback time.Time) uint64 {
	if task.ExpiresAt == nil {
		return unixSeconds(fallback)
	}
	deadline := *task.ExpiresAt
	if deadline.Nanosecond() != 0 {
		deadline = deadline.Truncate(time.Second).Add(time.Second)
	}
	return unixSeconds(deadline)
}

func (d *DB) limitTaskLease(task *domain.Task, until time.Time) (time.Time, error) {
	if task.ExpiresAt == nil {
		return until, nil
	}
	deadline, err := d.taskDeadline(task)
	if err != nil {
		return time.Time{}, err
	}
	if deadline.Before(until) {
		return deadline, nil
	}
	return until, nil
}

func taskLeaseText(task *domain.Task, until time.Time) string {
	if task.ExpiresAt != nil {
		return until.UTC().Format(time.RFC3339Nano)
	}
	return until.UTC().Format(time.RFC3339)
}

func taskLeaseUnix(task *domain.Task, until time.Time) int64 {
	if task.ExpiresAt == nil {
		return until.Unix()
	}
	if until.Nanosecond() != 0 {
		return until.Unix() + 1
	}
	return until.Unix()
}

func (d *DB) readAbsoluteIdentity(id string) (*absoluteIdentity, error) {
	body, err := d.Get(absoluteKey(id))
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var value absoluteIdentity
	if err := json.Unmarshal(body, &value); err != nil || value.ID != id || len(value.Hash) != 64 || value.ExpiresAt.IsZero() || value.CreatedAt.IsZero() || value.ReplayUntil.IsZero() {
		return nil, errors.New("invalid absolute identity")
	}
	return &value, nil
}

func (d *DB) replayRetired(id, tenant string, cmd domain.Command, payload string) error {
	identity, err := d.readAbsoluteIdentity(id)
	if err != nil {
		return err
	}
	if identity == nil || !d.expiryTime().Before(identity.ReplayUntil) {
		return nil
	}
	if identity.TenantID != tenant || identity.Hash != taskContentHash(cmd, payload) {
		return domain.ErrTaskIDConflict
	}
	return domain.ErrTaskExpired
}

// Absolute writes share a short critical section with retirement. This checks
// the final proposed write, so a result read before expiry cannot resurrect a
// task after the reaper removed it. Legacy batches keep the original fast path.
func absoluteTasksInBatch(b *pebbledb.Batch) (map[string]*domain.Task, error) {
	tasks := map[string]*domain.Task{}
	reader := b.Reader()
	for {
		kind, key, value, ok, err := reader.Next()
		if err != nil {
			return nil, err
		}
		if !ok {
			return tasks, nil
		}
		if kind != pebbledb.InternalKeyKindSet || !bytes.HasPrefix(key, []byte(pTasks)) || !bytes.Contains(value, []byte(`"expiresAt"`)) {
			continue
		}
		var task domain.Task
		if err := json.Unmarshal(value, &task); err != nil {
			return nil, err
		}
		if task.ExpiresAt == nil {
			continue
		}
		if !bytes.Equal(key, KeyTask(task.ID)) {
			return nil, errors.New("absolute task key mismatch")
		}
		tasks[task.ID] = &task
	}
}

func (d *DB) prepareAbsoluteBatch(b *pebbledb.Batch) (func(), error) {
	tasks, err := absoluteTasksInBatch(b)
	if err != nil || len(tasks) == 0 {
		return nil, err
	}
	if !d.DurableWrites() {
		return nil, errors.New("absoluteExpiry requires fsyncOnCommit=true on every storage member")
	}
	policyUnlock := func() {}
	if coordinator, ok := d.policyReader.(interface{ AcquireAbsolutePolicyRead() func() }); ok {
		policyUnlock = coordinator.AcquireAbsolutePolicyRead()
	}
	d.absoluteMu.Lock()
	unlock := func() { d.absoluteMu.Unlock(); policyUnlock() }
	fail := func(err error) (func(), error) { unlock(); return nil, err }
	count, err := d.absoluteCount()
	if err != nil {
		return fail(err)
	}
	added, err := d.stageAbsoluteTasks(b, tasks)
	if err != nil {
		return fail(err)
	}
	if err = d.stageAbsoluteReservation(b, count, added); err != nil {
		return fail(err)
	}
	if err := d.stageAbsoluteReferences(b, tasks); err != nil {
		return fail(err)
	}
	return unlock, nil
}

func (d *DB) stageAbsoluteReservation(b *pebbledb.Batch, count, added int64) error {
	if added > 0 && count+added > d.identityCapacity() {
		d.noteAbsoluteRefusal()
		return domain.ErrAbsoluteExpiryCapacity
	}
	if added == 0 {
		return nil
	}
	return b.Set([]byte(absoluteCountKey), []byte(strconv.FormatInt(count+added, 10)), nil)
}

func (d *DB) stageAbsoluteTasks(b *pebbledb.Batch, tasks map[string]*domain.Task) (int64, error) {
	added := int64(0)
	for _, task := range tasks {
		if err := d.checkTaskExpiry(task); err != nil {
			return 0, err
		}
		fresh, err := d.stageAbsoluteIdentity(b, task)
		if err != nil {
			return 0, err
		}
		if fresh {
			added++
		}
	}
	return added, nil
}

func (d *DB) noteAbsoluteRefusal() {
	if d.absoluteShard != "" {
		metrics.AbsoluteIdentityRefused.WithLabelValues(d.absoluteShard).Inc()
	}
}

func (d *DB) stageAbsoluteIdentity(b *pebbledb.Batch, task *domain.Task) (bool, error) {
	identity, err := d.readAbsoluteIdentity(task.ID)
	if err != nil {
		return false, err
	}
	fresh := identity == nil
	if fresh {
		identity = newAbsoluteIdentity(task)
	} else if !d.expiryTime().Before(identity.ReplayUntil) {
		if err = b.Delete(absoluteTopicKey(identity.TopicID, task.ID), nil); err != nil {
			return false, err
		}
		identity = newAbsoluteIdentity(task)
	} else if err = validateAbsoluteIdentity(identity, task); err != nil {
		return false, err
	}
	body, err := json.Marshal(identity)
	if err != nil {
		return false, err
	}
	if err = b.Set(absoluteKey(task.ID), body, nil); err != nil {
		return false, err
	}
	return fresh, b.Set(absoluteTopicKey(identity.TopicID, task.ID), nil, nil)
}

func newAbsoluteIdentity(task *domain.Task) *absoluteIdentity {
	return &absoluteIdentity{ID: task.ID, TenantID: task.TenantID, TopicID: taskTopicID(task), Hash: taskContentHash(task.Command, task.Payload), CreatedAt: task.CreatedAt, ExpiresAt: *task.ExpiresAt, ReplayUntil: task.ExpiresAt.Add(absoluteReplayWindow)}
}

func validateAbsoluteIdentity(identity *absoluteIdentity, task *domain.Task) error {
	if identity.TenantID != task.TenantID || identity.Hash != taskContentHash(task.Command, task.Payload) || !identity.CreatedAt.Equal(task.CreatedAt) || !identity.ExpiresAt.Equal(*task.ExpiresAt) {
		return domain.ErrTaskIDConflict
	}
	if identity.RetiredAt != nil {
		return domain.ErrTaskExpired
	}
	return nil
}

func (d *DB) absoluteCount() (int64, error) {
	value, err := d.Get([]byte(absoluteCountKey))
	if errors.Is(err, ErrNotFound) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	count, err := strconv.ParseInt(string(value), 10, 64)
	if err != nil || count < 0 {
		return 0, errors.New("invalid absolute identity count")
	}
	return count, nil
}

type absoluteReference struct {
	id     string
	key    []byte
	remove bool
}

func (d *DB) stageAbsoluteReferences(b *pebbledb.Batch, tasks map[string]*domain.Task) error {
	// Copy first: appending to a batch invalidates its Reader's backing slice.
	refs, err := d.absoluteBatchReferences(b, tasks)
	if err != nil {
		return err
	}
	for _, ref := range refs {
		key := absoluteRefKey(ref.id, ref.key)
		if ref.remove {
			err = b.Delete(key, nil)
		} else {
			err = b.Set(key, ref.key, nil)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (d *DB) absoluteBatchReferences(b *pebbledb.Batch, tasks map[string]*domain.Task) ([]absoluteReference, error) {
	refs := []absoluteReference{}
	reader := b.Reader()
	for {
		kind, key, value, ok, err := reader.Next()
		if err != nil {
			return nil, err
		}
		if !ok {
			return refs, nil
		}
		if !absoluteReferenceNamespace(key) {
			continue
		}
		for id := range tasks {
			matches, err := d.absoluteReferenceMatches(kind, key, value, id)
			if err != nil {
				return nil, err
			}
			if matches {
				refs = append(refs, absoluteReference{id, bytes.Clone(key), kind == pebbledb.InternalKeyKindDelete})
			}
		}
	}
}

func absoluteReferenceNamespace(key []byte) bool {
	return bytes.HasPrefix(key, []byte(pQueue)) || bytes.HasPrefix(key, []byte(pTTL)) || absoluteMappingKey(key)
}

func absoluteMappingKey(key []byte) bool {
	return bytes.HasPrefix(key, []byte(pIdempo)) || bytes.HasPrefix(key, []byte(pDedupe))
}

func (d *DB) absoluteReferenceMatches(kind pebbledb.InternalKeyKind, key, value []byte, id string) (bool, error) {
	if !absoluteMappingKey(key) {
		return bytes.HasSuffix(key, []byte("/"+id)), nil
	}
	if kind != pebbledb.InternalKeyKindDelete {
		return string(value) == id, nil
	}
	holder, err := d.Get(key)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return false, err
	}
	return string(holder) == id, nil
}

func (d *DB) stageAbsolutePurge(b *pebbledb.Batch, identity *absoluteIdentity, task *domain.Task, now time.Time) error {
	id := identity.ID
	for _, key := range [][]byte{KeyTask(id), KeyResult(id), KeyLease(id)} {
		if err := b.Delete(key, nil); err != nil {
			return err
		}
	}
	hasRefs, err := d.purgeAbsoluteReferences(b, id)
	if err != nil {
		return err
	}
	if task != nil && !hasRefs {
		if err = d.purgeRestoredReferences(b, task); err != nil {
			return err
		}
	}
	if identity.RetiredAt == nil {
		retired := now
		identity.RetiredAt = &retired
		identity.ReplayUntil = now.Add(absoluteReplayWindow)
	}
	body, err := json.Marshal(identity)
	if err != nil {
		return err
	}
	return b.Set(absoluteKey(id), body, nil)
}

func (d *DB) purgeAbsoluteReferences(b *pebbledb.Batch, id string) (bool, error) {
	prefix := absoluteRefs(id)
	it, err := d.Iter(prefix, prefixUpper(prefix))
	if err != nil {
		return false, err
	}
	defer it.Close()
	hasRefs := it.First()
	for valid := hasRefs; valid; valid = it.Next() {
		key := it.Value()
		keep, err := d.absoluteMappingReused(key, id)
		if err != nil {
			return false, err
		}
		if keep {
			continue
		}
		if err = b.Delete(key, nil); err != nil {
			return false, err
		}
	}
	if err = it.Error(); err != nil {
		return false, err
	}
	return hasRefs, b.DeleteRange(prefix, prefixUpper(prefix), nil)
}

func (d *DB) absoluteMappingReused(key []byte, id string) (bool, error) {
	if !absoluteMappingKey(key) {
		return false, nil
	}
	holder, err := d.Get(key)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return false, err
	}
	return string(holder) != id, nil
}

func (d *DB) purgeRestoredReferences(b *pebbledb.Batch, task *domain.Task) error {
	// A restored row may predate its reverse-reference manifest.
	for _, bounds := range []func() ([]byte, []byte){func() ([]byte, []byte) { return PrefixPendingAllPrios(task.Command, task.TenantID) }, func() ([]byte, []byte) { return PrefixDelayed(task.Command, task.TenantID) }} {
		lo, hi := bounds()
		if err := d.purgeRestoredRange(b, task.ID, lo, hi); err != nil {
			return err
		}
	}
	if err := b.Delete(KeyInprog(task.Command, task.TenantID, task.ID), nil); err != nil {
		return err
	}
	return b.Delete(KeyDLQ(task.Command, task.TenantID, task.ID), nil)
}

func (d *DB) purgeRestoredRange(b *pebbledb.Batch, id string, lo, hi []byte) error {
	it, err := d.Iter(lo, hi)
	if err != nil {
		return err
	}
	defer it.Close()
	for valid := it.First(); valid; valid = it.Next() {
		if !bytes.HasSuffix(it.Key(), []byte("/"+id)) {
			continue
		}
		if err = b.Delete(it.Key(), nil); err != nil {
			return err
		}
	}
	return it.Error()
}

// sweepAbsolute rotates over a bounded slice of identities, so shorter policies
// are enforced without waiting for the original TTL and unexpired rows cannot
// permanently starve later IDs. Progress is best effort; a restart resumes at 0.
func (d *DB) sweepAbsolute(ctx context.Context, limit int) (int, error) {
	if limit <= 0 {
		limit = 512
	}
	if err := d.RequireWriteLeader(); err != nil {
		return 0, err
	}
	d.absoluteMu.Lock()
	defer d.absoluteMu.Unlock()
	ids, last, more, err := d.absoluteSweepPage(limit)
	if err != nil {
		return 0, err
	}
	removed := 0
	now := d.expiryTime()
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return removed, err
		}
		purged, err := d.sweepAbsoluteIdentity(id, now)
		if err != nil {
			return removed, err
		}
		if purged {
			removed++
		}
	}
	d.observeAbsoluteCount()
	if more {
		d.absoluteSweepAfter = last
	} else {
		d.absoluteSweepAfter = nil
	}
	return removed, nil
}

func (d *DB) absoluteSweepPage(limit int) ([]string, []byte, bool, error) {
	lo := []byte(pAbsolute)
	it, err := d.Iter(lo, prefixUpper(lo))
	if err != nil {
		return nil, nil, false, err
	}
	defer it.Close()
	valid := it.First()
	if d.absoluteSweepAfter != nil {
		valid = it.SeekGE(d.absoluteSweepAfter)
		if valid && bytes.Equal(it.Key(), d.absoluteSweepAfter) {
			valid = it.Next()
		}
	}
	ids := []string{}
	var last []byte
	for ; valid && len(ids) < limit; valid = it.Next() {
		ids = append(ids, string(it.Key()[len(lo):]))
		last = bytes.Clone(it.Key())
	}
	return ids, last, valid, it.Error()
}

func (d *DB) absoluteTaskRow(id string) (*domain.Task, error) {
	body, err := d.Get(KeyTask(id))
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var task domain.Task
	if err = json.Unmarshal(body, &task); err != nil {
		return nil, err
	}
	return &task, nil
}

func (d *DB) sweepAbsoluteIdentity(id string, now time.Time) (bool, error) {
	identity, err := d.readAbsoluteIdentity(id)
	if err != nil || identity == nil {
		return false, err
	}
	task, err := d.absoluteTaskRow(id)
	if err != nil {
		return false, err
	}
	expired, err := d.absoluteRowExpired(task, now)
	if err != nil || !expired {
		return false, err
	}
	b := d.Batch()
	defer b.Close()
	if err = d.stageAbsolutePurge(b, identity, task, now); err != nil {
		return false, err
	}
	if err = d.removeExpiredAbsoluteIdentity(b, identity, now); err != nil {
		return false, err
	}
	if err = d.commitBatchRaw(b); err != nil {
		return false, err
	}
	d.Leases.Delete(id)
	if task != nil {
		metrics.AbsoluteExpiredPurged.Inc()
	}
	return task != nil, nil
}

func (d *DB) removeExpiredAbsoluteIdentity(b *pebbledb.Batch, identity *absoluteIdentity, now time.Time) error {
	if now.Before(identity.ReplayUntil) {
		return nil
	}
	return d.stageAbsoluteIdentityRemoval(b, identity)
}

func (d *DB) absoluteRowExpired(task *domain.Task, now time.Time) (bool, error) {
	if task == nil {
		return true, nil
	}
	deadline, err := d.taskDeadline(task)
	return !deadline.IsZero() && !now.Before(deadline), err
}

func (d *DB) stageAbsoluteIdentityRemoval(b *pebbledb.Batch, identity *absoluteIdentity) error {
	if err := b.Delete(absoluteKey(identity.ID), nil); err != nil {
		return err
	}
	if err := b.Delete(absoluteTopicKey(identity.TopicID, identity.ID), nil); err != nil {
		return err
	}
	count, err := d.absoluteCount()
	if err != nil {
		return err
	}
	return b.Set([]byte(absoluteCountKey), []byte(strconv.FormatInt(max(0, count-1), 10)), nil)
}

func (d *DB) purgeAbsoluteRow(task *domain.Task) error {
	d.absoluteMu.Lock()
	defer d.absoluteMu.Unlock()
	if err := d.checkTaskExpiry(task); !errors.Is(err, domain.ErrTaskExpired) {
		return err
	}
	identity, err := d.readAbsoluteIdentity(task.ID)
	if err != nil {
		return err
	}
	restored := identity == nil
	if restored {
		identity = &absoluteIdentity{ID: task.ID, TenantID: task.TenantID, TopicID: taskTopicID(task), Hash: taskContentHash(task.Command, task.Payload), CreatedAt: task.CreatedAt, ExpiresAt: *task.ExpiresAt}
	}
	b := d.Batch()
	defer b.Close()
	if restored {
		// Cleanup must still retire an accepted/restored row when its identity
		// manifest is missing. Account it even above the operational budget;
		// new acceptance is refused at capacity, retirement is never blocked.
		count, err := d.absoluteCount()
		if err != nil {
			return err
		}
		if err = b.Set([]byte(absoluteCountKey), []byte(strconv.FormatInt(count+1, 10)), nil); err != nil {
			return err
		}
		if err = b.Set(absoluteTopicKey(identity.TopicID, task.ID), nil, nil); err != nil {
			return err
		}
	}
	if err = d.stageAbsolutePurge(b, identity, task, d.expiryTime()); err != nil {
		return err
	}
	if err = d.commitBatchRaw(b); err != nil {
		return fmt.Errorf("retire absolute task: %w", err)
	}
	d.Leases.Delete(task.ID)
	d.observeAbsoluteCount()
	metrics.AbsoluteExpiredPurged.Inc()
	return nil
}
