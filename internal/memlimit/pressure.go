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
	allCPUMetric   = "/cpu/classes/total:cpu-seconds"
)

// gcBusyCPUMetrics are the GC CPU classes that take CPU from the program:
// mark assists, dedicated mark workers and stop-the-world pauses. Idle mark
// work (/cpu/classes/gc/mark/idle) only runs on otherwise idle Ps, so
// /cpu/classes/gc/total, which includes it, reads high on an idle voter.
var gcBusyCPUMetrics = []string{
	"/cpu/classes/gc/mark/assist:cpu-seconds",
	"/cpu/classes/gc/mark/dedicated:cpu-seconds",
	"/cpu/classes/gc/pause:cpu-seconds",
}

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
	// the GC took from the program since the previous sample: assists,
	// dedicated mark workers and pauses, not idle-priority marking.
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
	samples := make([]metrics.Sample, 0, 3+len(gcBusyCPUMetrics))
	samples = append(samples, metrics.Sample{Name: liveHeapMetric}, metrics.Sample{Name: goLimitMetric}, metrics.Sample{Name: allCPUMetric})
	for _, name := range gcBusyCPUMetrics {
		samples = append(samples, metrics.Sample{Name: name})
	}
	metrics.Read(samples)
	return runtimeReading{
		liveHeap: sampleUint(samples[0]),
		goLimit:  sampleUint(samples[1]),
		allCPU:   sampleFloat(samples[2]),
		gcCPU:    sumFloat(samples[3:]),
	}
}

// sumFloat adds the float samples; an unsupported metric counts as zero.
func sumFloat(samples []metrics.Sample) float64 {
	total := 0.0
	for _, s := range samples {
		total += sampleFloat(s)
	}
	return total
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
