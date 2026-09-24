//go:build !(linux || darwin || freebsd)

package archive

import "errors"

// Disk is unsupported here; the collector treats an error as "unknown" and
// never pauses for space.
func Disk(path string) (free, total uint64, err error) {
	return 0, 0, errors.New("free space is not available on this platform")
}
