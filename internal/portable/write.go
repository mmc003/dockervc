// Package portable writes and reads .dvca archives: single-file, portable
// copies of one snapshot. The outer tar is uncompressed — the blobs inside
// are already zstd, so a second compression layer would cost CPU for ~0% size
// gain — and checksums.sha256 uses the GNU `sha256sum -c` line shape, so any
// machine can verify an unpacked archive with stock tools.
package portable

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"dockervc/internal/model"
	"dockervc/internal/progress"
	"dockervc/internal/store"
)

// Entry names inside the archive. checksums.sha256 is written LAST: it covers
// every other entry, and tar is append-only.
const (
	manifestEntry  = "manifest.json"
	checksumsEntry = "checksums.sha256"
	objectsPrefix  = "objects/"
)

// Exporter writes a manifest and its objects as a .dvca archive.
type Exporter struct {
	St *store.Store
	// Reporter receives byte-level progress for the deterministic object-copy
	// phase. Nil is silent.
	Reporter progress.Reporter
	// Progress is called after each object is written (nil = silent); the
	// caller decides how often to print. Kept for compatibility with existing
	// callers; new frontends should use Reporter.
	Progress func(done, total int, lastHash string, bytes int64)
}

// Export writes m as a .dvca archive to w. Refuses broken snapshots
// (referenced object files missing) before writing a byte — an archive of a
// broken snapshot would look complete while missing data. Entries:
// manifest.json first, then one objects/<hash> per referenced object (sorted
// and deduped, so two exports of the same snapshot are byte-identical),
// checksums.sha256 last. Objects are the ObjectPath FILE bytes, verbatim —
// OpenObject returns a decompressing reader and would silently change every
// hash.
func (e *Exporter) Export(m *model.Manifest, w io.Writer) (retErr error) {
	hashes := sortedObjectHashes(m)

	sizes := make(map[string]int64, len(hashes))
	for _, h := range hashes {
		fi, err := os.Stat(e.St.ObjectPath(h))
		if err != nil {
			return fmt.Errorf("snapshot %s is broken: object %s is missing — it cannot be exported (see `dockervc doctor`)", m.ID, h)
		}
		sizes[h] = fi.Size()
	}
	var totalBytes int64
	for _, size := range sizes {
		totalBytes += size
	}
	meter := progress.NewMeter(e.Reporter, "export")
	defer func() { meter.Finish(retErr != nil) }()

	// json.Marshal of the decoded manifest is byte-stable, so this is the
	// same blob db.InsertSnapshot stored — and the same bytes Register will
	// re-hash on the importing machine.
	manifestBlob, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("marshal manifest: %w", err)
	}
	manifestSum := sha256.Sum256(manifestBlob)

	tw := tar.NewWriter(w)
	hdr := func(name string, size int64) *tar.Header {
		return &tar.Header{
			Typeflag: tar.TypeReg,
			Name:     name,
			Size:     size,
			Mode:     0o640,
			ModTime:  m.CreatedAt, // fixed per snapshot → deterministic archives
		}
	}

	if err := tw.WriteHeader(hdr(manifestEntry, int64(len(manifestBlob)))); err != nil {
		return err
	}
	if _, err := tw.Write(manifestBlob); err != nil {
		return err
	}

	buf := make([]byte, 32*1024) // one buffer for all objects — none held whole
	meter.Phase("writing objects", "", totalBytes, progress.TotalExact, len(hashes))
	for i, h := range hashes {
		meter.Item(shortHash(h), i, len(hashes))
		if err := e.writeObject(tw, h, sizes[h], m.CreatedAt, buf, meter.AddBytes); err != nil {
			return err
		}
		meter.Item(shortHash(h), i+1, len(hashes))
		if e.Progress != nil {
			e.Progress(i+1, len(hashes), h, sizes[h])
		}
	}

	// GNU sha256sum-compatible "<hash>  <path>" lines (two spaces), so
	// `tar xf state.dvca && shasum -a 256 -c checksums.sha256` just works.
	// The parser also accepts the older "sha256 <hash>  <path>" shape.
	var cs strings.Builder
	fmt.Fprintf(&cs, "%s  %s\n", hex.EncodeToString(manifestSum[:]), manifestEntry)
	for _, h := range hashes {
		// The CAS invariant names the bytes: an object's checksum IS its
		// filename, so no re-hashing here (doctor --deep audits that
		// invariant on the local store).
		fmt.Fprintf(&cs, "%s  %s%s\n", h, objectsPrefix, h)
	}
	if err := tw.WriteHeader(hdr(checksumsEntry, int64(cs.Len()))); err != nil {
		return err
	}
	if _, err := tw.Write([]byte(cs.String())); err != nil {
		return err
	}

	meter.Phase("finalizing archive", "", 0, progress.TotalUnknown, 0)
	return tw.Close()
}

// writeObject streams one stored object file into the tar. The file — not
// OpenObject — is the source: stored (compressed) bytes are what the hash
// names.
func (e *Exporter) writeObject(tw *tar.Writer, hash string, size int64, modTime time.Time,
	buf []byte, advance func(int64)) error {
	f, err := os.Open(e.St.ObjectPath(hash))
	if err != nil {
		return err
	}
	defer f.Close()

	// The header size comes from the stat Export already did; a file that
	// changed size in between makes a corrupt entry, so re-stat and refuse.
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	if fi.Size() != size {
		return fmt.Errorf("object %s changed while exporting (%d → %d bytes)", hash, size, fi.Size())
	}

	if err := tw.WriteHeader(&tar.Header{
		Typeflag: tar.TypeReg,
		Name:     objectsPrefix + hash,
		Size:     size,
		Mode:     0o640,
		ModTime:  modTime,
	}); err != nil {
		return err
	}
	counted := &progress.CountingReader{Reader: f, Advance: advance}
	_, err = io.CopyBuffer(tw, counted, buf)
	return err
}

func shortHash(hash string) string {
	if len(hash) <= 12 {
		return hash
	}
	return hash[:12]
}

// sortedObjectHashes returns the manifest's referenced hashes sorted and
// deduped — deterministic archive layout, one entry per unique blob (two
// volumes with identical content share a hash; the reader rejects duplicate
// entry names).
func sortedObjectHashes(m *model.Manifest) []string {
	hashes := m.ObjectHashes()
	sort.Strings(hashes)
	out := hashes[:0]
	var prev string
	for _, h := range hashes {
		if h != prev {
			out = append(out, h)
			prev = h
		}
	}
	return out
}
