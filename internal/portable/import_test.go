package portable

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"dockervc/internal/model"
	"dockervc/internal/store"
)

// newImportStore opens a second, empty store — the "another machine" side of
// the round trip.
func newImportStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Init(filepath.Join(t.TempDir(), "other"))
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// importInto runs the full three-pass import over path into s.
func importInto(t *testing.T, s *store.Store, path string) (*Index, int, int, error) {
	t.Helper()
	ix, err := IndexArchive(path)
	if err != nil {
		return nil, 0, 0, err
	}
	adopted, present, err := Adopt(s, ix, nil)
	if err != nil {
		return ix, adopted, present, err
	}
	return ix, adopted, present, Register(s, ix)
}

// TestImportRoundTripFreshStore: export → import into a fresh store must
// reproduce the snapshot row, every object (hash, kind, size, bytes) and the
// manifest hash exactly — the cross-machine collision semantics.
func TestImportRoundTripFreshStore(t *testing.T) {
	src, m := newFixtureStore(t)
	path := exportFixture(t, src, m)
	dst := newImportStore(t)

	ix, adopted, present, err := importInto(t, dst, path)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if adopted != 3 || present != 0 {
		t.Fatalf("adopted=%d present=%d, want 3/0", adopted, present)
	}

	rows, err := dst.ListSnapshots()
	if err != nil || len(rows) != 1 || rows[0].ID != m.ID {
		t.Fatalf("snapshots after import = %+v (err %v)", rows, err)
	}
	got, err := dst.GetSnapshot(m.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, m) {
		t.Fatalf("imported manifest differs:\n got %+v\nwant %+v", got, m)
	}
	wantHash, _ := m.Hash()
	if ix.ManifestHash != wantHash {
		t.Fatalf("index manifest hash %s != canonical %s", ix.ManifestHash, wantHash)
	}

	// Objects: same hashes, same kinds, same sizes; files byte-identical.
	srcObjs, _ := src.AllObjects()
	dstObjs, _ := dst.AllObjects()
	byHash := func(objs []model.ObjectInfo) map[string]model.ObjectInfo {
		out := map[string]model.ObjectInfo{}
		for _, o := range objs {
			out[o.Hash] = o
		}
		return out
	}
	srcBy, dstBy := byHash(srcObjs), byHash(dstObjs)
	if len(srcBy) != 3 || len(dstBy) != 3 {
		t.Fatalf("objects: src=%d dst=%d, want 3/3", len(srcBy), len(dstBy))
	}
	for h, so := range srcBy {
		do, ok := dstBy[h]
		if !ok {
			t.Fatalf("object %s missing after import", h)
		}
		if so.Size != do.Size || so.Kind != do.Kind {
			t.Fatalf("object %s: got kind=%s size=%d, want kind=%s size=%d", h, do.Kind, do.Size, so.Kind, so.Size)
		}
		a, _ := os.ReadFile(src.ObjectPath(h))
		b, _ := os.ReadFile(dst.ObjectPath(h))
		if string(a) != string(b) {
			t.Fatalf("object %s bytes differ after import", h)
		}
	}

	// Kinds were derived per HANDOFF §5.
	vol := m.Volumes[0]
	if ix.Kinds[m.Images[0].Object] != "image" || ix.Kinds[vol.Object] != "volume" || ix.Kinds[vol.IndexObject] != "volindex" {
		t.Fatalf("kinds = %v", ix.Kinds)
	}
}

// TestImportTwiceIsIdempotent: the second import adopts nothing, registers
// nothing, and says "already imported" — one row remains.
func TestImportTwiceIsIdempotent(t *testing.T) {
	src, m := newFixtureStore(t)
	path := exportFixture(t, src, m)
	dst := newImportStore(t)

	if _, _, _, err := importInto(t, dst, path); err != nil {
		t.Fatalf("first import: %v", err)
	}
	_, adopted, present, err := importInto(t, dst, path)
	if !errors.Is(err, ErrAlreadyImported) {
		t.Fatalf("second import err = %v, want ErrAlreadyImported", err)
	}
	if adopted != 0 || present != 3 {
		t.Fatalf("second import adopted=%d present=%d, want 0/3", adopted, present)
	}
	if rows, _ := dst.ListSnapshots(); len(rows) != 1 {
		t.Fatalf("idempotent import must not add rows: %d", len(rows))
	}
}

// TestRegisterRejectsSameIDDifferentContent: a locally-edited manifest with
// an updated checksums file (so the archive itself verifies) still collides
// on the id — a hard error, not a silent overwrite.
func TestRegisterRejectsSameIDDifferentContent(t *testing.T) {
	src, m := newFixtureStore(t)
	path := exportFixture(t, src, m)
	dst := newImportStore(t)
	if _, _, _, err := importInto(t, dst, path); err != nil {
		t.Fatal(err)
	}

	// Rebuild the archive with an edited manifest and a fixed-up checksums
	// line — everything self-consistent except it clashes with the local id.
	edited := *m
	edited.Message = "tampered"
	rebuildArchive(t, path, map[string][]byte{manifestEntry: mustJSON(t, &edited)}, true)

	_, _, _, err := importInto(t, dst, path)
	if err == nil || errors.Is(err, ErrAlreadyImported) {
		t.Fatalf("same id different content must be a hard error, got %v", err)
	}
	if !strings.Contains(err.Error(), "DIFFERENT content") {
		t.Fatalf("error should point at delete/--store: %v", err)
	}
	if rows, _ := dst.ListSnapshots(); len(rows) != 1 {
		t.Fatalf("collision must not add rows")
	}
}

// TestIndexArchiveRejectsMalformedArchives walks the rejection gates: every
// case builds a variant of a good archive and expects a clean, named error.
func TestIndexArchiveRejectsMalformedArchives(t *testing.T) {
	src, m := newFixtureStore(t)
	good := exportFixture(t, src, m)
	edited := *m
	edited.Message = "edited"

	cases := []struct {
		name    string
		mutate  func(t *testing.T, path string)
		wantErr string
	}{
		{
			name:    "unexpected entry",
			mutate:  func(t *testing.T, p string) { addEntry(t, p, "evil.txt", []byte("nope")) },
			wantErr: `unexpected entry "evil.txt"`,
		},
		{
			name:    "duplicate object entry",
			mutate:  func(t *testing.T, p string) { dupFirstObject(t, p) },
			wantErr: "duplicate entry",
		},
		{
			name:    "non-hex object name",
			mutate:  func(t *testing.T, p string) { renameEntry(t, p, objectsPrefix+strings.Repeat("z", 64)) },
			wantErr: "not hex",
		},
		{
			name:    "wrong-length object name",
			mutate:  func(t *testing.T, p string) { renameEntry(t, p, objectsPrefix+"abcd") },
			wantErr: "64-char",
		},
		{
			name:    "missing checksums",
			mutate:  func(t *testing.T, p string) { dropEntry(t, p, checksumsEntry) },
			wantErr: "no checksums.sha256",
		},
		{
			name:    "missing manifest",
			mutate:  func(t *testing.T, p string) { dropEntry(t, p, manifestEntry) },
			wantErr: "no manifest.json",
		},
		{
			// A valid, self-consistent JSON manifest whose bytes differ from
			// what checksums.sha256 recorded — only the canonical re-hash
			// catches it.
			name: "manifest re-serialized, checksums line stale",
			mutate: func(t *testing.T, p string) {
				rebuildArchive(t, p, map[string][]byte{manifestEntry: mustJSON(t, &edited)}, false)
			},
			wantErr: "does not match manifest.json",
		},
		{
			// Cut mid-header-block (200 bytes in), so the tar reader itself
			// errors rather than seeing a clean end-of-archive.
			name: "truncated archive",
			mutate: func(t *testing.T, p string) {
				b, _ := os.ReadFile(p)
				os.WriteFile(p, b[:200], 0o640)
			},
			wantErr: "read archive",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "variant.dvca")
			b, err := os.ReadFile(good)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, b, 0o640); err != nil {
				t.Fatal(err)
			}
			tc.mutate(t, path)
			_, err = IndexArchive(path)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

// TestAdoptDetectsTamperedObjectBytes: flipping bytes inside an objects
// entry (name and checksums line untouched, so indexing passes) must fail in
// Adopt naming the entry — and leave no snapshot row behind, with any
// already-adopted objects present-but-unreferenced (an orphan, not
// corruption).
func TestAdoptDetectsTamperedObjectBytes(t *testing.T) {
	src, m := newFixtureStore(t)
	path := exportFixture(t, src, m)
	dst := newImportStore(t)

	hashes := sortedObjectHashes(m)
	victim := hashes[len(hashes)-1] // tamper the LAST object: earlier ones adopt first
	tamperEntry(t, path, objectsPrefix+victim)

	ix, err := IndexArchive(path)
	if err != nil {
		t.Fatalf("index should pass (only bytes changed): %v", err)
	}
	_, _, err = Adopt(dst, ix, nil)
	if err == nil || !strings.Contains(err.Error(), objectsPrefix+victim) {
		t.Fatalf("err = %v, want it to name %s", err, objectsPrefix+victim)
	}
	if rows, _ := dst.ListSnapshots(); len(rows) != 0 {
		t.Fatalf("no snapshot row may land on a failed import")
	}
	if unref, _ := dst.UnreferencedObjects(); len(unref) == 0 {
		t.Fatal("objects adopted before the failure should remain (unreferenced)")
	}
}

// TestAdoptPreflightMissingObject: an archive lacking a referenced object
// (entry and checksums line both gone, so indexing passes) is refused before
// anything is written.
func TestAdoptPreflightMissingObject(t *testing.T) {
	src, m := newFixtureStore(t)
	path := exportFixture(t, src, m)
	hashes := sortedObjectHashes(m)
	dropObject(t, path, hashes[0])

	ix, err := IndexArchive(path)
	if err != nil {
		t.Fatalf("index should pass: %v", err)
	}
	dst := newImportStore(t)
	_, _, err = Adopt(dst, ix, nil)
	if err == nil || !strings.Contains(err.Error(), hashes[0]) {
		t.Fatalf("err = %v, want it to name %s", err, hashes[0])
	}
	if objs, _ := dst.AllObjects(); len(objs) != 0 {
		t.Fatalf("pre-flight refusal must write nothing, got %d objects", len(objs))
	}
}

// TestParseChecksumsBothShapes: the reader accepts the GNU line shape it
// writes plus the older "sha256 <hash>  <path>" shape and GNU's binary "*"
// marker.
func TestParseChecksumsBothShapes(t *testing.T) {
	h := strings.Repeat("ab", 32)
	for _, line := range []string{
		h + "  manifest.json\n",
		"sha256 " + h + "  manifest.json\n",
		h + " *manifest.json\n",
	} {
		got, err := parseChecksums(line)
		if err != nil {
			t.Fatalf("parseChecksums(%q): %v", line, err)
		}
		if got[manifestEntry] != h {
			t.Fatalf("parsed %v from %q", got, line)
		}
	}
	for _, bad := range []string{
		"nonsense\n",
		h[:32] + "  manifest.json\n",       // short hash
		h + "  a\n" + h + "  a\n",          // duplicate path
		"zz" + h[2:] + "  manifest.json\n", // non-hex
	} {
		if _, err := parseChecksums(bad); err == nil {
			t.Fatalf("parseChecksums(%q) should fail", bad)
		}
	}
}

// ── archive rebuild helpers ───────────────────────────────────────────────────

// tarEntry is one member while rebuilding a variant archive.
type tarEntry struct {
	name string
	blob []byte
}

// loadEntries reads an archive into ordered entries.
func loadEntries(t *testing.T, path string) []tarEntry {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []tarEntry
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
		out = append(out, tarEntry{hdr.Name, blob})
	}
	return out
}

// writeEntries writes entries as an uncompressed tar to path.
func writeEntries(t *testing.T, path string, entries []tarEntry) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	tw := tar.NewWriter(f)
	for _, e := range entries {
		if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: e.name, Size: int64(len(e.blob)), Mode: 0o640}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(e.blob); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
}

// rebuildArchive applies replacements (name → new bytes) and optionally
// fixes up the manifest.json checksums line to match the replaced manifest
// (simulating a self-consistent but edited archive).
func rebuildArchive(t *testing.T, path string, replace map[string][]byte, fixManifestLine bool) {
	t.Helper()
	entries := loadEntries(t, path)
	for i := range entries {
		if blob, ok := replace[entries[i].name]; ok {
			entries[i].blob = blob
		}
	}
	if fixManifestLine {
		sum := sha256.Sum256(replace[manifestEntry])
		for i := range entries {
			if entries[i].name != checksumsEntry {
				continue
			}
			lines := strings.Split(string(entries[i].blob), "\n")
			for j, l := range lines {
				if strings.HasSuffix(l, manifestEntry) {
					shape := ""
					if fields := strings.Fields(l); len(fields) > 2 && fields[0] == "sha256" {
						shape = "sha256 "
					}
					lines[j] = shape + hex.EncodeToString(sum[:]) + "  " + manifestEntry
				}
			}
			entries[i].blob = []byte(strings.Join(lines, "\n"))
		}
	}
	writeEntries(t, path, entries)
}

// addEntry appends one extra member.
func addEntry(t *testing.T, path, name string, blob []byte) {
	entries := loadEntries(t, path)
	writeEntries(t, path, append(entries, tarEntry{name, blob}))
}

// dropEntry removes one member by name.
func dropEntry(t *testing.T, path, name string) {
	var kept []tarEntry
	for _, e := range loadEntries(t, path) {
		if e.name != name {
			kept = append(kept, e)
		}
	}
	writeEntries(t, path, kept)
}

// dropObject removes an object entry AND its checksums line, so the archive
// stays internally consistent while missing data.
func dropObject(t *testing.T, path, hash string) {
	dropEntry(t, path, objectsPrefix+hash)
	var fixed []byte
	for _, e := range loadEntries(t, path) {
		if e.name == checksumsEntry {
			var lines []string
			for _, l := range strings.Split(string(e.blob), "\n") {
				if !strings.HasSuffix(l, objectsPrefix+hash) {
					lines = append(lines, l)
				}
			}
			fixed = []byte(strings.Join(lines, "\n"))
			break
		}
	}
	if fixed == nil {
		t.Fatal("no checksums entry found")
	}
	rebuildArchive(t, path, map[string][]byte{checksumsEntry: fixed}, false)
}

// dupFirstObject appends a copy of the first objects/ entry.
func dupFirstObject(t *testing.T, path string) {
	entries := loadEntries(t, path)
	for _, e := range entries {
		if strings.HasPrefix(e.name, objectsPrefix) {
			writeEntries(t, path, append(entries, e))
			return
		}
	}
	t.Fatal("no object entry to duplicate")
}

// renameEntry renames the first object entry to newName, keeping its bytes.
func renameEntry(t *testing.T, path, newName string) {
	entries := loadEntries(t, path)
	for i := range entries {
		if strings.HasPrefix(entries[i].name, objectsPrefix) {
			entries[i].name = newName
			writeEntries(t, path, entries)
			return
		}
	}
	t.Fatal("no object entry to rename")
}

// tamperEntry flips the middle byte of one member's content — name and size
// stay, so only content verification can catch it.
func tamperEntry(t *testing.T, path, name string) {
	for _, e := range loadEntries(t, path) {
		if e.name == name {
			blob := append([]byte(nil), e.blob...)
			blob[len(blob)/2] ^= 0xff
			rebuildArchive(t, path, map[string][]byte{name: blob}, false)
			return
		}
	}
	t.Fatalf("entry %s not found", name)
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	blob, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return blob
}
