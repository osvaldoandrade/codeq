package memlimit

import (
	"math"
	"runtime"
	"testing"
	"time"
)

// fakeRuntime feeds the monitor deterministic runtime readings.
type fakeRuntime struct {
	live, limit uint64
	gc, all     float64
}

func newFakeMonitor(f *fakeRuntime) *PressureMonitor {
	m := NewPressureMonitor()
	m.read = func() runtimeReading {
		return runtimeReading{liveHeap: f.live, goLimit: f.limit, gcCPU: f.gc, allCPU: f.all}
	}
	return m
}

func TestPressureMonitorFlagsHeapAtSoftLimit(t *testing.T) {
	f := &fakeRuntime{live: 100 << 20, limit: 358 << 20, all: 10}
	m := newFakeMonitor(f)
	if p := m.Sample(); p.Over || p.GoLimitBytes != 358<<20 {
		t.Fatalf("healthy heap flagged: %+v", p)
	}
	f.live, f.all = 340<<20, 20
	p := m.Sample()
	if !p.Over || p.HeapRatio() < 0.9 {
		t.Fatalf("heap at 95%% of the soft limit not flagged: %+v ratio=%.2f", p, p.HeapRatio())
	}
}

func TestPressureMonitorFlagsGCShare(t *testing.T) {
	f := &fakeRuntime{live: 50 << 20, limit: math.MaxInt64, gc: 1, all: 10}
	m := newFakeMonitor(f)
	if p := m.Sample(); p.Over || p.GoLimitBytes != 0 || p.GCCPUFraction != 0 {
		t.Fatalf("first sample has no window: %+v", p)
	}
	f.gc, f.all = 5.5, 20 // 4.5 of 10 CPU-seconds in GC
	if p := m.Sample(); !p.Over || p.GCCPUFraction < 0.44 || p.GCCPUFraction > 0.46 {
		t.Fatalf("GC at 45%% of CPU not flagged: %+v", p)
	}
}

func TestPressureMonitorWarnsWhenSustainedAndRateLimited(t *testing.T) {
	m := NewPressureMonitor()
	over := Pressure{Over: true}
	now := time.Unix(1_000_000, 0)
	if m.ShouldWarn(now, over) {
		t.Fatal("a single over-threshold sample warned")
	}
	if !m.ShouldWarn(now.Add(5*time.Second), over) {
		t.Fatal("sustained pressure did not warn")
	}
	if m.ShouldWarn(now.Add(10*time.Second), over) {
		t.Fatal("warning not rate limited")
	}
	if m.ShouldWarn(now.Add(15*time.Second), Pressure{}) {
		t.Fatal("a healthy sample warned")
	}
	if m.ShouldWarn(now.Add(DefaultPressureWarnEvery+20*time.Second), over) {
		t.Fatal("pressure must be sustained again after recovering")
	}
	if !m.ShouldWarn(now.Add(DefaultPressureWarnEvery+25*time.Second), over) {
		t.Fatal("sustained pressure after the interval did not warn")
	}
}

func TestPressureMonitorReadsLiveRuntime(t *testing.T) {
	m := NewPressureMonitor()
	_ = m.Sample()
	// The live heap is what the last GC marked.
	runtime.GC()
	if p := m.Sample(); p.LiveHeapBytes == 0 || p.GCCPUFraction < 0 || p.GCCPUFraction > 1 {
		t.Fatalf("live runtime sample implausible: %+v", p)
	}
}
