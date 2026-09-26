//go:build linux || darwin || freebsd

package archive

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// lockFolder takes the archive's writer lock: an flock on LockFile, held for
// as long as the archive is open. The kernel drops it when the process dies,
// however it dies, so a killed or crashed server never leaves a stale lock
// behind for the next one to trip over.
func lockFolder(root string) (func() error, error) {
	path := filepath.Join(root, LockFile)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o640)
	if err != nil {
		return nil, fmt.Errorf("archive: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		holder, _ := os.ReadFile(path)
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			if pid := strings.TrimSpace(string(holder)); pid != "" {
				return nil, fmt.Errorf("%w (pid %s)", ErrLocked, pid)
			}
			return nil, ErrLocked
		}
		return nil, fmt.Errorf("archive: lock %s: %w", path, err)
	}
	// The pid is for a person wondering who holds it; nothing reads it back
	// except the message above.
	f.Truncate(0)
	f.WriteAt([]byte(fmt.Sprintf("%d\n", os.Getpid())), 0)
	return f.Close, nil
}
