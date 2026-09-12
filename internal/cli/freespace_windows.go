//go:build windows

package cli

// freeSpace is unimplemented on Windows: the syscall-free GetDiskFreeSpaceEx
// binding lives in x/sys/windows and is trivial, but this build has no live
// verification path here, so the export command simply skips the advisory
// check — a failed write on a full disk surfaces the condition honestly.
func freeSpace(dir string) (uint64, bool) {
	return 0, false
}
