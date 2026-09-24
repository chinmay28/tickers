//go:build !(linux || darwin || freebsd)

package archiver

// sameDevice can't tell here, so it never warns.
func sameDevice(a, b string) bool { return false }
