//go:build unix

package diskguard

import (
	"errors"
	"fmt"
	"syscall"
)

// Statfs samples the filesystem holding path. FreeBytes is f_bavail (space
// available to unprivileged users), matching what the non-root CodeQ
// process can actually write.
func Statfs(path string) (Usage, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return Usage{}, fmt.Errorf("statfs: %w", err)
	}
	bsize, ok := nonNegative(st.Bsize)
	if !ok || bsize == 0 {
		return Usage{}, errors.New("statfs: invalid block size")
	}
	return Usage{TotalBytes: st.Blocks * bsize, FreeBytes: st.Bavail * bsize}, nil
}

// nonNegative converts the platform-specific block size field (int64 on
// Linux, uint32 on Darwin) without a lossy signed conversion.
func nonNegative[T ~int32 | ~int64 | ~uint32 | ~uint64](v T) (uint64, bool) {
	if v < 0 {
		return 0, false
	}
	return uint64(v), true
}
