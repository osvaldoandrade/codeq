package raft

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	pebbledb "github.com/cockroachdb/pebble"
	hraft "github.com/hashicorp/raft"
)

func TestAutoSnapshotRetain(t *testing.T) {
	// 0 is an unknown volume; 976 MiB is the ext4 view of a 1Gi PVC.
	for _, tc := range []struct {
		total uint64
		want  int
	}{
		{0, DefaultSnapshotRetain},
		{976 << 20, 1},
		{1 << 30, 1},
		{1<<30 + 1, DefaultSnapshotRetain},
		{10 << 30, DefaultSnapshotRetain},
	} {
		if got := AutoSnapshotRetain(tc.total); got != tc.want {
			t.Fatalf("AutoSnapshotRetain(%d) = %d, want %d", tc.total, got, tc.want)
		}
	}
}

func writeFile(t *testing.T, path string, size int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, bytes.Repeat([]byte{1}, size), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestMeasureSnapshotDir(t *testing.T) {
	if du, err := MeasureSnapshotDir(filepath.Join(t.TempDir(), "absent")); err != nil || (du != SnapshotDiskUsage{}) {
		t.Fatalf("missing dir = %+v, %v", du, err)
	}
	dir := t.TempDir()
	// Older (term 2, index 90) and newest (term 3, index 10) snapshots: the
	// term decides, as in hashicorp's ordering.
	writeFile(t, filepath.Join(dir, "2-90-100", "state.bin"), 700)
	writeFile(t, filepath.Join(dir, "2-90-100", "meta.json"), 10)
	writeFile(t, filepath.Join(dir, "3-10-50", "state.bin"), 300)
	writeFile(t, filepath.Join(dir, "3-10-50", "meta.json"), 10)
	// An in-progress snapshot counts on disk but is never the latest.
	writeFile(t, filepath.Join(dir, "3-20-60.tmp", "state.bin"), 5000)
	// Foreign entries: a directory without a parsable name, one without
	// state.bin and a loose file.
	writeFile(t, filepath.Join(dir, "garbage", "state.bin"), 1)
	writeFile(t, filepath.Join(dir, "4-1-1", "meta.json"), 2)
	writeFile(t, filepath.Join(dir, "loose"), 99)

	du, err := MeasureSnapshotDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if du.LatestBytes != 300 {
		t.Fatalf("latest = %d, want 300", du.LatestBytes)
	}
	if want := uint64(700 + 10 + 300 + 10 + 5000 + 1 + 2); du.OnDiskBytes != want {
		t.Fatalf("on disk = %d, want %d", du.OnDiskBytes, want)
	}
}

func TestParseSnapshotIDOrdering(t *testing.T) {
	for _, bad := range []string{"", "1-2", "1-2-3-4", "a-2-3", "1-b-3", "1-2-c"} {
		if _, ok := parseSnapshotID(bad); ok {
			t.Fatalf("parseSnapshotID(%q) accepted", bad)
		}
	}
	a, _ := parseSnapshotID("1-5-9")
	b, _ := parseSnapshotID("1-6-1")
	c, _ := parseSnapshotID("1-6-2")
	if !a.less(b) || !b.less(c) || c.less(a) {
		t.Fatalf("ordering broken: %v %v %v", a, b, c)
	}
}

// TestMeasureSnapshotDirMatchesFileSnapshotStore drives the real hashicorp
// store so the directory layout assumptions stay pinned to the dependency.
func TestMeasureSnapshotDirMatchesFileSnapshotStore(t *testing.T) {
	base := filepath.Join(t.TempDir(), "snapshots")
	store, err := newSnapshotStore(base, 1)
	if err != nil {
		t.Fatal(err)
	}
	for i, size := range []int{4096, 1024} {
		sink, err := store.Create(hraft.SnapshotVersionMax, uint64(i+1)*10, 1, hraft.Configuration{}, 1, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := sink.Write(bytes.Repeat([]byte{7}, size)); err != nil {
			t.Fatal(err)
		}
		if err := sink.Close(); err != nil {
			t.Fatal(err)
		}
	}
	du, err := MeasureSnapshotStore(base)
	if err != nil {
		t.Fatal(err)
	}
	list, err := store.List()
	if err != nil || len(list) != 1 {
		t.Fatalf("List = %d, %v", len(list), err)
	}
	if du.LatestBytes != uint64(list[0].Size) || du.LatestBytes != 1024 { // #nosec G115 -- test value
		t.Fatalf("latest = %d, store says %d", du.LatestBytes, list[0].Size)
	}
	// retain=1 reaped the 4 KiB snapshot; meta.json adds a little.
	if du.OnDiskBytes < 1024 || du.OnDiskBytes >= 4096 {
		t.Fatalf("on disk = %d", du.OnDiskBytes)
	}

	// A sink still being written counts on disk only; once published it
	// becomes the latest and the older one is reaped.
	sink, err := store.Create(hraft.SnapshotVersionMax, 99, 1, hraft.Configuration{}, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sink.Write(bytes.Repeat([]byte{7}, 8192)); err != nil {
		t.Fatal(err)
	}
	inFlight, err := MeasureSnapshotStore(base)
	if err != nil {
		t.Fatal(err)
	}
	// bufio writes a chunk larger than its 4 KiB buffer straight through.
	if inFlight.LatestBytes != 1024 || inFlight.OnDiskBytes < du.OnDiskBytes+4096 {
		t.Fatalf("in-flight snapshot = %+v (before %+v)", inFlight, du)
	}
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}
	du2, err := MeasureSnapshotStore(base)
	if err != nil {
		t.Fatal(err)
	}
	if du2.LatestBytes != 8192 {
		t.Fatalf("latest after new snapshot = %d", du2.LatestBytes)
	}
}

func TestDBSnapshotFootprintAccessors(t *testing.T) {
	pdb := openSnapshotTestDB(t)
	for i := range 2000 {
		key := []byte("codeq/tasks/" + string(rune('a'+i%26)) + string(rune('a'+i/26%26)) + string(rune('0'+i%10)) + "-" + itoa(i))
		if err := pdb.Set(key, bytes.Repeat([]byte{byte(i)}, 512), pebbledb.NoSync); err != nil {
			t.Fatal(err)
		}
	}
	if err := pdb.Flush(); err != nil {
		t.Fatal(err)
	}
	path := t.TempDir()
	d := &DB{pebble: pdb, cfg: Config{Path: path, SnapshotRetain: 1}}
	if d.SnapshotRetain() != 1 {
		t.Fatalf("retain = %d", d.SnapshotRetain())
	}
	live, err := d.LiveStateBytes()
	if err != nil || live == 0 {
		t.Fatalf("live state = %d, %v", live, err)
	}
	writeFile(t, filepath.Join(snapshotDirName(path), "snapshots", "1-1-1", "state.bin"), 123)
	du, err := d.SnapshotDiskUsage()
	if err != nil || du.LatestBytes != 123 || du.OnDiskBytes != 123 {
		t.Fatalf("disk usage = %+v, %v", du, err)
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for ; i > 0; i /= 10 {
		b = append([]byte{byte('0' + i%10)}, b...)
	}
	return string(b)
}
