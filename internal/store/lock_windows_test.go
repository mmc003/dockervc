//go:build windows

package store

import (
	"strings"
	"testing"
	"time"
)

func TestWindowsStoreLockIsExclusiveAndReleasedOnClose(t *testing.T) {
	path := t.TempDir() + `\store`
	first, err := Init(path)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}

	started := time.Now()
	second, err := Open(path)
	if second != nil {
		second.Close()
	}
	if err == nil {
		first.Close()
		t.Fatal("second Open succeeded while the first store handle held the lock")
	}
	if !strings.Contains(err.Error(), "another dockervc operation is holding the store") {
		first.Close()
		t.Fatalf("second Open error = %q, want clear lock message", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		first.Close()
		t.Fatalf("second Open took %s; Windows lock must fail immediately", elapsed)
	}

	if err := first.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("Open after Close: %v", err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatalf("reopened Close: %v", err)
	}
}
