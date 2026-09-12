//go:build darwin || linux

package cli

import "syscall"

// freeSpace reports a directory's available bytes. Best-effort by design:
// (0, false) means "couldn't tell — skip the check" and a failed write on a
// full disk is an acceptable outcome for this prototype. Bsize (not Linux's
// Frsize) is used deliberately: it is the only block size darwin's Statfs_t
// has, ext4 sets both to the same value, and this is advisory.
func freeSpace(dir string) (uint64, bool) {
	var fs syscall.Statfs_t
	if err := syscall.Statfs(dir, &fs); err != nil {
		return 0, false
	}
	if fs.Bsize <= 0 {
		return 0, false
	}
	return uint64(fs.Bavail) * uint64(fs.Bsize), true
}
