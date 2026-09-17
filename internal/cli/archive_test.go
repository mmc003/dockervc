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

func TestArchiveCommandsEndToEnd(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DOCKERVC_HOME", dir)
	storePath = filepath.Join(dir, "store")

	s, err := store.Init(storePath)
	if err != nil {
		t.Fatal(err)
	}
	volume, err := s.PutBlob("volume", "", strings.NewReader("volume tar placeholder"))
	if err != nil {
		t.Fatal(err)
	}
	index := &snapshot.FileIndex{Files: map[string]snapshot.FileMeta{
		"config/app.ini": {Type: "file", Size: 12, Mtime: 1_700_000_000, Mode: 0o644, Hash: strings.Repeat("a", 64)},
		"data/item.txt":  {Type: "file", Size: 7, Mtime: 1_700_000_100, Mode: 0o600, Hash: strings.Repeat("b", 64)},
		"current":        {Type: "symlink", Mtime: 1_700_000_200, Mode: 0o777, Link: "data/item.txt"},
	}}
	indexObject, err := s.PutBlob("volindex", "", index.Reader())
	if err != nil {
		t.Fatal(err)
	}
	m := &model.Manifest{
		ID:        "snap-archive-commands",
		CreatedAt: time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC),
		Message:   "archive command fixture",
		Volumes: []model.VolumeRecord{{
			Name: "app-data", Object: volume.Hash, IndexObject: indexObject.Hash,
			Files: len(index.Files), Size: volume.Size,
		}},
		TotalSize: volume.Size + indexObject.Size,
	}
	if err := s.InsertSnapshot(m); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	archive := filepath.Join(dir, "exports", "app.dvca")
	out := captureOutput(func() { runArgs("export", m.ID, "-o", archive) })
	if !strings.Contains(out, "Exported "+m.ID) {
		t.Fatalf("export output = %q", out)
	}

	out = captureOutput(func() { runArgs("archive", "list", filepath.Dir(archive)) })
	for _, want := range []string{m.ID, "app.dvca", "archives in"} {
		if !strings.Contains(out, want) {
			t.Fatalf("archive list missing %q:\n%s", want, out)
		}
	}

	out = captureOutput(func() { runArgs("archive", "show", archive) })
	for _, want := range []string{m.ID, m.Message, "app-data", "3 indexed file(s)", "verification:   not run"} {
		if !strings.Contains(out, want) {
			t.Fatalf("archive show missing %q:\n%s", want, out)
		}
	}

	out = captureOutput(func() { runArgs("archive", "verify", archive) })
	if !strings.Contains(out, "Verified "+archive) || !strings.Contains(out, m.ID) {
		t.Fatalf("archive verify output = %q", out)
	}

	out = captureOutput(func() { runArgs("archive", "files", archive, "app-data", "config") })
	if !strings.Contains(out, "config/app.ini") || strings.Contains(out, "data/item.txt") {
		t.Fatalf("archive files output = %q", out)
	}

	out = captureOutput(func() { runArgs("archive", "files", archive, "app-data", "../escape") })
	if !strings.Contains(out, "must not contain ..") {
		t.Fatalf("unsafe prefix output = %q", out)
	}
}

func TestArchiveIsAvailableInGuidedTUI(t *testing.T) {
	found := false
	for _, action := range menuActions {
		if action.name == "archive" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("archive action missing from interactive menu")
	}

	tt := &tui{}
	tt.askArchiveAction()
	if tt.dlg == nil || tt.dlg.kind != dlgList || len(tt.dlg.items) != 4 {
		t.Fatalf("archive dialog = %+v", tt.dlg)
	}
	want := []string{"list", "show", "verify", "files"}
	for i, value := range want {
		if tt.dlg.items[i].value != value {
			t.Fatalf("archive action %d = %q, want %q", i, tt.dlg.items[i].value, value)
		}
	}
}
