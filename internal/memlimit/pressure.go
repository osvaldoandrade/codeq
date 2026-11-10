package memlimit

import (
	"math"
	"runtime/metrics"
	"sync"
	"time"
)

// Runtime metrics read by the pressure monitor (Go 1.21+).
const (
	liveHeapMetric = "/gc/heap/live:bytes"
	goLimitMetric  = "/gc/gomemlimit:bytes"
	gcCPUMetric    = "/cpu/classes/gc/total:cpu-seconds"
	allCPUMetric   = "/cpu/classes/total:cpu-seconds"
)

// Default thresholds of the pressure monitor. A live heap at 90% of the soft
// limit leaves the GC almost no room: past it the runtime collects back to
// back, up to its CPU cap of half of GOMAXPROCS, which on a voter limited to
// one CPU (GOMAXPROCS=2) is the whole quota (ADR 0015).
const (
	DefaultPressureHeapRatio   = 0.90
	DefaultPressureGCFraction  = 0.25
	DefaultPressureWarnEvery   = 5 * time.Minute
	defaultPressureConsecutive = 2
)

// Pressure is one sample of the Go heap against the runtime soft limit.
type Pressure struct {
	// LiveHeapBytes is the heap the last GC marked live.
	LiveHeapBytes uint64
	// GoLimitBytes is the soft limit in effect; 0 when there is none.
	GoLimitBytes uint64
	// GCCPUFraction is the share of the CPU available to Go (GOMAXPROCS)
	// spent in GC since the previous sample.
	GCCPUFraction float64
	// Over reports that the sample crossed a threshold.
	Over bool
}

// HeapRatio is LiveHeapBytes / GoLimitBytes, or 0 without a limit.
func (p Pressure) HeapRatio() float64 {
	if p.GoLimitBytes == 0 {
		return 0
	}
	return float64(p.LiveHeapBytes) / float64(p.GoLimitBytes)
}

// PressureMonitor samples the runtime on a caller's timer and decides when a
// sustained memory-pressure condition deserves a (rate-limited) warning. The
// zero value is not usable; use NewPressureMonitor.
type PressureMonitor struct {
	mu          sync.Mutex
	read        func() runtimeReading
	heapRatio   float64
	gcFraction  float64
	warnEvery   time.Duration
	consecutive int

	primed   bool
	lastGC   float64
	lastAll  float64
	over     int
	lastWarn time.Time
}

// NewPressureMonitor uses the default thresholds and the live runtime.
func NewPressureMonitor() *PressureMonitor {
	return &PressureMonitor{
		read:        readRuntime,
		heapRatio:   DefaultPressureHeapRatio,
		gcFraction:  DefaultPressureGCFraction,
		warnEvery:   DefaultPressureWarnEvery,
		consecutive: defaultPressureConsecutive,
	}
}

// runtimeReading is the raw runtime state one sample needs.
type runtimeReading struct {
	liveHeap, goLimit uint64
	gcCPU, allCPU     float64
}

func readRuntime() runtimeReading {
	samples := []metrics.Sample{{Name: liveHeapMetric}, {Name: goLimitMetric}, {Name: gcCPUMetric}, {Name: allCPUMetric}}
	metrics.Read(samples)
	return runtimeReading{
		liveHeap: sampleUint(samples[0]),
		goLimit:  sampleUint(samples[1]),
		gcCPU:    sampleFloat(samples[2]),
		allCPU:   sampleFloat(samples[3]),
	}
}

// Sample reads the runtime. Over is set when the live heap reaches the heap
// ratio of the soft limit or the GC share of CPU reaches its fraction.
func (m *PressureMonitor) Sample() Pressure {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.read()
	p := Pressure{LiveHeapBytes: r.liveHeap}
	if r.goLimit > 0 && r.goLimit < math.MaxInt64 {
		p.GoLimitBytes = r.goLimit
	}
	if m.primed && r.allCPU > m.lastAll {
		p.GCCPUFraction = (r.gcCPU - m.lastGC) / (r.allCPU - m.lastAll)
	}
	m.primed, m.lastGC, m.lastAll = true, r.gcCPU, r.allCPU
	p.Over = (p.GoLimitBytes > 0 && p.HeapRatio() >= m.heapRatio) || p.GCCPUFraction >= m.gcFraction
	return p
}

// ShouldWarn reports whether p, the latest sample, completes a sustained
// pressure condition that was not reported within the warning interval.
func (m *PressureMonitor) ShouldWarn(now time.Time, p Pressure) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !p.Over {
		m.over = 0
		return false
	}
	m.over++
	if m.over < m.consecutive || (!m.lastWarn.IsZero() && now.Sub(m.lastWarn) < m.warnEvery) {
		return false
	}
	m.lastWarn = now
	return true
}

func sampleUint(s metrics.Sample) uint64 {
	if s.Value.Kind() != metrics.KindUint64 {
		return 0
	}
	return s.Value.Uint64()
}

func sampleFloat(s metrics.Sample) float64 {
	if s.Value.Kind() != metrics.KindFloat64 {
		return 0
	}
	return s.Value.Float64()
}
