// Package singleinstance prevents multiple copies of the process from running
// at once. On Windows it uses a named mutex; on Unix it uses an advisory
// flock. In both cases the OS releases the lock automatically on process
// exit, including crashes, so no stale-lock cleanup is required.
package singleinstance

import (
	"fmt"
	"syscall"
	"unsafe"
)

const errAlreadyExists = 183

var (
	kernel32        = syscall.NewLazyDLL("kernel32.dll")
	procCreateMutex = kernel32.NewProc("CreateMutexW")
)

// LockPath returns the lock file Acquire uses for name. Windows uses a named
// mutex, which leaves nothing on disk, so the path is empty.
func LockPath(name string) string { return "" }

// Acquire creates a named mutex. Returns an error if another process in the
// same session already holds it. Use a "Global\\" prefix to span sessions
// (e.g. across RDP users); a bare name is per-session, which is usually what
// a desktop app wants.
func Acquire(name string) error {
	p, err := syscall.UTF16PtrFromString(name)
	if err != nil {
		return fmt.Errorf("invalid mutex name: %w", err)
	}
	h, _, callErr := procCreateMutex.Call(0, 0, uintptr(unsafe.Pointer(p)))
	if h == 0 {
		return fmt.Errorf("CreateMutex failed: %w", callErr)
	}
	if errno, ok := callErr.(syscall.Errno); ok && errno == errAlreadyExists {
		// Close the handle we just opened so we don't leak it; the original
		// owner still holds the mutex.
		_ = syscall.CloseHandle(syscall.Handle(h))
		return fmt.Errorf("another instance is already running")
	}
	// The handle is deliberately never closed: the OS releases the mutex when the
	// process exits, which is the lifetime we want - closing it would defeat the
	// guard. (A raw handle is not garbage-collected, so nothing needs to hold it.)
	return nil
}
