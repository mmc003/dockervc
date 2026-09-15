//go:build windows

package store

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultPathUsesProgramDataOnWindows(t *testing.T) {
	t.Setenv("DOCKERVC_HOME", "")
	programData := t.TempDir()
	t.Setenv("ProgramData", programData)

	want := filepath.Join(programData, "dockervc")
	if got := DefaultPath(); got != want {
		t.Fatalf("DefaultPath() = %q, want %q", got, want)
	}
}

func TestDefaultPathFallsBackToHomeOnWindows(t *testing.T) {
	t.Setenv("DOCKERVC_HOME", "")
	root := t.TempDir()
	unusable := filepath.Join(root, "not-a-directory")
	if err := os.WriteFile(unusable, []byte("file blocks MkdirAll"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ProgramData", unusable)
	home := filepath.Join(root, "home")
	t.Setenv("USERPROFILE", home)

	want := filepath.Join(home, ".dockervc")
	if got := DefaultPath(); got != want {
		t.Fatalf("DefaultPath() = %q, want %q", got, want)
	}
}
