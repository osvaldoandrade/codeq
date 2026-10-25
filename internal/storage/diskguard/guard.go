// Package diskguard protects the local CodeQ data volume.
//
// A full volume is the one failure an embedded LSM cannot recover from by
// itself: Pebble cannot write the WAL, MANIFEST or a compaction output, the
// process dies and the next start fails to open the store. The guard keeps
// two independent safety margins:
//
//   - admission: at or above a used-space fraction, NEW task creation is
//     refused with domain.ErrStorageCapacity. Every path that drains or
//     shrinks the queue (claim, lease renewal, completion, nack, deletion,
//     cleanup, Raft replication and compaction) is never consulted, so the
//     queue keeps draining and space can be reclaimed.
//   - snapshot headroom: on Raft voters the admission threshold is lowered
//     so the next snapshot cycle fits (see EffectiveThreshold): a snapshot
//     is written in full before older ones are reaped.
//   - reserve: a small ballast file is written while space is plentiful and
//     deleted when the volume is critically full (at runtime, and at startup
//     before Pebble opens), giving Pebble room to open and compact without a
//     manual purge.
//
// The reserve file name never parses as a Pebble file, so Pebble ignores it
// and an older binary can still open the directory.
package diskguard

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/osvaldoandrade/codeq/internal/metrics"
	"github.com/osvaldoandrade/codeq/pkg/domain"
)

const (
	// ReserveFileName is the ballast kept in the data directory root.
	ReserveFileName = "codeq-reserve.ballast"
	tmpSuffix       = ".tmp"

	// DefaultMaxUsedFraction refuses new tasks at 80% used.
	DefaultMaxUsedFraction = 0.80
	// DefaultReserveBytes is the requested ballast size before the volume cap.
	DefaultReserveBytes = int64(64 << 20)
	// DefaultInterval bounds how stale an admission decision may be.
	DefaultInterval = 5 * time.Second

	// maxReserveFraction caps the ballast to a small share of the volume.
	maxReserveFraction = 0.05
	reserveChunk       = int64(1 << 20)
)

// Usage is one filesystem sample. FreeBytes is the space available to the
// unprivileged process (statfs f_bavail), so a root-reserved area counts as
// used: the process sees ENOSPC exactly when FreeBytes reaches zero.
type Usage struct {
	TotalBytes uint64
	FreeBytes  uint64
}

// Known reports whether the sample describes a real volume.
func (u Usage) Known() bool { return u.TotalBytes > 0 }

// UsedBytes is the space unavailable to the process.
func (u Usage) UsedBytes() uint64 {
	if u.FreeBytes >= u.TotalBytes {
		return 0
	}
	return u.TotalBytes - u.FreeBytes
}

// UsedFraction reports UsedBytes/TotalBytes, or 0 for an unknown sample.
func (u Usage) UsedFraction() float64 {
	if !u.Known() {
		return 0
	}
	return float64(u.UsedBytes()) / float64(u.TotalBytes)
}

// atOrAbove compares bytes, not rounded ratios, so 80% of a volume is
// exactly at an 0.80 threshold.
func (u Usage) atOrAbove(extra uint64, fraction float64) bool {
	return float64(u.UsedBytes()+extra) >= fraction*float64(u.TotalBytes)
}

// SnapshotFootprint is the Raft snapshot space on the guarded volume,
// summed over every shard that keeps snapshots there.
type SnapshotFootprint struct {
	// EstimateBytes is the expected size of the next snapshot: the newest
	// snapshot on disk, or the live replicated state when larger or when
	// no snapshot exists yet. Zero disables snapshot headroom.
	EstimateBytes uint64
	// OnDiskBytes is what snapshots occupy now (already in UsedBytes).
	OnDiskBytes uint64
	// Retain is how many snapshots survive each new one (raft.snapshotRetain).
	Retain int
}

// SnapshotSizer measures the snapshot footprint. It runs only on the
// monitor goroutine, never on the request path.
type SnapshotSizer func() (SnapshotFootprint, error)

// EffectiveThreshold is the used fraction at or above which new tasks are
// refused:
//
//	min(maxUsed, 1 - (max(E*(R+1) - D, E) + reserve) / total)
//
// with E the snapshot estimate, R the retention and D the snapshot bytes
// already on disk. Raft writes a new snapshot completely before reaping
// old ones, so the snapshot footprint peaks at R+1 copies. D is credited
// because those bytes are already counted as used (without the credit the
// retained copies would be counted twice and a drained queue could keep
// admission closed until the next snapshot), but never below one full new
// copy E. reserve keeps that many bytes free at the peak. The result is
// clamped to [0, maxUsed]; 0 means the volume cannot hold the snapshot
// footprint and every new task is refused until it shrinks. Without a
// snapshot estimate (no Raft, empty state) it is maxUsed.
func EffectiveThreshold(maxUsed float64, total uint64, fp SnapshotFootprint, reserve uint64) float64 {
	if total == 0 || fp.EstimateBytes == 0 {
		return maxUsed
	}
	retain := uint64(max(fp.Retain, 1)) // #nosec G115 -- clamped positive
	est := float64(fp.EstimateBytes)
	snap := max(est*float64(retain+1)-float64(fp.OnDiskBytes), est)
	eff := 1 - (snap+float64(reserve))/float64(total)
	return min(max(eff, 0), maxUsed)
}

// StatFunc samples the filesystem holding path.
type StatFunc func(path string) (Usage, error)

// Config configures a Guard. Zero values select the defaults above, except
// ReserveBytes where zero disables the reserve.
type Config struct {
	// Path is the data directory root (the Pebble path, or the parent of
	// its shard directories). The reserve lives directly inside it.
	Path string
	// MaxUsedFraction in (0, 1]: refuse new tasks at or above it.
	MaxUsedFraction float64
	// ReserveBytes requested ballast; capped to 5% of the volume and
	// rounded down to whole MiB. Zero disables (and removes) the reserve.
	ReserveBytes int64
	// Interval is the sampling period and the staleness bound.
	Interval time.Duration
	Stat     StatFunc
	Logger   *slog.Logger
	Now      func() time.Time
}

// Guard is the data-volume admission guard. It is safe for concurrent use.
type Guard struct {
	path          string
	maxUsed       float64
	reserveTarget int64
	interval      time.Duration
	stat          StatFunc
	logger        *slog.Logger
	now           func() time.Time

	blocked     atomic.Bool
	effective   atomic.Uint64 // math.Float64bits of the effective threshold
	sizer       atomic.Pointer[SnapshotSizer]
	footprint   atomic.Pointer[SnapshotFootprint]
	lastSample  atomic.Int64 // unix nanos of the last sample attempt; 0 = never
	reserveHeld atomic.Int64
	reserveMu   sync.Mutex
}

// New validates cfg and builds a Guard. It performs no I/O.
func New(cfg Config) (*Guard, error) {
	if cfg.Path == "" {
		return nil, errors.New("diskguard: data path is required")
	}
	if cfg.MaxUsedFraction < 0 || cfg.MaxUsedFraction > 1 {
		return nil, fmt.Errorf("diskguard: max used fraction %v must be within (0, 1]", cfg.MaxUsedFraction)
	}
	if cfg.ReserveBytes < 0 {
		return nil, fmt.Errorf("diskguard: reserve bytes %d must not be negative", cfg.ReserveBytes)
	}
	g := &Guard{
		path:          cfg.Path,
		maxUsed:       cfg.MaxUsedFraction,
		reserveTarget: cfg.ReserveBytes,
		interval:      cfg.Interval,
		stat:          cfg.Stat,
		logger:        cfg.Logger,
		now:           cfg.Now,
	}
	if g.maxUsed == 0 {
		g.maxUsed = DefaultMaxUsedFraction
	}
	if g.interval <= 0 {
		g.interval = DefaultInterval
	}
	if g.stat == nil {
		g.stat = Statfs
	}
	if g.logger == nil {
		g.logger = slog.Default()
	}
	if g.now == nil {
		g.now = time.Now
	}
	metrics.StorageAdmissionThresholdRatio.Set(g.maxUsed)
	g.setEffective(g.maxUsed)
	return g, nil
}

// SetSnapshotSizer installs the Raft snapshot footprint source. The
// monitor (Run) samples it before each volume sample; until the first
// sample the effective threshold equals the configured one.
func (g *Guard) SetSnapshotSizer(fn SnapshotSizer) {
	if g == nil || fn == nil {
		return
	}
	g.sizer.Store(&fn)
}

// RefreshSnapshotFootprint samples the installed sizer. A failure keeps
// the last known footprint (a transient read error must not reopen
// admission that snapshot headroom closed).
func (g *Guard) RefreshSnapshotFootprint() error {
	fn := g.sizer.Load()
	if fn == nil {
		return nil
	}
	fp, err := (*fn)()
	if err != nil {
		metrics.StorageStatErrors.Inc()
		return fmt.Errorf("diskguard: snapshot footprint: %w", err)
	}
	g.footprint.Store(&fp)
	metrics.StorageSnapshotEstimateBytes.Set(float64(fp.EstimateBytes))
	metrics.StorageSnapshotDiskBytes.Set(float64(fp.OnDiskBytes))
	return nil
}

// SnapshotFootprint reports the last sampled footprint.
func (g *Guard) SnapshotFootprint() SnapshotFootprint {
	if fp := g.footprint.Load(); fp != nil {
		return *fp
	}
	return SnapshotFootprint{}
}

// EffectiveThreshold reports the threshold applied by the last sample.
func (g *Guard) EffectiveThreshold() float64 {
	return math.Float64frombits(g.effective.Load())
}

func (g *Guard) setEffective(v float64) {
	g.effective.Store(math.Float64bits(v))
	metrics.StorageAdmissionEffectiveThresholdRatio.Set(v)
}

// thresholdFor computes and publishes the effective threshold for u.
func (g *Guard) thresholdFor(u Usage) float64 {
	reserve := uint64(max(g.reserveSize(u), 0)) // #nosec G115 -- clamped non-negative
	eff := EffectiveThreshold(g.maxUsed, u.TotalBytes, g.SnapshotFootprint(), reserve)
	g.setEffective(eff)
	return eff
}

// Refresh samples the volume, updates the admission state and metrics.
// A failed sample leaves admission open: an unknown volume must not stop
// the queue, and the error is counted and returned to the caller.
func (g *Guard) Refresh() (Usage, error) {
	g.lastSample.Store(g.now().UnixNano())
	u, err := g.stat(g.path)
	if err == nil && !u.Known() {
		err = errors.New("filesystem reported zero size")
	}
	if err != nil {
		metrics.StorageStatErrors.Inc()
		g.setBlocked(false, u)
		return u, fmt.Errorf("diskguard: sample %s: %w", g.path, err)
	}
	metrics.StorageDataDirTotalBytes.Set(float64(u.TotalBytes))
	metrics.StorageDataDirFreeBytes.Set(float64(u.FreeBytes))
	metrics.StorageDataDirUsedRatio.Set(u.UsedFraction())
	g.setBlocked(u.atOrAbove(0, g.thresholdFor(u)), u)
	return u, nil
}

func (g *Guard) setBlocked(blocked bool, u Usage) {
	if blocked {
		metrics.StorageAdmissionBlocked.Set(1)
	} else {
		metrics.StorageAdmissionBlocked.Set(0)
	}
	if g.blocked.Swap(blocked) == blocked {
		return
	}
	eff := g.EffectiveThreshold()
	if blocked {
		fp := g.SnapshotFootprint()
		reason := "used_fraction"
		if eff < g.maxUsed {
			reason = "snapshot_headroom"
		}
		g.logger.Warn("storage admission closed: new tasks refused until space is reclaimed",
			"code", domain.StorageCapacityCode, "reason", reason, "usedRatio", u.UsedFraction(),
			"threshold", eff, "configuredThreshold", g.maxUsed,
			"snapshotEstimateBytes", fp.EstimateBytes, "snapshotDiskBytes", fp.OnDiskBytes, "snapshotRetain", fp.Retain,
			"freeBytes", u.FreeBytes, "totalBytes", u.TotalBytes)
		return
	}
	g.logger.Info("storage admission open", "usedRatio", u.UsedFraction(), "threshold", eff, "configuredThreshold", g.maxUsed)
}

// AdmitCreate returns domain.ErrStorageCapacity while the volume is at or
// above the threshold. It samples lazily when the last sample is older than
// the interval, so it stays correct even without Run.
func (g *Guard) AdmitCreate() error {
	if g == nil {
		return nil
	}
	g.refreshIfStale()
	if !g.blocked.Load() {
		return nil
	}
	metrics.StorageAdmissionRefused.Inc()
	return domain.ErrStorageCapacity
}

func (g *Guard) refreshIfStale() {
	last := g.lastSample.Load()
	now := g.now().UnixNano()
	if last != 0 && now-last < int64(g.interval) {
		return
	}
	// One caller samples; concurrent callers use the current state.
	if g.lastSample.CompareAndSwap(last, now) {
		_, _ = g.Refresh() // counted and fail-open inside Refresh
	}
}

// Blocked reports whether new task creation is currently refused.
func (g *Guard) Blocked() bool { return g != nil && g.blocked.Load() }

// ReserveBytes reports the ballast currently held on disk.
func (g *Guard) ReserveBytes() int64 { return g.reserveHeld.Load() }

// PrepareStartup runs before the store opens. It removes a torn temporary
// reserve and, when the volume is critically full, releases the reserve so
// Pebble can replay its WAL, flush and compact. It never creates a reserve:
// that is Run's job once the process is serving.
func (g *Guard) PrepareStartup() error {
	if err := removeIfExists(g.reservePath() + tmpSuffix); err != nil {
		return err
	}
	if err := g.reconcile(false); err != nil {
		return err
	}
	_, err := g.Refresh()
	return err
}

// ReconcileReserve creates the reserve when space allows and releases it
// when the volume is critically full. It is idempotent.
func (g *Guard) ReconcileReserve() error { return g.reconcile(true) }

func (g *Guard) reconcile(allowCreate bool) error {
	g.reserveMu.Lock()
	defer g.reserveMu.Unlock()
	held, err := g.heldReserve()
	if err != nil {
		return err
	}
	g.reserveHeld.Store(held)
	metrics.StorageReserveBytes.Set(float64(held))
	if g.reserveTarget == 0 {
		if held > 0 {
			return g.releaseReserve("reserve disabled")
		}
		return nil
	}
	u, err := g.Refresh()
	if err != nil {
		return err
	}
	if held > 0 {
		// Critical: less free space than the reserve itself.
		if u.FreeBytes < uint64(held) {
			metrics.StorageReserveReleased.Inc()
			return g.releaseReserve("volume critically full")
		}
		return nil
	}
	if allowCreate {
		return g.maybeCreateReserve(u)
	}
	return nil
}

func (g *Guard) maybeCreateReserve(u Usage) error {
	size := g.reserveSize(u)
	if size <= 0 {
		return nil
	}
	need := uint64(size)
	// Never let the reserve itself close admission, and keep at least one
	// more reserve worth of free space after writing it.
	if u.atOrAbove(need, g.EffectiveThreshold()) || u.FreeBytes < 2*need {
		return nil
	}
	if err := g.writeReserve(size); err != nil {
		return err
	}
	g.reserveHeld.Store(size)
	metrics.StorageReserveBytes.Set(float64(size))
	g.logger.Info("storage reserve created", "bytes", size)
	_, err := g.Refresh()
	return err
}

// reserveSize caps the request to 5% of the volume, in whole MiB.
func (g *Guard) reserveSize(u Usage) int64 {
	if g.reserveTarget <= 0 || !u.Known() {
		return 0
	}
	size := g.reserveTarget
	if limit := int64(float64(u.TotalBytes) * maxReserveFraction); limit < size {
		size = limit
	}
	return size / reserveChunk * reserveChunk
}

func (g *Guard) reservePath() string { return filepath.Join(g.path, ReserveFileName) }

func (g *Guard) heldReserve() (int64, error) {
	info, err := os.Lstat(g.reservePath())
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("diskguard: stat reserve: %w", err)
	}
	if !info.Mode().IsRegular() {
		return 0, fmt.Errorf("diskguard: reserve %s is not a regular file", g.reservePath())
	}
	return info.Size(), nil
}

func (g *Guard) releaseReserve(reason string) error {
	if err := removeIfExists(g.reservePath()); err != nil {
		return err
	}
	syncDir(g.path)
	g.reserveHeld.Store(0)
	metrics.StorageReserveBytes.Set(0)
	g.logger.Warn("storage reserve released", "reason", reason)
	return nil
}

// writeReserve writes real zero blocks (a sparse file would reserve
// nothing), syncs, then renames into place so a crash never leaves a
// half-sized reserve under the final name.
func (g *Guard) writeReserve(size int64) (err error) {
	tmp := g.reservePath() + tmpSuffix
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600) // #nosec G304 -- fixed name under the configured data dir
	if err != nil {
		return fmt.Errorf("diskguard: create reserve: %w", err)
	}
	defer func() {
		if err != nil {
			_ = f.Close()
			_ = os.Remove(tmp)
		}
	}()
	chunk := make([]byte, reserveChunk)
	for written := int64(0); written < size; written += reserveChunk {
		if _, err = f.Write(chunk); err != nil {
			return fmt.Errorf("diskguard: write reserve: %w", err)
		}
	}
	if err = f.Sync(); err != nil {
		return fmt.Errorf("diskguard: sync reserve: %w", err)
	}
	if err = f.Close(); err != nil {
		return fmt.Errorf("diskguard: close reserve: %w", err)
	}
	if err = os.Rename(tmp, g.reservePath()); err != nil {
		return fmt.Errorf("diskguard: publish reserve: %w", err)
	}
	syncDir(g.path)
	return nil
}

// Run samples every interval until ctx ends: it refreshes the snapshot
// footprint and admission, reconciles the reserve and invokes hooks
// (engine metrics). The first pass runs immediately.
func (g *Guard) Run(ctx context.Context, hooks ...func()) {
	ticker := time.NewTicker(g.interval)
	defer ticker.Stop()
	for {
		g.tick(hooks)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (g *Guard) tick(hooks []func()) {
	if err := g.RefreshSnapshotFootprint(); err != nil {
		g.logger.Warn("snapshot footprint sample failed; keeping the last estimate", "err", err)
	}
	if _, err := g.Refresh(); err != nil {
		g.logger.Warn("storage sample failed; admission stays open", "err", err)
	}
	if err := g.ReconcileReserve(); err != nil {
		g.logger.Warn("storage reserve reconcile failed", "err", err)
	}
	for _, hook := range hooks {
		hook()
	}
}

func removeIfExists(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("diskguard: remove %s: %w", filepath.Base(path), err)
	}
	return nil
}

// syncDir makes a create/rename/remove durable; best effort.
func syncDir(dir string) {
	d, err := os.Open(dir) // #nosec G304 -- the configured data directory
	if err != nil {
		return
	}
	_ = d.Sync()
	_ = d.Close()
}
