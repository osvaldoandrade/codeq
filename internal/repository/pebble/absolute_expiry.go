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
//
// The page retires in batches of at most absoluteRetireBatch identities, one
// replicated commit each. It used to commit every identity on its own while
// holding absoluteMu, so on a slow leader a page of expired rows kept every
// absolute claim waiting behind hundreds of serial Raft round trips.
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
	retire, err := d.newAbsoluteRetirement()
	if err != nil {
		return 0, err
	}
	defer retire.close()
	removed := 0
	now := d.expiryTime()
	var stopErr error
	for _, id := range ids {
		if stopErr = ctx.Err(); stopErr != nil {
			break
		}
		n, err := retire.flushIfFull()
		removed += n
		if err != nil {
			return removed, err
		}
		if stopErr = retire.stageSweptIdentity(id, now); stopErr != nil {
			break
		}
	}
	// Whatever was staged before a cancellation or an error still commits,
	// like the identities the per-row sweep had already retired.
	n, err := retire.commit()
	removed += n
	if err != nil || stopErr != nil {
		return removed, joinErrors(stopErr, err)
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

// stageSweptIdentity stages the sweep's retirement of one identity: the purge
// of an expired (or missing) row and, once its replay window is over, the
// identity itself.
func (r *absoluteRetirement) stageSweptIdentity(id string, now time.Time) error {
	d := r.d
	identity, err := d.readAbsoluteIdentity(id)
	if err != nil || identity == nil {
		return err
	}
	task, err := d.absoluteTaskRow(id)
	if err != nil {
		return err
	}
	if settled, err := d.retiredTombstoneSettled(identity, task, now); err != nil || settled {
		return err
	}
	expired, err := d.absoluteRowExpired(task, now)
	if err != nil || !expired {
		return err
	}
	return r.stage(id, task != nil, nil, func(b *pebbledb.Batch) (int64, error) {
		if err := d.stageAbsolutePurge(b, identity, task, now); err != nil {
			return 0, err
		}
		// stageAbsolutePurge restarts the replay window of a row it retires
		// now, so only an identity retired earlier can leave here.
		if now.Before(identity.ReplayUntil) {
			return 0, nil
		}
		return -1, stageAbsoluteIdentityKeysRemoval(b, identity)
	})
}

// retiredTombstoneSettled reports a tombstone that has nothing left to purge:
// retired, no task row, no reverse references, replay window still open. The
// rotating sweep revisits every tombstone many times during that window;
// re-staging its purge would replicate point and range deletions of absent
// keys plus an identical identity rewrite on every visit, which only grows
// the Raft log, forces snapshots and leaves tombstones for compaction.
func (d *DB) retiredTombstoneSettled(identity *absoluteIdentity, task *domain.Task, now time.Time) (bool, error) {
	if identity.RetiredAt == nil || task != nil || !now.Before(identity.ReplayUntil) {
		return false, nil
	}
	prefix := absoluteRefs(identity.ID)
	it, err := d.Iter(prefix, prefixUpper(prefix))
	if err != nil {
		return false, err
	}
	residue := it.First()
	err = it.Error()
	if cerr := it.Close(); err == nil {
		err = cerr
	}
	return !residue, err
}

func (d *DB) absoluteRowExpired(task *domain.Task, now time.Time) (bool, error) {
	if task == nil {
		return true, nil
	}
	deadline, err := d.taskDeadline(task)
	return !deadline.IsZero() && !now.Before(deadline), err
}

func (d *DB) stageAbsoluteIdentityRemoval(b *pebbledb.Batch, identity *absoluteIdentity) error {
	if err := stageAbsoluteIdentityKeysRemoval(b, identity); err != nil {
		return err
	}
	count, err := d.absoluteCount()
	if err != nil {
		return err
	}
	return b.Set([]byte(absoluteCountKey), []byte(strconv.FormatInt(max(0, count-1), 10)), nil)
}

func stageAbsoluteIdentityKeysRemoval(b *pebbledb.Batch, identity *absoluteIdentity) error {
	if err := b.Delete(absoluteKey(identity.ID), nil); err != nil {
		return err
	}
	return b.Delete(absoluteTopicKey(identity.TopicID, identity.ID), nil)
}

// retireCompletedAbsolute ends an absolute task's custody the moment its
// consumer acknowledges success. Payload, result, lease, every queue/TTL/dedupe
// reference and the identity itself go in one replicated batch: an
// acknowledged task leaves nothing behind (ADR 0012). The consumer of an
// absolute topic owns replay protection for data it accepted (Managed Logs
// keeps per-event receipts in CefasDB), so publishing the same ID again later
// is a new task. Tasks retired by their deadline still keep the bounded
// replay tombstone.
//
// seen is the row the acknowledgement was validated against. Under the
// absolute lock the row is read again and must still be that in-progress,
// unexpired, unretired incarnation; a duplicate acknowledgement, a
// re-published ID or a row retired by its deadline in between is refused
// instead of being purged.
func (d *DB) retireCompletedAbsolute(seen *domain.Task) error {
	d.absoluteMu.Lock()
	defer d.absoluteMu.Unlock()
	task, err := d.acknowledgedAbsoluteRow(seen)
	if err != nil {
		return err
	}
	b := d.Batch()
	defer b.Close()
	if err = d.stageCompletedRetirement(b, task); err != nil {
		return err
	}
	if err = d.commitBatchRaw(b); err != nil {
		return fmt.Errorf("retire completed absolute task: %w", err)
	}
	d.Leases.Delete(task.ID)
	d.observeAbsoluteCount()
	metrics.AbsoluteCompletedPurged.Inc()
	return nil
}

// acknowledgedAbsoluteRow re-reads the row under the absolute lock and
// returns it only while it is still the in-progress, unexpired, unretired
// incarnation the acknowledgement was validated against.
func (d *DB) acknowledgedAbsoluteRow(seen *domain.Task) (*domain.Task, error) {
	raw, err := d.Get(KeyTask(seen.ID))
	if errors.Is(err, ErrNotFound) {
		return nil, errors.New("task not found")
	}
	if err != nil {
		return nil, err
	}
	var task domain.Task
	if err = json.Unmarshal(raw, &task); err != nil {
		return nil, fmt.Errorf("unmarshal task: %w", err)
	}
	if err = d.checkTaskExpiry(&task); err != nil {
		return nil, err
	}
	if task.ExpiresAt == nil || task.Status != domain.StatusInProgress || !task.CreatedAt.Equal(seen.CreatedAt) {
		return nil, errors.New("not-in-progress")
	}
	return &task, nil
}

// stageCompletedRetirement stages the purge of every trace of task and of
// its identity. A restored row may predate its identity manifest; it was
// never counted, so only its manifest keys are dropped.
func (d *DB) stageCompletedRetirement(b *pebbledb.Batch, task *domain.Task) error {
	identity, err := d.readAbsoluteIdentity(task.ID)
	if err != nil {
		return err
	}
	if identity != nil && identity.RetiredAt != nil {
		return errors.New("not-in-progress")
	}
	restored := identity == nil
	if restored {
		identity = &absoluteIdentity{ID: task.ID, TenantID: task.TenantID, TopicID: taskTopicID(task), Hash: taskContentHash(task.Command, task.Payload), CreatedAt: task.CreatedAt, ExpiresAt: *task.ExpiresAt}
	}
	if err = d.stageAbsolutePurge(b, identity, task, d.expiryTime()); err != nil {
		return err
	}
	if restored {
		return errors.Join(b.Delete(absoluteKey(identity.ID), nil), b.Delete(absoluteTopicKey(identity.TopicID, identity.ID), nil))
	}
	return d.stageAbsoluteIdentityRemoval(b, identity)
}

// purgeAbsoluteRows retires the expired rows among tasks with one replicated
// commit per absoluteRetireBatch rows instead of one per row, so a claim that
// meets a backlog of expired heads pays one Raft round trip, not hundreds.
//
// retired[i] reports that tasks[i] is gone (or was never there) and its hint
// can be dropped. A row that is not expired stays and retired[i] is false.
// A row that fails to stage stays too: its staging ran in a scratch batch and
// added nothing, and the rows after it still retire, so one bad row cannot
// pin the backlog behind it. The first staging error is returned after the
// commit. A failed commit leaves its rows unretired.
func (d *DB) purgeAbsoluteRows(tasks []*domain.Task) ([]bool, error) {
	retired := make([]bool, len(tasks))
	if len(tasks) == 0 {
		return retired, nil
	}
	d.absoluteMu.Lock()
	defer d.absoluteMu.Unlock()
	retire, err := d.newAbsoluteRetirement()
	if err != nil {
		return retired, err
	}
	defer retire.close()
	var stageErr error
	failed := 0
	for i, task := range tasks {
		if _, err := retire.flushIfFull(); err != nil {
			return retired, joinErrors(stageErr, fmt.Errorf("retire absolute tasks: %w", err))
		}
		if err := retire.stageExpiredRow(task, &retired[i]); err != nil {
			failed++
			if stageErr == nil {
				stageErr = fmt.Errorf("retire absolute task %s: %w", task.ID, err)
			}
		}
	}
	if failed > 1 {
		stageErr = fmt.Errorf("%d absolute rows failed to stage; first: %w", failed, stageErr)
	}
	if _, err := retire.commit(); err != nil {
		return retired, joinErrors(stageErr, fmt.Errorf("retire absolute tasks: %w", err))
	}
	return retired, stageErr
}

// absoluteRetireBatch bounds the rows one retirement commit carries. It is at
// least the default claim inspection budget, so a claim retires every expired
// head it inspected in one commit, and keeps one Raft entry near 150 KiB
// (about 600 bytes per retired row).
const absoluteRetireBatch = 256

// absoluteRetirement stages several retirements into one Pebble batch and
// replicates them in one commit. The caller holds absoluteMu from
// newAbsoluteRetirement through the last commit. The identity count is read
// when the retirement starts and again after every commit, and carried
// across the rows staged in between, so every row adjusts the running value
// instead of the stored one the batch is about to replace.
//
// A retirement belongs to one leadership term. If this node lost and
// regained leadership since it started, another leader may have written the
// rows and the count it read, so the rest is abandoned unretired
// (errAbsoluteRetirementTermChanged) and a later pass rereads them.
type absoluteRetirement struct {
	d       *DB
	b       *pebbledb.Batch
	scratch *pebbledb.Batch
	epoch   uint64
	count   int64
	changed bool
	ids     map[string]struct{}
	expired int
	// marks are set to true once the staged rows they belong to commit.
	marks []*bool
}

// errAbsoluteRetirementTermChanged reports staged retirements abandoned
// because leadership changed after they were read.
var errAbsoluteRetirementTermChanged = errors.New("leadership changed during absolute retirement; remaining rows left for a later pass")

func (d *DB) newAbsoluteRetirement() (*absoluteRetirement, error) {
	epoch := d.leadershipEpoch()
	count, err := d.absoluteCount()
	if err != nil {
		return nil, err
	}
	return &absoluteRetirement{d: d, b: d.Batch(), scratch: d.Batch(), epoch: epoch, count: count, ids: map[string]struct{}{}}, nil
}

// leadershipEpoch is the replicator's count of transitions to leader, or 0
// for standalone Pebble and replicators that do not report one.
func (d *DB) leadershipEpoch() uint64 {
	if src, ok := d.repl.(interface{ LeadershipEpoch() uint64 }); ok {
		return src.LeadershipEpoch()
	}
	return 0
}

func (r *absoluteRetirement) close() {
	_ = r.b.Close()
	_ = r.scratch.Close()
}

// flushIfFull commits the staged rows once they reach absoluteRetireBatch.
func (r *absoluteRetirement) flushIfFull() (int, error) {
	if len(r.ids) < absoluteRetireBatch {
		return 0, nil
	}
	return r.commit()
}

// stage runs one row's staging in a scratch batch and adds it to the
// retirement only if it succeeds, so a failing row never leaves half its
// deletions in the commit. delta is the row's identity count change.
func (r *absoluteRetirement) stage(id string, expired bool, mark *bool, fn func(*pebbledb.Batch) (int64, error)) error {
	r.scratch.Reset()
	delta, err := fn(r.scratch)
	if err != nil {
		return err
	}
	if err = r.b.Apply(r.scratch, nil); err != nil {
		return err
	}
	if delta != 0 {
		r.count = max(0, r.count+delta)
		r.changed = true
	}
	r.ids[id] = struct{}{}
	if expired {
		r.expired++
	}
	if mark != nil {
		r.marks = append(r.marks, mark)
	}
	return nil
}

// stageExpiredRow stages what the v0.5.12 per-row purge committed for one row
// and sets *retired once that commits. A row that is not expired is skipped.
func (r *absoluteRetirement) stageExpiredRow(task *domain.Task, retired *bool) error {
	d := r.d
	if err := d.checkTaskExpiry(task); !errors.Is(err, domain.ErrTaskExpired) {
		return err
	}
	if _, dup := r.ids[task.ID]; dup {
		if retired != nil {
			r.marks = append(r.marks, retired)
		}
		return nil
	}
	identity, err := d.readAbsoluteIdentity(task.ID)
	if err != nil {
		return err
	}
	restored := identity == nil
	if restored {
		identity = &absoluteIdentity{ID: task.ID, TenantID: task.TenantID, TopicID: taskTopicID(task), Hash: taskContentHash(task.Command, task.Payload), CreatedAt: task.CreatedAt, ExpiresAt: *task.ExpiresAt}
	}
	now := d.expiryTime()
	return r.stage(task.ID, true, retired, func(b *pebbledb.Batch) (int64, error) {
		delta := int64(0)
		if restored {
			// Cleanup must still retire an accepted/restored row when its
			// identity manifest is missing. Account it even above the
			// operational budget; new acceptance is refused at capacity,
			// retirement is never blocked.
			delta = 1
			if err := b.Set(absoluteTopicKey(identity.TopicID, task.ID), nil, nil); err != nil {
				return 0, err
			}
		}
		return delta, d.stageAbsolutePurge(b, identity, task, now)
	})
}

// commit replicates the staged rows in one write and starts an empty batch
// for the next ones. It reports how many staged rows had a task row.
func (r *absoluteRetirement) commit() (int, error) {
	if len(r.ids) == 0 {
		return 0, nil
	}
	err := r.replicate()
	ids, expired, marks := r.ids, r.expired, r.marks
	r.ids, r.expired, r.changed, r.marks = map[string]struct{}{}, 0, false, nil
	// The next rows adjust the stored count, which this commit (or another
	// leader's write, after a failure) may have changed; reread it.
	countErr := r.rereadCount()
	if err != nil {
		return 0, err
	}
	for _, mark := range marks {
		*mark = true
	}
	for id := range ids {
		r.d.Leases.Delete(id)
	}
	r.d.observeAbsoluteCount()
	metrics.AbsoluteExpiredPurged.Add(float64(expired))
	return expired, countErr
}

// replicate commits the staged rows, with the running count when it changed,
// within the leadership term the retirement started in, and starts a fresh
// batch: a batch whose bytes a replicator may still reference is never reused.
func (r *absoluteRetirement) replicate() error {
	var err error
	if r.changed {
		err = r.b.Set([]byte(absoluteCountKey), []byte(strconv.FormatInt(r.count, 10)), nil)
	}
	if err == nil {
		if r.d.leadershipEpoch() == r.epoch {
			err = r.d.commitBatchRaw(r.b)
		} else if err = r.d.RequireWriteLeader(); err == nil {
			err = errAbsoluteRetirementTermChanged
		}
	}
	_ = r.b.Close()
	r.b = r.d.Batch()
	return err
}

func (r *absoluteRetirement) rereadCount() error {
	count, err := r.d.absoluteCount()
	if err == nil {
		r.count = count
	}
	return err
}

// joinErrors is errors.Join that returns a single non-nil error unwrapped, so
// callers matching a concrete type (NotLeaderError) or comparing a context
// error keep seeing it as before.
func joinErrors(errs ...error) error {
	var kept []error
	for _, err := range errs {
		if err != nil {
			kept = append(kept, err)
		}
	}
	switch len(kept) {
	case 0:
		return nil
	case 1:
		return kept[0]
	}
	return errors.Join(kept...)
}
