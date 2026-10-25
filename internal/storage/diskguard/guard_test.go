package diskguard

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/osvaldoandrade/codeq/pkg/domain"
)

const mib = 1 << 20

// fakeStat is a settable filesystem sample.
type fakeStat struct {
	mu    sync.Mutex
	usage Usage
	err   error
	calls int
}

func (f *fakeStat) set(total, free uint64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.usage = Usage{TotalBytes: total, FreeBytes: free}
	f.err = nil
}

func (f *fakeStat) fail(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

func (f *fakeStat) stat(string) (Usage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.usage, f.err
}

// dirVolume simulates a size-limited volume: used bytes are the real bytes
// written under dir plus a fixed base, so creating or deleting the ballast
// changes the sample exactly like a real filesystem would.
type dirVolume struct {
	dir   string
	total uint64
	base  uint64
}

func (v dirVolume) stat(string) (Usage, error) {
	var used uint64
	err := filepath.WalkDir(v.dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		used += uint64(info.Size()) // #nosec G115 -- test file sizes are non-negative
		return nil
	})
	if err != nil {
		return Usage{}, err
	}
	used += v.base
	if used > v.total {
		used = v.total
	}
	return Usage{TotalBytes: v.total, FreeBytes: v.total - used}, nil
}

func newTestGuard(t *testing.T, cfg Config) *Guard {
	t.Helper()
	if cfg.Path == "" {
		cfg.Path = t.TempDir()
	}
	g, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return g
}

func TestUsedFractionMath(t *testing.T) {
	cases := []struct {
		name  string
		u     Usage
		want  float64
		known bool
	}{
		{"empty", Usage{TotalBytes: 1000, FreeBytes: 1000}, 0, true},
		{"eighty", Usage{TotalBytes: 1000, FreeBytes: 200}, 0.8, true},
		{"full", Usage{TotalBytes: 1000, FreeBytes: 0}, 1, true},
		{"free above total is clamped", Usage{TotalBytes: 1000, FreeBytes: 5000}, 0, true},
		{"unknown total", Usage{}, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.u.UsedFraction(); got < tc.want-1e-9 || got > tc.want+1e-9 {
				t.Fatalf("UsedFraction = %v, want %v", got, tc.want)
			}
			if got := tc.u.Known(); got != tc.known {
				t.Fatalf("Known = %v, want %v", got, tc.known)
			}
		})
	}
}

func TestNewRejectsInvalidThresholds(t *testing.T) {
	for _, cfg := range []Config{
		{Path: "", MaxUsedFraction: 0.8},
		{Path: t.TempDir(), MaxUsedFraction: -0.1},
		{Path: t.TempDir(), MaxUsedFraction: 1.1},
		{Path: t.TempDir(), MaxUsedFraction: 0.8, ReserveBytes: -1},
	} {
		if _, err := New(cfg); err == nil {
			t.Fatalf("New(%+v) accepted an invalid config", cfg)
		}
	}
}

func TestAdmitCreateThresholdBoundaries(t *testing.T) {
	st := &fakeStat{}
	g := newTestGuard(t, Config{MaxUsedFraction: 0.80, Stat: st.stat, Interval: time.Hour})
	const total = 1000 * mib

	st.set(total, 201*mib) // 79.9% used
	if _, err := g.Refresh(); err != nil {
		t.Fatal(err)
	}
	if err := g.AdmitCreate(); err != nil {
		t.Fatalf("below threshold refused: %v", err)
	}

	st.set(total, 200*mib) // exactly 80%
	if _, err := g.Refresh(); err != nil {
		t.Fatal(err)
	}
	if err := g.AdmitCreate(); !errors.Is(err, domain.ErrStorageCapacity) {
		t.Fatalf("at threshold admitted: %v", err)
	}
	if !g.Blocked() {
		t.Fatal("Blocked() = false at threshold")
	}

	st.set(total, 10*mib) // 99%
	if _, err := g.Refresh(); err != nil {
		t.Fatal(err)
	}
	if err := g.AdmitCreate(); !errors.Is(err, domain.ErrStorageCapacity) {
		t.Fatalf("full volume admitted: %v", err)
	}

	st.set(total, 500*mib) // cleanup freed space
	if _, err := g.Refresh(); err != nil {
		t.Fatal(err)
	}
	if err := g.AdmitCreate(); err != nil {
		t.Fatalf("recovered volume still refused: %v", err)
	}
}

func TestAdmitCreateSamplesLazilyWhenStale(t *testing.T) {
	st := &fakeStat{}
	now := time.Unix(1_700_000_000, 0)
	var mu sync.Mutex
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	g := newTestGuard(t, Config{MaxUsedFraction: 0.5, Stat: st.stat, Interval: time.Second, Now: clock})

	st.set(100*mib, 10*mib) // 90%
	if err := g.AdmitCreate(); !errors.Is(err, domain.ErrStorageCapacity) {
		t.Fatalf("first admission must sample: %v", err)
	}
	st.set(100*mib, 90*mib) // 10%
	if err := g.AdmitCreate(); !errors.Is(err, domain.ErrStorageCapacity) {
		t.Fatalf("fresh sample must be cached within the interval: %v", err)
	}
	mu.Lock()
	now = now.Add(2 * time.Second)
	mu.Unlock()
	if err := g.AdmitCreate(); err != nil {
		t.Fatalf("stale sample must be refreshed: %v", err)
	}
}

func TestStatFailureFailsOpen(t *testing.T) {
	st := &fakeStat{}
	g := newTestGuard(t, Config{MaxUsedFraction: 0.8, Stat: st.stat, Interval: time.Hour})
	st.fail(errors.New("statfs unavailable"))
	if _, err := g.Refresh(); err == nil {
		t.Fatal("Refresh hid the stat error")
	}
	if err := g.AdmitCreate(); err != nil {
		t.Fatalf("unknown usage must not block the queue: %v", err)
	}
}

func TestReserveCreatedWhenSpaceAllowsAndCountsAsUsed(t *testing.T) {
	dir := t.TempDir()
	vol := dirVolume{dir: dir, total: 100 * mib, base: 10 * mib}
	g := newTestGuard(t, Config{Path: dir, MaxUsedFraction: 0.8, ReserveBytes: 4 * mib, Stat: vol.stat, Interval: time.Hour})

	if err := g.ReconcileReserve(); err != nil {
		t.Fatalf("ReconcileReserve: %v", err)
	}
	info, err := os.Stat(filepath.Join(dir, ReserveFileName))
	if err != nil {
		t.Fatalf("reserve not created: %v", err)
	}
	if info.Size() != 4*mib {
		t.Fatalf("reserve size = %d, want %d", info.Size(), 4*mib)
	}
	u, err := g.Refresh()
	if err != nil {
		t.Fatal(err)
	}
	if want := uint64(86 * mib); u.FreeBytes != want {
		t.Fatalf("free = %d, want %d (reserve must occupy real space)", u.FreeBytes, want)
	}
	if got := g.ReserveBytes(); got != 4*mib {
		t.Fatalf("ReserveBytes() = %d", got)
	}
	// Idempotent: a second pass keeps the single reserve.
	if err := g.ReconcileReserve(); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("unexpected files after second pass: %v", entries)
	}
}

func TestReserveCappedToVolumeFraction(t *testing.T) {
	dir := t.TempDir()
	vol := dirVolume{dir: dir, total: 40 * mib}
	g := newTestGuard(t, Config{Path: dir, MaxUsedFraction: 0.8, ReserveBytes: 64 * mib, Stat: vol.stat, Interval: time.Hour})
	if err := g.ReconcileReserve(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, ReserveFileName))
	if err != nil {
		t.Fatalf("reserve not created: %v", err)
	}
	if info.Size() != 2*mib { // 5% of 40 MiB rounded down to a MiB
		t.Fatalf("reserve size = %d, want %d", info.Size(), 2*mib)
	}
}

func TestReserveNotCreatedAboveThreshold(t *testing.T) {
	dir := t.TempDir()
	vol := dirVolume{dir: dir, total: 100 * mib, base: 78 * mib}
	g := newTestGuard(t, Config{Path: dir, MaxUsedFraction: 0.8, ReserveBytes: 4 * mib, Stat: vol.stat, Interval: time.Hour})
	if err := g.ReconcileReserve(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, ReserveFileName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("reserve created although it would cross the admission threshold: %v", err)
	}
}

func TestReserveReleasedAtRuntimeWhenCritical(t *testing.T) {
	dir := t.TempDir()
	vol := &dirVolume{dir: dir, total: 100 * mib, base: 10 * mib}
	g := newTestGuard(t, Config{Path: dir, MaxUsedFraction: 0.8, ReserveBytes: 4 * mib, Stat: func(p string) (Usage, error) { return vol.stat(p) }, Interval: time.Hour})
	if err := g.ReconcileReserve(); err != nil {
		t.Fatal(err)
	}
	vol.base = 93 * mib // replication/results filled the volume: 3 MiB free < reserve
	if err := g.ReconcileReserve(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, ReserveFileName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("reserve kept on a critically full volume: %v", err)
	}
	if g.ReserveBytes() != 0 {
		t.Fatalf("ReserveBytes() = %d after release", g.ReserveBytes())
	}
}

func TestPrepareStartupReleasesReserveOnFullDisk(t *testing.T) {
	dir := t.TempDir()
	// A previous process left the reserve and a torn temporary file behind.
	if err := os.WriteFile(filepath.Join(dir, ReserveFileName), make([]byte, 4*mib), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ReserveFileName+tmpSuffix), make([]byte, mib), 0o600); err != nil {
		t.Fatal(err)
	}
	vol := dirVolume{dir: dir, total: 100 * mib, base: 95 * mib} // 0 bytes free
	g := newTestGuard(t, Config{Path: dir, MaxUsedFraction: 0.8, ReserveBytes: 4 * mib, Stat: vol.stat, Interval: time.Hour})

	if err := g.PrepareStartup(); err != nil {
		t.Fatalf("PrepareStartup: %v", err)
	}
	for _, name := range []string{ReserveFileName, ReserveFileName + tmpSuffix} {
		if _, err := os.Stat(filepath.Join(dir, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s survived startup on a full disk: %v", name, err)
		}
	}
	u, err := g.Refresh()
	if err != nil {
		t.Fatal(err)
	}
	if u.FreeBytes != 5*mib {
		t.Fatalf("free after release = %d, want %d", u.FreeBytes, 5*mib)
	}
	if !g.Blocked() {
		t.Fatal("a 95%-used volume must still refuse new tasks after the release")
	}
}

func TestPrepareStartupKeepsReserveWithHeadroom(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ReserveFileName), make([]byte, 4*mib), 0o600); err != nil {
		t.Fatal(err)
	}
	vol := dirVolume{dir: dir, total: 100 * mib, base: 80 * mib} // 16 MiB free
	g := newTestGuard(t, Config{Path: dir, MaxUsedFraction: 0.8, ReserveBytes: 4 * mib, Stat: vol.stat, Interval: time.Hour})
	if err := g.PrepareStartup(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, ReserveFileName)); err != nil {
		t.Fatalf("reserve released without an emergency: %v", err)
	}
}

func TestDisabledReserveRemovesLeftover(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ReserveFileName), make([]byte, mib), 0o600); err != nil {
		t.Fatal(err)
	}
	vol := dirVolume{dir: dir, total: 100 * mib}
	g := newTestGuard(t, Config{Path: dir, MaxUsedFraction: 0.8, ReserveBytes: 0, Stat: vol.stat, Interval: time.Hour})
	if err := g.ReconcileReserve(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, ReserveFileName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("disabled reserve left on disk: %v", err)
	}
}

func TestRealStatfsReportsTempDir(t *testing.T) {
	u, err := Statfs(t.TempDir())
	if err != nil {
		t.Skipf("statfs unsupported here: %v", err)
	}
	if !u.Known() || u.FreeBytes > u.TotalBytes {
		t.Fatalf("implausible statfs sample: %+v", u)
	}
}

func TestRunSamplesReconcilesAndCallsHooks(t *testing.T) {
	dir := t.TempDir()
	vol := dirVolume{dir: dir, total: 100 * mib, base: 10 * mib}
	g := newTestGuard(t, Config{Path: dir, MaxUsedFraction: 0.8, ReserveBytes: 2 * mib, Stat: vol.stat, Interval: 5 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	hooks := make(chan struct{}, 16)
	done := make(chan struct{})
	go func() {
		defer close(done)
		g.Run(ctx, func() {
			select {
			case hooks <- struct{}{}:
			default:
			}
		})
	}()
	for range 2 {
		select {
		case <-hooks:
		case <-time.After(5 * time.Second):
			t.Fatal("Run did not invoke its hook")
		}
	}
	cancel()
	<-done
	if g.ReserveBytes() != 2*mib {
		t.Fatalf("Run did not create the reserve: %d", g.ReserveBytes())
	}
}
