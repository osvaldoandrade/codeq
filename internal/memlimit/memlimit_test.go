package memlimit

import (
	"os"
	"path/filepath"
	"runtime/debug"
	"testing"
)

func writeFile(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestParseLimit(t *testing.T) {
	cases := []struct {
		raw   string
		limit int64
		ok    bool
		err   bool
	}{
		{"max\n", 0, false, false},
		{"536870912\n", 512 << 20, true, false},
		{" 1073741824 ", 1 << 30, true, false},
		{"9223372036854771712\n", 0, false, false}, // cgroup v1 unlimited
		{"0", 0, false, false},
		{"-1", 0, false, true},
		{"lots", 0, false, true},
	}
	for _, tc := range cases {
		limit, ok, err := ParseLimit(tc.raw)
		if (err != nil) != tc.err || ok != tc.ok || limit != tc.limit {
			t.Fatalf("ParseLimit(%q) = %d,%v,%v; want %d,%v,err=%v", tc.raw, limit, ok, err, tc.limit, tc.ok, tc.err)
		}
	}
}

func TestLimitCgroupV2Namespaced(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "memory.max"), "536870912\n")
	limit, ok, err := Source{Root: root, SelfCgroup: filepath.Join(root, "absent")}.Limit()
	if err != nil || !ok || limit != 512<<20 {
		t.Fatalf("Limit = %d %v %v", limit, ok, err)
	}
}

func TestLimitCgroupV2PrefersOwnCgroup(t *testing.T) {
	root := t.TempDir()
	self := filepath.Join(t.TempDir(), "cgroup")
	writeFile(t, self, "0::/kubepods/pod1/ctr\n")
	writeFile(t, filepath.Join(root, "kubepods/pod1/ctr/memory.max"), "268435456\n")
	limit, ok, err := Source{Root: root, SelfCgroup: self}.Limit()
	if err != nil || !ok || limit != 256<<20 {
		t.Fatalf("Limit = %d %v %v", limit, ok, err)
	}
}

func TestLimitCgroupV2Unlimited(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "memory.max"), "max\n")
	limit, ok, err := Source{Root: root, SelfCgroup: filepath.Join(root, "absent")}.Limit()
	if err != nil || ok || limit != 0 {
		t.Fatalf("Limit = %d %v %v", limit, ok, err)
	}
}

func TestLimitCgroupV1Fallback(t *testing.T) {
	root := t.TempDir()
	self := filepath.Join(t.TempDir(), "cgroup")
	writeFile(t, self, "12:cpu,cpuacct:/x\n9:memory:/docker/abc\n")
	writeFile(t, filepath.Join(root, "memory/docker/abc/memory.limit_in_bytes"), "402653184\n")
	limit, ok, err := Source{Root: root, SelfCgroup: self}.Limit()
	if err != nil || !ok || limit != 384<<20 {
		t.Fatalf("Limit = %d %v %v", limit, ok, err)
	}
	// Root-level v1 file when the process path is not mounted.
	root2 := t.TempDir()
	writeFile(t, filepath.Join(root2, "memory/memory.limit_in_bytes"), "9223372036854771712\n")
	if limit, ok, err = (Source{Root: root2, SelfCgroup: self}).Limit(); err != nil || ok || limit != 0 {
		t.Fatalf("v1 unlimited = %d %v %v", limit, ok, err)
	}
}

func TestLimitNoCgroup(t *testing.T) {
	root := t.TempDir()
	limit, ok, err := Source{Root: root, SelfCgroup: filepath.Join(root, "absent")}.Limit()
	if err != nil || ok || limit != 0 {
		t.Fatalf("Limit = %d %v %v", limit, ok, err)
	}
}

func TestLimitMalformedIsAnError(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "memory.max"), "garbage")
	if _, _, err := (Source{Root: root, SelfCgroup: filepath.Join(root, "absent")}).Limit(); err == nil {
		t.Fatal("malformed memory.max accepted")
	}
}

func TestResolve(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "memory.max"), "536870912")
	src := Source{Root: root, SelfCgroup: filepath.Join(root, "absent")}
	if limit, source, err := Resolve(1<<30, src); err != nil || limit != 1<<30 || source != SourceConfig {
		t.Fatalf("explicit = %d %s %v", limit, source, err)
	}
	if limit, source, err := Resolve(0, src); err != nil || limit != 512<<20 || source != SourceCgroup {
		t.Fatalf("cgroup = %d %s %v", limit, source, err)
	}
	empty := Source{Root: t.TempDir(), SelfCgroup: filepath.Join(root, "absent")}
	if limit, source, err := Resolve(0, empty); err != nil || limit != 0 || source != SourceNone {
		t.Fatalf("none = %d %s %v", limit, source, err)
	}
}

func TestDeriveFromProductionLimit(t *testing.T) {
	p := Derive(512<<20, 1, Overrides{})
	if p.CacheBytesPerShard != 76<<20 {
		t.Fatalf("cache = %d MiB, want 76", p.CacheBytesPerShard>>20)
	}
	if p.MemTableBytesPerShard != 16<<20 {
		t.Fatalf("memtable = %d MiB, want 16", p.MemTableBytesPerShard>>20)
	}
	// Cache + 3 memtables (2 queued + mutable) + Go soft limit headroom must
	// stay well below the container limit.
	pebble := p.CacheBytesPerShard + 3*int64(p.MemTableBytesPerShard) // #nosec G115 -- small test value
	if pebble > (512<<20)/3 {
		t.Fatalf("pebble budget %d MiB exceeds a third of 512 MiB", pebble>>20)
	}
	if got := GoMemLimit(512<<20, 0); got != 375809638 {
		t.Fatalf("GoMemLimit = %d", got)
	}
}

func TestDeriveFloorsCeilingsShardsAndOverrides(t *testing.T) {
	if p := Derive(0, 1, Overrides{}); p.CacheBytesPerShard != 256<<20 || p.MemTableBytesPerShard != 64<<20 {
		t.Fatalf("unlimited must keep legacy sizes: %+v", p)
	}
	if p := Derive(64<<30, 1, Overrides{}); p.CacheBytesPerShard != 256<<20 || p.MemTableBytesPerShard != 64<<20 {
		t.Fatalf("ceiling not applied: %+v", p)
	}
	if p := Derive(32<<20, 1, Overrides{}); p.CacheBytesPerShard != 8<<20 || p.MemTableBytesPerShard != 4<<20 {
		t.Fatalf("floor not applied: %+v", p)
	}
	if p := Derive(1<<30, 4, Overrides{}); p.CacheBytesPerShard != 38<<20 || p.MemTableBytesPerShard != 8<<20 {
		t.Fatalf("shard split = %+v", p)
	}
	if p := Derive(512<<20, 0, Overrides{}); p.CacheBytesPerShard != 76<<20 {
		t.Fatalf("zero shards must mean one: %+v", p)
	}
	p := Derive(512<<20, 1, Overrides{CacheBytes: 32 << 20, MemTableBytes: 8 << 20})
	if p.CacheBytesPerShard != 32<<20 || p.MemTableBytesPerShard != 8<<20 {
		t.Fatalf("overrides ignored: %+v", p)
	}
}

func TestApplyGoMemLimit(t *testing.T) {
	old := debug.SetMemoryLimit(-1)
	t.Cleanup(func() { debug.SetMemoryLimit(old) })
	noEnv := func(string) (string, bool) { return "", false }

	got, source := ApplyGoMemLimit(512<<20, 0.70, noEnv)
	if source != SourceDerived || got != 375809638 || debug.SetMemoryLimit(-1) != got {
		t.Fatalf("derived = %d %s", got, source)
	}

	debug.SetMemoryLimit(300 << 20)
	explicit := func(k string) (string, bool) { return "300MiB", k == goMemLimitEnv }
	if got, source = ApplyGoMemLimit(512<<20, 0.70, explicit); source != SourceEnv || got != 300<<20 {
		t.Fatalf("explicit GOMEMLIMIT overridden: %d %s", got, source)
	}

	if _, source = ApplyGoMemLimit(0, 0.70, noEnv); source != SourceNone {
		t.Fatalf("no limit = %s", source)
	}
	if debug.SetMemoryLimit(-1) != 300<<20 {
		t.Fatal("no limit must leave the runtime limit untouched")
	}
	if got := GoMemLimit(1000, 2); got != 700 {
		t.Fatalf("invalid fraction must use the default: %d", got)
	}
}

func TestConfigureProcess(t *testing.T) {
	old := debug.SetMemoryLimit(-1)
	t.Cleanup(func() { debug.SetMemoryLimit(old) })
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "memory.max"), "536870912\n")
	src := Source{Root: root, SelfCgroup: filepath.Join(root, "absent")}
	noEnv := func(string) (string, bool) { return "", false }
	r, err := ConfigureProcess(0, 0.70, src, noEnv)
	if err != nil || r.LimitBytes != 512<<20 || r.LimitSource != SourceCgroup || r.GoLimitSource != SourceDerived || r.GoLimitBytes != 375809638 {
		t.Fatalf("report = %+v err=%v", r, err)
	}
	writeFile(t, filepath.Join(root, "memory.max"), "junk")
	debug.SetMemoryLimit(old)
	if r, err = ConfigureProcess(0, 0.70, src, noEnv); err == nil || r.GoLimitSource != SourceNone {
		t.Fatalf("malformed cgroup: report = %+v err=%v", r, err)
	}
}
