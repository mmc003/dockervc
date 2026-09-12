package snapshot

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"strings"
	"testing"

	"dockervc/internal/model"
)

// buildTar builds an in-memory tar from name → content. Directories are
// implied by entry paths (as with most volume tars).
func buildTar(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for name, content := range files {
		if err := tw.WriteHeader(&tar.Header{
			Name: name, Mode: 0o644, Size: int64(len(content)),
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestNewFileIndex(t *testing.T) {
	raw := buildTar(t, map[string]string{
		"./hello.txt":     "hello",
		"./sub/data.bin":  "\x00\x01\x02",
		"./sub/empty.txt": "",
	})
	idx, err := NewFileIndex(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("NewFileIndex: %v", err)
	}
	if len(idx.Files) != 3 {
		t.Fatalf("want 3 entries, got %d: %v", len(idx.Files), idx.Files)
	}
	h := idx.Files["hello.txt"] // "./" prefix must be cleaned away
	if h.Type != "file" || h.Size != 5 {
		t.Fatalf("hello.txt meta wrong: %+v", h)
	}
	sum := sha256.Sum256([]byte("hello"))
	if h.Hash != hex.EncodeToString(sum[:]) {
		t.Fatalf("content hash wrong: %s", h.Hash)
	}
	if _, ok := idx.Files["sub/data.bin"]; !ok {
		t.Fatalf("nested path missing: %v", idx.Files)
	}
	if e := idx.Files["sub/empty.txt"]; e.Size != 0 || e.Hash == "" {
		t.Fatalf("empty file should still be hashed: %+v", e)
	}
}

func TestVolumeIndexerPassthrough(t *testing.T) {
	raw := buildTar(t, map[string]string{"a": "aaa", "b": "bbb"})
	indexer := NewVolumeIndexer(bytes.NewReader(raw))
	var got bytes.Buffer
	if _, err := io.Copy(&got, indexer.Reader()); err != nil {
		t.Fatalf("copy through tee: %v", err)
	}
	if !bytes.Equal(got.Bytes(), raw) {
		t.Fatal("tee must pass the original bytes through unchanged (CAS hash depends on it)")
	}
	idx, err := indexer.Finish()
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if len(idx.Files) != 2 {
		t.Fatalf("want 2 indexed files, got %d", len(idx.Files))
	}
}

// Finish must return (not hang) when the consumer abandons the stream
// mid-read — the PutBlob-error path in captureVolumes.
func TestVolumeIndexerAbandonedStream(t *testing.T) {
	raw := buildTar(t, map[string]string{"a": strings.Repeat("x", 256*1024)})
	indexer := NewVolumeIndexer(bytes.NewReader(raw))
	buf := make([]byte, 1024)
	if _, err := indexer.Reader().Read(buf); err != nil {
		t.Fatalf("partial read: %v", err)
	}
	if _, err := indexer.Finish(); err == nil {
		t.Log("Finish returned a partial index without error")
	}
}

func TestDiffVolumeFiles(t *testing.T) {
	idxA, err := NewFileIndex(bytes.NewReader(buildTar(t, map[string]string{
		"keep.txt":  "same",
		"mod.txt":   "old",
		"gone.txt":  "bye",
		"chmod.txt": "c",
	})))
	if err != nil {
		t.Fatal(err)
	}
	// B: mod.txt rewritten, new.txt added, gone.txt deleted, chmod.txt same
	// content but mode 0600 — a metadata-only modification that must still
	// count as modified. keep.txt identical in every indexed respect.
	var tarB bytes.Buffer
	tw := tar.NewWriter(&tarB)
	write := func(name, content string, mode int64) {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: mode, Size: int64(len(content))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	write("keep.txt", "same", 0o644)
	write("mod.txt", "new", 0o644)
	write("new.txt", "hi", 0o644)
	write("chmod.txt", "c", 0o600)
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	idxB, err := NewFileIndex(bytes.NewReader(tarB.Bytes()))
	if err != nil {
		t.Fatal(err)
	}

	// Both sides round-trip through the real storage format (JSON blobs).
	blobA, blobB := idxA.Reader(), idxB.Reader()
	var bufA, bufB bytes.Buffer
	if _, err := io.Copy(&bufA, blobA); err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(&bufB, blobB); err != nil {
		t.Fatal(err)
	}

	recA := model.VolumeRecord{Name: "v", IndexObject: "a"}
	recB := model.VolumeRecord{Name: "v", IndexObject: "b"}
	fc, err := DiffVolumeFiles(func(hash string) (io.ReadCloser, error) {
		if hash == "a" {
			return io.NopCloser(bytes.NewReader(bufA.Bytes())), nil
		}
		return io.NopCloser(bytes.NewReader(bufB.Bytes())), nil
	}, recA, recB)
	if err != nil {
		t.Fatalf("DiffVolumeFiles: %v", err)
	}
	if fc == nil {
		t.Fatal("expected a diff, got nil")
	}
	if len(fc.Created) != 1 || fc.Created[0] != "new.txt" {
		t.Fatalf("Created = %v", fc.Created)
	}
	if len(fc.Modified) != 2 || fc.Modified[0] != "chmod.txt" || fc.Modified[1] != "mod.txt" {
		t.Fatalf("Modified = %v (want chmod.txt + mod.txt)", fc.Modified)
	}
	if len(fc.Deleted) != 1 || fc.Deleted[0] != "gone.txt" {
		t.Fatalf("Deleted = %v", fc.Deleted)
	}
	if got := fc.Summarize(); got != "+1 created, ~2 modified, -1 deleted" {
		t.Fatalf("Summarize = %q", got)
	}
}

// A volume record without IndexObject (pre-indexing snapshot) yields a nil
// diff, not an error — DiffManifests falls back to "content changed".
func TestDiffVolumeFilesMissingIndex(t *testing.T) {
	a := model.VolumeRecord{Name: "v", Object: "h1"}
	b := model.VolumeRecord{Name: "v", Object: "h2", IndexObject: "idx"}
	fc, err := DiffVolumeFiles(func(string) (io.ReadCloser, error) {
		t.Fatal("opener must not be called for a missing index")
		return nil, nil
	}, a, b)
	if err != nil || fc != nil {
		t.Fatalf("want nil, nil; got %+v, %v", fc, err)
	}
}
