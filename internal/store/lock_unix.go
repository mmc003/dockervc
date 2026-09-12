//go:build !windows

package store

import (
	"os"

	"golang.org/x/sys/unix"
)

// lockFile takes an exclusive, non-blocking advisory lock on the file.
func lockFile(f *os.File) error {
	return unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
}

// unlockFile releases it.
func unlockFile(f *os.File) error {
	return unix.Flock(int(f.Fd()), unix.LOCK_UN)
}
