//go:build !windows

package singleinstance

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// held keeps the lock file open for the process lifetime. Closing the fd
// would release the flock, so we deliberately stash it here and never close.
var held *os.File //nolint:unused

// LockPath returns the lock file Acquire uses for name.
func LockPath(name string) string {
	return filepath.Join(os.TempDir(), fmt.Sprintf("%s-%d.lock", name, os.Getuid()))
}

// Acquire takes an exclusive advisory flock on a lock file in the system temp
// dir. Returns an error if another process already holds the lock. The kernel
// releases the flock when the process exits (including SIGKILL), so no stale-
// lock cleanup is needed. The file name carries the user id: the temp dir is
// shared between users and the file is 0600, so a lock file left by another user
// would otherwise make this one fail with a misleading permission error. This
// matches the Windows mutex, which is per login session.
func Acquire(name string) error {
	path := LockPath(name)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return fmt.Errorf("open lock file: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if err == syscall.EWOULDBLOCK {
			return fmt.Errorf("another instance is already running")
		}
		return fmt.Errorf("flock: %w", err)
	}
	held = f
	return nil
}
