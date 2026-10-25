package diskguard

import (
	"errors"
	"math"
	"sync"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/osvaldoandrade/codeq/pkg/domain"
)

// gauge reads an unlabeled gauge from the default registry.
func gauge(t *testing.T, name string) float64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range families {
		if mf.GetName() == name && len(mf.GetMetric()) == 1 {
			return mf.GetMetric()[0].GetGauge().GetValue()
		}
	}
	t.Fatalf("gauge %s not found", name)
	return 0
}

const gib = 1 << 30

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestEffectiveThresholdFormula(t *testing.T) {
	cases := []struct {
		name    string
		maxUsed float64
		total   uint64
		fp      SnapshotFootprint
		reserve uint64
		want    float64
	}{
		{"no estimate keeps configured", 0.8, gib, SnapshotFootprint{Retain: 2}, 51 * mib, 0.8},
		{"unknown volume keeps configured", 0.8, 0, SnapshotFootprint{EstimateBytes: mib, Retain: 2}, 0, 0.8},
		{"small snapshot keeps configured", 0.8, gib, SnapshotFootprint{EstimateBytes: 10 * mib, Retain: 2}, 51 * mib, 0.8},
		// No snapshot on disk yet: exactly 1 - (E*(R+1) + reserve)/total.
		{"first snapshot retain 2", 0.8, gib, SnapshotFootprint{EstimateBytes: 100 * mib, Retain: 2}, 51 * mib, 1 - float64(351*mib)/gib},
		{"first snapshot retain 1", 0.8, gib, SnapshotFootprint{EstimateBytes: 100 * mib, Retain: 1}, 51 * mib, 1 - float64(251*mib)/gib},
		// Retained copies already count as used: credit them.
		{"steady state retain 2", 0.8, gib, SnapshotFootprint{EstimateBytes: 200 * mib, OnDiskBytes: 400 * mib, Retain: 2}, 51 * mib, 1 - float64(251*mib)/gib},
		// The credit never drops the headroom below one full new copy.
		{"large old snapshots", 0.8, gib, SnapshotFootprint{EstimateBytes: 200 * mib, OnDiskBytes: 900 * mib, Retain: 2}, 51 * mib, 1 - float64(251*mib)/gib},
		{"retain below one counts as one", 0.8, gib, SnapshotFootprint{EstimateBytes: 150 * mib}, 0, 1 - float64(300*mib)/gib},
		{"volume too small clamps to zero", 0.8, gib, SnapshotFootprint{EstimateBytes: 600 * mib, Retain: 2}, 51 * mib, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := EffectiveThreshold(tc.maxUsed, tc.total, tc.fp, tc.reserve); !near(got, tc.want) {
				t.Fatalf("EffectiveThreshold = %v, want %v", got, tc.want)
			}
		})
	}
}

// staticSizer is a settable SnapshotSizer.
type staticSizer struct {
	mu  sync.Mutex
	fp  SnapshotFootprint
	err error
	n   int
}

func (s *staticSizer) set(fp SnapshotFootprint, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fp, s.err = fp, err
}

func (s *staticSizer) size() (SnapshotFootprint, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.n++
	return s.fp, s.err
}

func TestSnapshotHeadroomClosesAdmissionBelowConfiguredThreshold(t *testing.T) {
	st := &fakeStat{}
	st.set(gib, gib-790*mib) // 77% used: below the configured 80%
	g := newTestGuard(t, Config{MaxUsedFraction: 0.8, ReserveBytes: 64 * mib, Stat: st.stat})
	if _, err := g.Refresh(); err != nil {
		t.Fatal(err)
	}
	if err := g.AdmitCreate(); err != nil {
		t.Fatalf("no sizer: admission must follow the configured threshold: %v", err)
	}
	if !near(g.EffectiveThreshold(), 0.8) {
		t.Fatalf("effective threshold without sizer = %v", g.EffectiveThreshold())
	}

	sizer := &staticSizer{}
	// One 200 MiB snapshot retained (retain=1) and no older copy: the next
	// snapshot cycle needs max(2*200-200, 200) + 51 MiB reserve = 251 MiB.
	sizer.set(SnapshotFootprint{EstimateBytes: 200 * mib, OnDiskBytes: 200 * mib, Retain: 1}, nil)
	g.SetSnapshotSizer(sizer.size)
	g.tick(nil)
	want := 1 - float64(251*mib)/gib
	if !near(g.EffectiveThreshold(), want) {
		t.Fatalf("effective threshold = %v, want %v", g.EffectiveThreshold(), want)
	}
	if got := gauge(t, "codeq_storage_admission_effective_threshold_ratio"); !near(got, want) {
		t.Fatalf("effective threshold metric = %v, want %v", got, want)
	}
	if got := gauge(t, "codeq_storage_snapshot_estimate_bytes"); got != 200*mib {
		t.Fatalf("snapshot estimate metric = %v", got)
	}
	if got := gauge(t, "codeq_storage_snapshot_disk_bytes"); got != 200*mib {
		t.Fatalf("snapshot disk metric = %v", got)
	}
	if got := gauge(t, "codeq_storage_admission_threshold_ratio"); got != 0.8 {
		t.Fatalf("configured threshold metric changed: %v", got)
	}
	if err := g.AdmitCreate(); !errors.Is(err, domain.ErrStorageCapacity) {
		t.Fatalf("snapshot headroom must refuse new tasks at 77%% used: %v", err)
	}

	// A sizer failure keeps the last estimate: admission stays closed.
	sizer.set(SnapshotFootprint{}, errors.New("read snapshot dir: boom"))
	g.tick(nil)
	if !near(g.EffectiveThreshold(), want) || !g.Blocked() {
		t.Fatalf("sizer failure reopened admission: eff=%v blocked=%v", g.EffectiveThreshold(), g.Blocked())
	}

	// The queue drained and a smaller snapshot replaced the old one.
	sizer.set(SnapshotFootprint{EstimateBytes: 20 * mib, OnDiskBytes: 20 * mib, Retain: 1}, nil)
	st.set(gib, gib-500*mib)
	g.tick(nil)
	if err := g.AdmitCreate(); err != nil {
		t.Fatalf("admission must reopen once the snapshot shrinks: %v (eff=%v)", err, g.EffectiveThreshold())
	}
	if !near(g.EffectiveThreshold(), 0.8) {
		t.Fatalf("small snapshot must restore the configured threshold: %v", g.EffectiveThreshold())
	}
}

func TestSnapshotHeadroomCreditAvoidsDrainedDeadlock(t *testing.T) {
	// Two retained 230 MiB snapshots fill 460 MiB of a 1 GiB volume. If the
	// retained copies were counted again, the threshold would be
	// 1 - (3*230+51)/1024 = 27.6%, below the 50% the snapshots and reserve
	// alone occupy, so admission could never reopen without a new snapshot.
	st := &fakeStat{}
	st.set(gib, gib-560*mib) // drained: snapshots + reserve + 49 MiB Pebble
	g := newTestGuard(t, Config{MaxUsedFraction: 0.8, ReserveBytes: 64 * mib, Stat: st.stat})
	sizer := &staticSizer{}
	sizer.set(SnapshotFootprint{EstimateBytes: 230 * mib, OnDiskBytes: 460 * mib, Retain: 2}, nil)
	g.SetSnapshotSizer(sizer.size)
	if err := g.RefreshSnapshotFootprint(); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Refresh(); err != nil {
		t.Fatal(err)
	}
	if err := g.AdmitCreate(); err != nil {
		t.Fatalf("drained queue must admit: %v (eff=%v)", err, g.EffectiveThreshold())
	}
	// Peak at the next snapshot stays below the volume with the reserve free.
	peak := float64(560*mib + 230*mib + 51*mib)
	if peak >= gib {
		t.Fatalf("test premise: peak %v exceeds the volume", peak)
	}
}

func TestSnapshotHeadroomKeepsReserveFromClosingAdmission(t *testing.T) {
	st := &fakeStat{}
	// 40% used; the snapshot term lowers the threshold to 1-(400+51)/1024 = 56%.
	st.set(gib, gib-410*mib)
	dir := t.TempDir()
	g := newTestGuard(t, Config{Path: dir, MaxUsedFraction: 0.8, ReserveBytes: 64 * mib, Stat: st.stat})
	sizer := &staticSizer{}
	sizer.set(SnapshotFootprint{EstimateBytes: 200 * mib, Retain: 1}, nil)
	g.SetSnapshotSizer(sizer.size)
	if err := g.RefreshSnapshotFootprint(); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Refresh(); err != nil {
		t.Fatal(err)
	}
	// 410 + 51 = 461 MiB < 56% of 1 GiB (573 MiB): the reserve may be created.
	if err := g.ReconcileReserve(); err != nil {
		t.Fatal(err)
	}
	if g.ReserveBytes() != 51*mib {
		t.Fatalf("reserve = %d, want 51 MiB", g.ReserveBytes())
	}
	_ = g.releaseReserve("test")

	// At 530 MiB used the reserve would cross the effective (not the
	// configured) threshold, so it is not created.
	st.set(gib, gib-530*mib)
	if err := g.ReconcileReserve(); err != nil {
		t.Fatal(err)
	}
	if g.ReserveBytes() != 0 {
		t.Fatalf("reserve created across the effective threshold: %d", g.ReserveBytes())
	}
}

func TestSetSnapshotSizerIgnoresNil(t *testing.T) {
	g := newTestGuard(t, Config{MaxUsedFraction: 0.8, Stat: (&fakeStat{}).stat})
	g.SetSnapshotSizer(nil)
	var nilGuard *Guard
	nilGuard.SetSnapshotSizer(func() (SnapshotFootprint, error) { return SnapshotFootprint{}, nil })
	if err := g.RefreshSnapshotFootprint(); err != nil {
		t.Fatal(err)
	}
	if (g.SnapshotFootprint() != SnapshotFootprint{}) {
		t.Fatalf("footprint without sizer = %+v", g.SnapshotFootprint())
	}
}
