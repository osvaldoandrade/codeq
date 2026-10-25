// Package memlimit sizes CodeQ's memory consumers from the container limit.
//
// Pebble's block cache and memtables were fixed (256 MiB + 64 MiB per
// shard), which alone exceeds a 512 MiB container limit once the Go heap is
// added, so voters were OOMKilled under load. The server now reads the
// cgroup memory limit, derives conservative per-shard sizes from it and sets
// the Go runtime soft limit (GOMEMLIMIT) below it. CodeQ builds with
// CGO_ENABLED=0, so Pebble's manual allocations live on the Go heap and are
// governed by that soft limit too. Explicit overrides always win.
package memlimit

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"

	"github.com/osvaldoandrade/codeq/internal/metrics"
)

const (
	// DefaultGoMemLimitFraction keeps the Go soft limit at 70% of the
	// container limit, leaving headroom for stacks, OS buffers and spikes.
	DefaultGoMemLimitFraction = 0.70

	mib = int64(1 << 20)
	// The block cache gets 15% of the limit, within [8 MiB, 256 MiB].
	cacheFraction = 0.15
	minCacheBytes = 8 * mib
	maxCacheBytes = 256 * mib
	// One memtable gets 1/32 of the limit, within [4 MiB, 64 MiB]. Pebble
	// may hold up to two queued memtables plus the mutable one.
	memTableDivisor  = 32
	minMemTableBytes = 4 * mib
	maxMemTableBytes = 64 * mib
	// cgroup v1 reports "unlimited" as a huge page-aligned value.
	unlimitedV1 = int64(1) << 62

	goMemLimitEnv = "GOMEMLIMIT"
)

// Sources reported by Resolve and ApplyGoMemLimit.
const (
	SourceConfig  = "config"      // explicit memory.limitBytes
	SourceCgroup  = "cgroup"      // cgroup v2/v1 limit file
	SourceNone    = "none"        // no limit known / none applied
	SourceEnv     = goMemLimitEnv // explicit GOMEMLIMIT honored
	SourceDerived = "derived"     // fraction × container limit
)

// Source locates the cgroup filesystem. Zero values select the real paths.
type Source struct {
	Root       string // default /sys/fs/cgroup
	SelfCgroup string // default /proc/self/cgroup
}

func (s Source) root() string {
	if s.Root == "" {
		return "/sys/fs/cgroup"
	}
	return s.Root
}

func (s Source) selfCgroup() string {
	if s.SelfCgroup == "" {
		return "/proc/self/cgroup"
	}
	return s.SelfCgroup
}

// Limit returns the memory limit of this process's cgroup. ok is false
// when there is no limit ("max"/unlimited) or no cgroup filesystem.
// cgroup v2 is tried first, then v1; the process's own cgroup path from
// /proc/self/cgroup is preferred over the namespace root.
func (s Source) Limit() (limit int64, ok bool, err error) {
	v2, v1 := s.selfPaths()
	for _, p := range candidates(s.root(), v2, "memory.max") {
		if limit, ok, found, err := readLimit(p); found || err != nil {
			return limit, ok, err
		}
	}
	for _, p := range candidates(filepath.Join(s.root(), "memory"), v1, "memory.limit_in_bytes") {
		if limit, ok, found, err := readLimit(p); found || err != nil {
			return limit, ok, err
		}
	}
	return 0, false, nil
}

// candidates lists the process-specific file first, then the root one.
func candidates(base, self, name string) []string {
	out := make([]string, 0, 2)
	if self != "" && self != "/" {
		out = append(out, filepath.Join(base, filepath.Clean(self), name))
	}
	return append(out, filepath.Join(base, name))
}

// selfPaths parses /proc/self/cgroup: "0::/path" (v2) and
// "N:...memory...:/path" (v1). A missing file yields empty paths.
func (s Source) selfPaths() (v2, v1 string) {
	data, err := os.ReadFile(s.selfCgroup())
	if err != nil {
		return "", ""
	}
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		parts := strings.SplitN(sc.Text(), ":", 3)
		if len(parts) != 3 {
			continue
		}
		if parts[0] == "0" && parts[1] == "" {
			v2 = parts[2]
			continue
		}
		for _, ctrl := range strings.Split(parts[1], ",") {
			if ctrl == "memory" {
				v1 = parts[2]
			}
		}
	}
	return v2, v1
}

// readLimit reads one limit file. found is false when it does not exist.
func readLimit(path string) (limit int64, ok, found bool, err error) {
	data, err := os.ReadFile(path) // #nosec G304 -- fixed cgroup file names under the cgroup root
	if errors.Is(err, fs.ErrNotExist) {
		return 0, false, false, nil
	}
	if err != nil {
		return 0, false, true, fmt.Errorf("memlimit: read %s: %w", path, err)
	}
	limit, ok, err = ParseLimit(string(data))
	return limit, ok, true, err
}

// ParseLimit parses a cgroup memory limit value: "max" (v2) or a byte
// count. Values at or above the v1 "unlimited" sentinel are unlimited.
func ParseLimit(raw string) (limit int64, ok bool, err error) {
	v := strings.TrimSpace(raw)
	if v == "max" {
		return 0, false, nil
	}
	n, err := strconv.ParseUint(v, 10, 64)
	if err != nil {
		return 0, false, fmt.Errorf("memlimit: invalid cgroup limit %q", v)
	}
	if n == 0 || n >= uint64(unlimitedV1) {
		return 0, false, nil
	}
	return int64(n), true, nil // #nosec G115 -- n < 1<<62 checked above
}

// Resolve picks the effective container limit: an explicit positive
// override, else the cgroup limit. source names where it came from.
func Resolve(explicit int64, src Source) (limit int64, source string, err error) {
	if explicit > 0 {
		return explicit, SourceConfig, nil
	}
	limit, ok, err := src.Limit()
	if err != nil {
		return 0, SourceNone, err
	}
	if !ok {
		return 0, SourceNone, nil
	}
	return limit, SourceCgroup, nil
}

// Overrides are explicit per-shard Pebble sizes; zero derives them.
type Overrides struct {
	CacheBytes    int64
	MemTableBytes int64
}

// Plan is the per-shard Pebble sizing for one process.
type Plan struct {
	LimitBytes            int64 // 0 = unlimited/unknown
	CacheBytesPerShard    int64
	MemTableBytesPerShard uint64
}

// Derive sizes each Pebble shard from the container limit. Without a limit
// it keeps the legacy fixed sizes so unconstrained hosts and benchmarks are
// unchanged. With a limit the totals are split across shards; per-shard
// floors keep a shard functional. Explicit overrides are per shard.
func Derive(limit int64, shards int, o Overrides) Plan {
	if shards < 1 {
		shards = 1
	}
	p := Plan{LimitBytes: max(limit, 0), CacheBytesPerShard: maxCacheBytes, MemTableBytesPerShard: uint64(maxMemTableBytes)}
	if limit > 0 {
		cacheTotal := clamp(int64(float64(limit)*cacheFraction), minCacheBytes, maxCacheBytes)
		memTotal := clamp(limit/memTableDivisor, minMemTableBytes, maxMemTableBytes)
		p.CacheBytesPerShard = roundMiB(max(cacheTotal/int64(shards), minCacheBytes))
		p.MemTableBytesPerShard = uint64(roundMiB(max(memTotal/int64(shards), minMemTableBytes))) // #nosec G115 -- roundMiB returns at least 1 MiB
	}
	if o.CacheBytes > 0 {
		p.CacheBytesPerShard = o.CacheBytes
	}
	if o.MemTableBytes > 0 {
		p.MemTableBytesPerShard = uint64(o.MemTableBytes) // #nosec G115 -- positive, checked above
	}
	return p
}

func clamp(v, lo, hi int64) int64 { return min(max(v, lo), hi) }

func roundMiB(v int64) int64 { return max(v/mib*mib, mib) }

// GoMemLimit is fraction × limit; an invalid fraction selects the default.
func GoMemLimit(limit int64, fraction float64) int64 {
	if fraction <= 0 || fraction > 1 {
		fraction = DefaultGoMemLimitFraction
	}
	return int64(float64(limit) * fraction)
}

// ApplyGoMemLimit sets the Go runtime soft memory limit to fraction × limit
// unless GOMEMLIMIT is set in the environment (the operator's explicit
// value, already applied by the runtime, always wins) or no limit is known.
// It returns the limit in effect and its source: "GOMEMLIMIT", "derived"
// or "none".
func ApplyGoMemLimit(limit int64, fraction float64, lookupEnv func(string) (string, bool)) (int64, string) {
	if lookupEnv == nil {
		lookupEnv = os.LookupEnv
	}
	if v, ok := lookupEnv(goMemLimitEnv); ok && strings.TrimSpace(v) != "" {
		return debug.SetMemoryLimit(-1), SourceEnv
	}
	if limit <= 0 {
		return debug.SetMemoryLimit(-1), SourceNone
	}
	target := GoMemLimit(limit, fraction)
	if target <= 0 || target == math.MaxInt64 {
		return debug.SetMemoryLimit(-1), SourceNone
	}
	debug.SetMemoryLimit(target)
	return target, SourceDerived
}

// Report describes the memory configuration applied to the process.
type Report struct {
	LimitBytes    int64  // container limit; 0 = unlimited/unknown
	LimitSource   string // "config", "cgroup" or "none"
	GoLimitBytes  int64  // Go soft limit in effect (math.MaxInt64 = none)
	GoLimitSource string // "GOMEMLIMIT", "derived" or "none"
}

// ConfigureProcess resolves the container limit, applies the Go soft limit
// and publishes both as metrics. A detection error is returned for logging;
// the process then runs with the runtime's existing limit.
func ConfigureProcess(explicit int64, fraction float64, src Source, lookupEnv func(string) (string, bool)) (Report, error) {
	limit, source, err := Resolve(explicit, src)
	applied, how := ApplyGoMemLimit(limit, fraction, lookupEnv)
	metrics.MemoryContainerLimitBytes.Set(float64(limit))
	if applied == math.MaxInt64 {
		metrics.MemoryGoLimitBytes.Set(0)
	} else {
		metrics.MemoryGoLimitBytes.Set(float64(applied))
	}
	return Report{LimitBytes: limit, LimitSource: source, GoLimitBytes: applied, GoLimitSource: how}, err
}
