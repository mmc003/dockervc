package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dockervc/internal/model"
	"dockervc/internal/store"
)

// brokenStore builds a temp store with: one snapshot whose only object file
// has been deleted (the manual-rm scenario), plus one untracked stray file.
func brokenStore(t *testing.T) *store.Store {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("DOCKERVC_HOME", dir)
	storePath = filepath.Join(dir, "store")
	s, err := store.Init(storePath)
	if err != nil {
		t.Fatalf("init store: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	obj, err := s.PutBlob("volume", "", strings.NewReader("volume data"))
	if err != nil {
		t.Fatalf("put blob: %v", err)
	}
	m := &model.Manifest{
		ID: "snap-doctor-test-1", CreatedAt: time.Now(), Message: "test",
		Volumes: []model.VolumeRecord{{Name: "v", Object: obj.Hash, Size: obj.Size}},
	}
	if err := s.InsertSnapshot(m); err != nil {
		t.Fatalf("insert snapshot: %v", err)
	}
	if err := os.Remove(s.ObjectPath(obj.Hash)); err != nil { // the damage
		t.Fatalf("remove object file: %v", err)
	}
	stray := filepath.Join(s.Path, "objects", "zz")
	if err := os.MkdirAll(stray, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stray, "zznotincatalog"), []byte("junk"), 0o644); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestDoctorDiagnosesBrokenSnapshot(t *testing.T) {
	s := brokenStore(t)
	rep, err := diagnose(s, false)
	if err != nil {
		t.Fatalf("diagnose: %v", err)
	}
	if rep.Healthy() {
		t.Fatal("store with deleted object files must not be healthy")
	}
	if len(rep.Broken) != 1 || rep.Broken[0].ID != "snap-doctor-test-1" {
		t.Fatalf("Broken = %+v", rep.Broken)
	}
	if len(rep.Broken[0].Missing) != 1 {
		t.Fatalf("expected 1 missing object, got %v", rep.Broken[0].Missing)
	}
	if len(rep.Untracked) != 1 || !strings.HasSuffix(rep.Untracked[0].Path, "zznotincatalog") {
		t.Fatalf("Untracked = %+v", rep.Untracked)
	}
	if len(rep.Orphans) != 0 {
		t.Fatalf("referenced-but-missing object is not an orphan: %+v", rep.Orphans)
	}

	// The log marker must name the same damage.
	rows, err := s.ListSnapshots()
	if err != nil || len(rows) != 1 {
		t.Fatalf("ListSnapshots: %v %v", rows, err)
	}
	if got := BrokenSuffix(s, rows[0]); got != "✗ BROKEN (1 object missing)" {
		t.Fatalf("BrokenSuffix = %q", got)
	}
}

func TestDoctorRepairAutoYes(t *testing.T) {
	s := brokenStore(t)
	rep, err := diagnose(s, false)
	if err != nil {
		t.Fatalf("diagnose: %v", err)
	}
	var buf bytes.Buffer
	if err := applyRepairs(s, rep, func(string) bool { return true }, &buf); err != nil {
		t.Fatalf("applyRepairs: %v", err)
	}

	if rows, _ := s.ListSnapshots(); len(rows) != 0 {
		t.Fatalf("broken snapshot should be deleted, still %d rows", len(rows))
	}
	if objs, _ := s.AllObjects(); len(objs) != 0 {
		t.Fatalf("orphaned row should be removed, still %+v", objs)
	}
	if _, err := os.Stat(filepath.Join(s.Path, "objects", "zz", "zznotincatalog")); !os.IsNotExist(err) {
		t.Fatal("untracked file should be deleted")
	}

	rep, err = diagnose(s, false)
	if err != nil {
		t.Fatalf("re-diagnose: %v", err)
	}
	if !rep.Healthy() {
		t.Fatalf("store should be healthy after repair: %+v", rep)
	}
}

// Declining must keep everything — the prompts are the whole point.
func TestDoctorRepairDeclined(t *testing.T) {
	s := brokenStore(t)
	rep, _ := diagnose(s, false)
	var buf bytes.Buffer
	if err := applyRepairs(s, rep, func(string) bool { return false }, &buf); err != nil {
		t.Fatalf("applyRepairs: %v", err)
	}
	if rows, _ := s.ListSnapshots(); len(rows) != 1 {
		t.Fatalf("declined repair must keep the snapshot")
	}
	if _, err := os.Stat(filepath.Join(s.Path, "objects", "zz", "zznotincatalog")); err != nil {
		t.Fatalf("declined repair must keep untracked files: %v", err)
	}
	rep2, _ := diagnose(s, false)
	if rep2.Healthy() {
		t.Fatal("declined repair must leave problems reported")
	}
}

// Deep check: an object file whose bytes were swapped in place still stats
// fine, so only the re-hash catches it.
func TestDoctorDeepDetectsCorruption(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DOCKERVC_HOME", dir)
	storePath = filepath.Join(dir, "store")
	s, err := store.Init(storePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	obj, err := s.PutBlob("volume", "", strings.NewReader("volume data"))
	if err != nil {
		t.Fatal(err)
	}
	gone, err := s.PutBlob("image", "", strings.NewReader("image data"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.InsertSnapshot(&model.Manifest{
		ID: "snap-deep", CreatedAt: time.Now(),
		Volumes: []model.VolumeRecord{{Name: "v", Object: obj.Hash}},
		Images:  []model.ImageRecord{{Refs: []string{"img"}, Object: gone.Hash}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.ObjectPath(obj.Hash), []byte("tampered"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(s.ObjectPath(gone.Hash)); err != nil {
		t.Fatal(err)
	}

	// Structural pass must report the deletion but stay blind to the tamper.
	rep, err := diagnose(s, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Broken) != 1 || len(rep.Broken[0].Missing) != 1 || len(rep.Broken[0].Corrupt) != 0 {
		t.Fatalf("structural pass: %+v", rep.Broken)
	}

	rep, err = diagnose(s, true)
	if err != nil {
		t.Fatal(err)
	}
	// Only the tampered file counts as corrupted — the missing one belongs to
	// the structural finding, not double-listed here.
	if len(rep.Corrupted) != 1 || rep.Corrupted[0].Hash != obj.Hash {
		t.Fatalf("Corrupted = %+v", rep.Corrupted)
	}
	// One merged entry per snapshot: 1 missing + 1 corrupt, not two rows.
	if len(rep.Broken) != 1 || len(rep.Broken[0].Missing) != 1 || len(rep.Broken[0].Corrupt) != 1 {
		t.Fatalf("deep pass must merge into one entry: %+v", rep.Broken)
	}

	// Repair: auto-yes deletes the unrestorable snapshot, the orphan sweep
	// then removes the corrupted row and its file.
	var buf bytes.Buffer
	if err := applyRepairs(s, rep, func(string) bool { return true }, &buf); err != nil {
		t.Fatalf("applyRepairs: %v", err)
	}
	rep, err = diagnose(s, true)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Healthy() {
		t.Fatalf("deep check should pass after repair: %+v", rep)
	}
}

func TestDoctorHealthyStore(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DOCKERVC_HOME", dir)
	storePath = filepath.Join(dir, "store")
	s, err := store.Init(storePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	obj, err := s.PutBlob("volume", "", strings.NewReader("data"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.InsertSnapshot(&model.Manifest{
		ID: "snap-ok", CreatedAt: time.Now(),
		Volumes: []model.VolumeRecord{{Name: "v", Object: obj.Hash}},
	}); err != nil {
		t.Fatal(err)
	}
	rep, err := diagnose(s, false)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Healthy() {
		t.Fatalf("healthy store flagged: %+v", rep)
	}
	rows, _ := s.ListSnapshots()
	if got := BrokenSuffix(s, rows[0]); got != "" {
		t.Fatalf("healthy snapshot marked broken: %q", got)
	}
	rep, err = diagnose(s, true)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Healthy() || rep.HashChecked != 1 {
		t.Fatalf("deep pass on healthy store: %+v", rep)
	}
}
