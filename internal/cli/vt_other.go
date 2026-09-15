//go:build !windows

package cli

func enableVTImpl() bool { return true }
