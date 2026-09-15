//go:build windows

package cli

import "golang.org/x/sys/windows"

// freeSpace reports the bytes available to the current user on the volume
// containing dir. Failure is advisory: callers skip the pre-flight check.
func freeSpace(dir string) (uint64, bool) {
	var availableToUser, total, totalFree uint64
	path, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return 0, false
	}
	if err := windows.GetDiskFreeSpaceEx(path, &availableToUser, &total, &totalFree); err != nil {
		return 0, false
	}
	return availableToUser, true
}
