//go:build windows

package store

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

// lockFile takes an exclusive, non-blocking lock on the whole file via
// LockFileEx — the Windows counterpart of flock.
func lockFile(f *os.File) error {
	overlapped := new(windows.Overlapped)
	err := windows.LockFileEx(
		windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0,
		^uint32(0), ^uint32(0), // lock the entire file
		overlapped,
	)
	if err != nil {
		return fmt.Errorf("store is locked by another dockervc operation: %w", err)
	}
	return nil
}

// unlockFile releases the lock.
func unlockFile(f *os.File) error {
	overlapped := new(windows.Overlapped)
	return windows.UnlockFileEx(
		windows.Handle(f.Fd()),
		0,
		^uint32(0), ^uint32(0),
		overlapped,
	)
}
