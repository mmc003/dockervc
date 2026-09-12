package cli

import (
	"path/filepath"
	"testing"

	"dockervc/internal/store"
)

// TestRunArgsClosesStoreOnError guards against the store-lock leak: cobra
// skips PersistentPostRun when RunE returns an error, so a failed command
// used to leave the flock held — and every later command in the same process
// failed with "another dockervc operation is holding the store".
func TestRunArgsClosesStoreOnError(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DOCKERVC_HOME", dir)
	storePath = filepath.Join(dir, "store")
	if _, err := store.Init(storePath); err != nil {
		t.Fatalf("init store: %v", err)
	}

	// This command fails after PersistentPreRunE has opened the store.
	runArgs("show", "snap-does-not-exist")
	if st != nil {
		st.Close()
		t.Fatal("store handle leaked after a failed command — later commands would hit the lock")
	}

	// The next command must still be able to open the store.
	runArgs("log")
	if st != nil {
		st.Close()
		t.Fatal("store handle leaked after a successful command")
	}
}
