package portable

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"dockervc/internal/model"
	"dockervc/internal/store"
)

// newFixtureStore builds a real mini-store: two content objects, a volume
// file index, and one snapshot referencing them all — the shapes capture
// produces, minus the engine.
func newFixtureStore(t *testing.T) (*store.Store, *model.Manifest) {
	t.Helper()
	s, err := store.Init(filepath.Join(t.TempDir(), "store"))
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	vol, err := s.PutBlob("volume", "", strings.NewReader("volume tar bytes"))
	if err != nil {
		t.Fatal(err)
	}
	idx, err := s.PutBlob("volindex", "", strings.NewReader(`{"files":{}}`))
	if err != nil {
		t.Fatal(err)
	}
	img, err := s.PutBlob("image", "sha256:demo", strings.NewReader("image tar bytes"))
	if err != nil {
		t.Fatal(err)
	}

	m := &model.Manifest{
		ID:        "snap-20260912-120000-test",
		CreatedAt: time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC),
		Message:   "export fixture",
		Volumes:   []model.VolumeRecord{{Name: "demo-data", Object: vol.Hash, IndexObject: idx.Hash, Size: vol.Size}},
		Images:    []model.ImageRecord{{Refs: []string{"nginx:alpine"}, Digest: "sha256:demo", Object: img.Hash, Size: img.Size}},
	}
	if err := s.InsertSnapshot(m); err != nil {
		t.Fatalf("InsertSnapshot: %v", err)
	}
	return s, m
}

// exportFixture writes m to a temp .dvca and returns its path.
func exportFixture(t *testing.T, s *store.Store, m *model.Manifest) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.dvca")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := (&Exporter{St: s}).Export(m, f); err != nil {
		f.Close()
		t.Fatalf("Export: %v", err)
	}
	f.Close()
	return path
}

// readTarEntries unpacks an archive into name→bytes, in file order.
func readTarEntries(t *testing.T, path string) ([]Entry, map[string][]byte) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var order []Entry
	content := map[string][]byte{}
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
		order = append(order, Entry{Name: hdr.Name, Size: hdr.Size})
		content[hdr.Name] = blob
	}
	return order, content
}

// TestExportLayout asserts the binding .dvca shape: manifest.json first,
// objects/<hash> verbatim and sorted, checksums.sha256 last, and GNU-shaped
// checksum lines.
func TestExportLayout(t *testing.T) {
	s, m := newFixtureStore(t)
	path := exportFixture(t, s, m)
	order, content := readTarEntries(t, path)

	wantHashes := sortedObjectHashes(m)
	wantNames := []string{manifestEntry}
	for _, h := range wantHashes {
		wantNames = append(wantNames, objectsPrefix+h)
	}
	wantNames = append(wantNames, checksumsEntry)
	var gotNames []string
	for _, e := range order {
		gotNames = append(gotNames, e.Name)
	}
	if !reflect.DeepEqual(gotNames, wantNames) {
		t.Fatalf("entry order = %v, want %v", gotNames, wantNames)
	}

	// manifest.json is the canonical re-marshal — byte-identical to what the
	// db stores, which is what Register re-hashes on the far side.
	canonical, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(content[manifestEntry], canonical) {
		t.Fatalf("manifest.json is not the canonical encoding")
	}

	// Objects are the FILE bytes verbatim — not OpenObject's decompressed
	// stream (which would change every hash).
	for _, h := range wantHashes {
		onDisk, err := os.ReadFile(s.ObjectPath(h))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(content[objectsPrefix+h], onDisk) {
			t.Fatalf("objects/%s is not the stored file verbatim", h)
		}
	}

	// Checksums: one GNU line per entry, two spaces, manifest first.
	manifestHash, err := m.Hash()
	if err != nil {
		t.Fatal(err)
	}
	var want strings.Builder
	want.WriteString(manifestHash + "  " + manifestEntry + "\n")
	for _, h := range wantHashes {
		want.WriteString(h + "  " + objectsPrefix + h + "\n")
	}
	if got := string(content[checksumsEntry]); got != want.String() {
		t.Fatalf("checksums.sha256 =\n%q\nwant\n%q", got, want.String())
	}
}

// TestExportIsDeterministic: two exports of the same snapshot are
// byte-identical (sorted objects, fixed mtimes) — a re-export can resume a
// interrupted transfer without confusing anything downstream.
func TestExportIsDeterministic(t *testing.T) {
	s, m := newFixtureStore(t)
	a, err := os.ReadFile(exportFixture(t, s, m))
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(exportFixture(t, s, m))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) {
		t.Fatal("two exports of the same snapshot differ")
	}
}

// TestExportRefusesBrokenSnapshot: a snapshot with a missing object file
// must be refused before a byte is written.
func TestExportRefusesBrokenSnapshot(t *testing.T) {
	s, m := newFixtureStore(t)
	hashes := sortedObjectHashes(m)
	if err := os.Remove(s.ObjectPath(hashes[0])); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := (&Exporter{St: s}).Export(m, &out); err == nil {
		t.Fatal("export of a broken snapshot must fail")
	} else if !strings.Contains(err.Error(), "broken") {
		t.Fatalf("error should name the problem: %v", err)
	}
	if out.Len() != 0 {
		t.Fatal("a refused export must not write bytes")
	}
}

// TestExportProgressReportsObjects: the callback fires once per object, in
// order, with a final done==total.
func TestExportProgressReportsObjects(t *testing.T) {
	s, m := newFixtureStore(t)
	var calls []int
	var lastHashes []string
	(&Exporter{St: s, Progress: func(done, total int, hash string, bytes int64) {
		calls = append(calls, done)
		lastHashes = append(lastHashes, hash)
	}}).Export(m, &bytes.Buffer{})

	want := sortedObjectHashes(m)
	if len(calls) != len(want) || calls[len(calls)-1] != len(want) {
		t.Fatalf("progress calls = %v, want one per object ending at %d", calls, len(want))
	}
	for i, h := range want {
		if lastHashes[i] != h {
			t.Fatalf("progress order: hash %d = %s, want %s", i, lastHashes[i], h)
		}
	}
}
