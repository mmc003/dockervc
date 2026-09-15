//go:build windows

package store

import (
	"os"

	"golang.org/x/sys/windows"
)

const wholeFileRange = ^uint32(0)

// lockFile takes an exclusive, non-blocking advisory lock on the whole file.
func lockFile(f *os.File) error {
	return windows.LockFileEx(
		windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0,
		wholeFileRange,
		wholeFileRange,
		new(windows.Overlapped),
	)
}

// unlockFile releases the same whole-file byte range used by lockFile.
func unlockFile(f *os.File) error {
	return windows.UnlockFileEx(
		windows.Handle(f.Fd()),
		0,
		wholeFileRange,
		wholeFileRange,
		new(windows.Overlapped),
	)
}
