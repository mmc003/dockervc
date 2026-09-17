package cli

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dockervc/internal/model"
	"dockervc/internal/snapshot"
	"dockervc/internal/store"
)

func TestShowReportsSnapshotStorageSharing(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DOCKERVC_HOME", dir)
	storePath = filepath.Join(dir, "store")

	s, err := store.Init(storePath)
	if err != nil {
		t.Fatal(err)
	}
	shared, err := s.PutBlob("volume", "", strings.NewReader("shared object"))
	if err != nil {
		t.Fatal(err)
	}
	exclusive, err := s.PutBlob("volume", "", strings.NewReader("exclusive object"))
	if err != nil {
		t.Fatal(err)
	}
	a := &model.Manifest{
		ID:        "snap-show-a",
		CreatedAt: time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC),
		Message:   "storage accounting",
		Stats: model.SnapshotStats{
			NewObjects:    1,
			ReusedObjects: 1,
			NewBytes:      exclusive.Size,
		},
		Volumes: []model.VolumeRecord{
			{Name: "shared", Object: shared.Hash, Size: shared.Size},
			{Name: "exclusive", Object: exclusive.Hash, Size: exclusive.Size},
		},
	}
	b := &model.Manifest{
		ID:        "snap-show-b",
		CreatedAt: a.CreatedAt.Add(time.Minute),
		Volumes:   []model.VolumeRecord{{Name: "shared", Object: shared.Hash, Size: shared.Size}},
	}
	if err := s.InsertSnapshot(a); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertSnapshot(b); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	out := captureOutput(func() { runArgs("show", a.ID) })
	wants := []string{
		"capture:        1 new object(s) (" + snapshot.HumanBytes(exclusive.Size) + "), 1 reused object(s)",
		"storage:",
		"referenced:     " + snapshot.HumanBytes(shared.Size+exclusive.Size) + " across 2 object(s)",
		"exclusive:      " + snapshot.HumanBytes(exclusive.Size) + " across 1 object(s)",
		"shared:         " + snapshot.HumanBytes(shared.Size) + " across 1 object(s)",
	}
	for _, want := range wants {
		if !strings.Contains(out, want) {
			t.Fatalf("show output missing %q:\n%s", want, out)
		}
	}
}
