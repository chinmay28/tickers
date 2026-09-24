//go:build linux || darwin || freebsd

package archive

import "syscall"

// Disk reports the free and total bytes of the filesystem holding path —
// what the collector pauses on before a drive fills.
func Disk(path string) (free, total uint64, err error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0, err
	}
	// The field types differ by platform (and by word size on Linux), so
	// both sides are widened before multiplying.
	return uint64(st.Bavail) * uint64(st.Bsize), uint64(st.Blocks) * uint64(st.Bsize), nil
}
