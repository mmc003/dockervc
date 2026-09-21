package cli

import (
	"archive/tar"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dockervc/internal/model"
	"dockervc/internal/store"
)

// buildStoreSnapshot creates a store at storePath holding one real snapshot
// (two PutBlob objects + manifest) without touching the engine.
func buildStoreSnapshot(t *testing.T, id, message string) {
	t.Helper()
	s, err := store.Init(storePath)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer s.Close()
	vol, err := s.PutBlob("volume", "", strings.NewReader("cli-test volume"))
	if err != nil {
		t.Fatal(err)
	}
	img, err := s.PutBlob("image", "sha256:cli", strings.NewReader("cli-test image"))
	if err != nil {
		t.Fatal(err)
	}
	m := &model.Manifest{
		ID:        id,
		CreatedAt: time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC),
		Message:   message,
		Volumes:   []model.VolumeRecord{{Name: "v", Object: vol.Hash, Size: vol.Size}},
		Images:    []model.ImageRecord{{Refs: []string{"nginx:alpine"}, Digest: "sha256:cli", Object: img.Hash, Size: img.Size}},
	}
	if err := s.InsertSnapshot(m); err != nil {
		t.Fatal(err)
	}
}

// TestExportImportViaRunArgs drives the real commands end to end (no engine):
// export from one store, import into a second, refuse overwrite without --yes.
func TestExportImportViaRunArgs(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DOCKERVC_HOME", dir)

	// Source store + snapshot.
	storePath = filepath.Join(dir, "src")
	buildStoreSnapshot(t, "snap-20260912-130000-cli1", "cli export test")

	archive := filepath.Join(dir, "state.dvca")
	out := captureOutput(func() { runArgs("export", "snap-20260912-130000-cli1", "-o", archive) })
	if !strings.Contains(out, "Exported snap-20260912-130000-cli1") {
		t.Fatalf("export output = %q", out)
	}
	if !strings.Contains(out, "writing objects") {
		t.Fatalf("export progress missing: %q", out)
	}
	if strings.Contains(out, "\x1b[") || strings.Contains(out, "\r") {
		t.Fatalf("captured export output contains terminal controls: %q", out)
	}
	if fi, err := os.Stat(archive); err != nil || fi.Size() == 0 {
		t.Fatalf("archive not written: %v", err)
	}

	// An existing -o target is refused without --yes, and --latest + id clash.
	before, _ := os.ReadFile(archive)
	out = captureOutput(func() { runArgs("export", "--latest", "-o", archive) })
	if !strings.Contains(out, "already exists") {
		t.Fatalf("overwrite refusal missing: %q", out)
	}
	after, _ := os.ReadFile(archive)
	if string(before) != string(after) {
		t.Fatal("refused export must not touch the existing file")
	}
	out = captureOutput(func() { runArgs("export", "snap-20260912-130000-cli1", "--latest", "-o", archive) })
	if !strings.Contains(out, "not both") {
		t.Fatalf("id + --latest clash missing: %q", out)
	}
	out = captureOutput(func() { runArgs("export") })
	if !strings.Contains(out, "pass a snapshot id or --latest") {
		t.Fatalf("no-argument error missing: %q", out)
	}

	// Fresh store: import registers the snapshot with identical objects.
	storePath = filepath.Join(dir, "dst")
	buildStoreSnapshot(t, "snap-unrelated", "unrelated") // import must coexist
	out = captureOutput(func() { runArgs("import", archive) })
	if !strings.Contains(out, "imported snap-20260912-130000-cli1") {
		t.Fatalf("import output = %q", out)
	}
	// The store handle must be closed again before any further runArgs — the
	// flock is exclusive even within this process.
	assertImported := func(wantRows int) {
		s, err := store.Open(storePath)
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		rows, err := s.ListSnapshots()
		if err != nil || len(rows) != wantRows {
			t.Fatalf("snapshots after import = %+v (err %v)", rows, err)
		}
		if m, err := s.GetSnapshot("snap-20260912-130000-cli1"); err != nil || m.Message != "cli export test" {
			t.Fatalf("imported manifest wrong: %+v (%v)", m, err)
		}
	}
	assertImported(2)

	// Re-import is the idempotent no-op.
	out = captureOutput(func() { runArgs("import", archive) })
	if !strings.Contains(out, "already imported") {
		t.Fatalf("re-import output = %q", out)
	}
	assertImported(2)
}

// TestExportBrokenSnapshotRefusedCLI: a missing object file must refuse the
// export at the CLI gate, same message shape rollback uses.
func TestExportBrokenSnapshotRefusedCLI(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DOCKERVC_HOME", dir)
	storePath = filepath.Join(dir, "src")
	buildStoreSnapshot(t, "snap-20260912-130100-cli2", "broken")

	s, err := store.Open(storePath)
	if err != nil {
		t.Fatal(err)
	}
	rows, _ := s.ListSnapshots()
	m, _ := s.GetSnapshot(rows[0].ID)
	s.Close()
	// Break it: remove one referenced object file.
	if err := os.Remove(s.ObjectPath(m.ObjectHashes()[0])); err != nil {
		t.Fatal(err)
	}

	out := captureOutput(func() { runArgs("export", rows[0].ID, "-o", filepath.Join(dir, "x.dvca")) })
	if !strings.Contains(out, "broken") {
		t.Fatalf("broken-snapshot refusal missing: %q", out)
	}
	if _, err := os.Stat(filepath.Join(dir, "x.dvca")); err == nil {
		t.Fatal("refused export must not create the target")
	}
}

// TestImportRejectsTamperedArchiveCLI: a flipped byte inside an object entry
// must fail the checksum gate with a clean error and leave no snapshot row.
func TestImportRejectsTamperedArchiveCLI(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DOCKERVC_HOME", dir)
	storePath = filepath.Join(dir, "src")
	buildStoreSnapshot(t, "snap-20260912-130200-cli3", "tamper me")

	archive := filepath.Join(dir, "t.dvca")
	captureOutput(func() { runArgs("export", "snap-20260912-130200-cli3", "-o", archive) })

	// Flip a byte inside the FIRST object entry's data (name and checksums
	// line stay valid, so only content verification can catch it). A blind
	// byte-flip can land in tar padding and prove nothing.
	tamperFirstObject(t, archive)

	storePath = filepath.Join(dir, "dst")
	s2, err := store.Init(storePath)
	if err != nil {
		t.Fatal(err)
	}
	s2.Close() // the flock is exclusive even in-process; runArgs reopens
	out := captureOutput(func() { runArgs("import", archive) })
	if !strings.Contains(out, "not a valid .dvca archive") && !strings.Contains(out, "import failed") {
		t.Fatalf("tampered archive must fail cleanly, got %q", out)
	}
	s, err := store.Open(storePath)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if rows, _ := s.ListSnapshots(); len(rows) != 0 {
		t.Fatalf("tampered import must not register a snapshot: %d rows", len(rows))
	}
}

// tamperFirstObject XORs one byte of the first objects/<hash> member's data,
// rewriting the archive — deterministically inside object content, not tar
// padding.
func tamperFirstObject(t *testing.T, path string) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	type member struct {
		name string
		blob []byte
	}
	var members []member
	tr := tar.NewReader(f)
	for {
		hdr, err := tr.Next()
		if err != nil {
			break
		}
		blob, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		members = append(members, member{hdr.Name, blob})
	}
	f.Close()

	for i, m := range members {
		if strings.HasPrefix(m.name, "objects/") && len(m.blob) > 0 {
			members[i].blob = append([]byte(nil), m.blob...)
			members[i].blob[0] ^= 0xff
		}
	}
	out, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(out)
	for _, m := range members {
		if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: m.name, Size: int64(len(m.blob)), Mode: 0o640}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(m.blob); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	out.Close()
}

// TestResetCommandFlagsExportImport: the new option structs must be zeroed —
// flags leak between TUI runs (bit us before).
func TestResetCommandFlagsExportImport(t *testing.T) {
	exportOpts.out = "leak.dvca"
	exportOpts.latest = true
	exportOpts.split = "4g"
	importOpts.apply = true
	resetCommandFlags()
	if exportOpts != (struct {
		out    string
		latest bool
		split  string
		volume string
		image  string
		path   string
		raw    bool
	}{}) {
		t.Fatalf("exportOpts leaked: %+v", exportOpts)
	}
	if importOpts != (struct{ apply bool }{}) {
		t.Fatalf("importOpts leaked: %+v", importOpts)
	}
}
