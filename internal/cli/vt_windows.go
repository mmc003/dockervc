//go:build windows

package cli

import (
	"os"

	"golang.org/x/sys/windows"
)

// enableVTImpl enables ANSI escape processing on stdout. UTF-8 output is a
// best-effort companion setting: failing to change the code page must not
// prevent an otherwise VT-capable console from running the full-screen TUI.
func enableVTImpl() bool {
	handle := windows.Handle(os.Stdout.Fd())
	var mode uint32
	if err := windows.GetConsoleMode(handle, &mode); err != nil {
		return false
	}
	if err := windows.SetConsoleMode(handle, mode|windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING); err != nil {
		return false
	}
	_ = windows.SetConsoleOutputCP(65001)
	return true
}
