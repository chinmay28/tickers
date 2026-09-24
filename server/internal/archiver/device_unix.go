//go:build linux || darwin || freebsd

package archiver

import "syscall"

// sameDevice reports whether two paths are on the same filesystem.
func sameDevice(a, b string) bool {
	var sa, sb syscall.Stat_t
	if syscall.Stat(a, &sa) != nil || syscall.Stat(b, &sb) != nil {
		return false
	}
	return sa.Dev == sb.Dev
}
