package portable

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"dockervc/internal/model"
	"dockervc/internal/progress"
)

const maxPeekManifest = 32 << 20 // manifests are metadata; reject absurd allocations

// PeekArchive reads only the leading manifest. Export always writes
// manifest.json first, making archive listing and inspection independent of
// the potentially multi-gigabyte object payload that follows it.
func PeekArchive(path string) (*model.Manifest, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	tr := tar.NewReader(f)
	hdr, err := tr.Next()
	if err != nil {
		return nil, fmt.Errorf("read archive header: %w", err)
	}
	if hdr.Name != manifestEntry || hdr.Typeflag != tar.TypeReg {
		return nil, fmt.Errorf("first entry is %q, want %s", hdr.Name, manifestEntry)
	}
	if hdr.Size < 0 || hdr.Size > maxPeekManifest {
		return nil, fmt.Errorf("%s has unreasonable size %d", manifestEntry, hdr.Size)
	}
	blob, err := io.ReadAll(io.LimitReader(tr, hdr.Size))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", manifestEntry, err)
	}
	var m model.Manifest
	if err := json.Unmarshal(blob, &m); err != nil {
		return nil, fmt.Errorf("%s is not valid: %w", manifestEntry, err)
	}
	if m.ID == "" {
		return nil, fmt.Errorf("%s has no snapshot id", manifestEntry)
	}
	return &m, nil
}

// VerifyArchive validates archive structure and hashes every payload entry.
// It does not touch the object store or register the snapshot.
func VerifyArchive(path string, reporter progress.Reporter) (ix *Index, retErr error) {
	meter := progress.NewMeter(reporter, "archive verify")
	defer func() { meter.Finish(retErr != nil) }()
	archiveSize := int64(0)
	if fi, err := os.Stat(path); err == nil {
		archiveSize = fi.Size()
	}
	meter.Phase("checking archive structure", filepath.Base(path), archiveSize, progress.TotalExact, 0)

	ix, err := indexArchive(path, meter.AddBytes)
	if err != nil {
		return nil, err
	}
	if archiveSize > 0 {
		meter.SetBytes(archiveSize, archiveSize, progress.TotalExact)
	}

	need := sortedObjectHashes(ix.Manifest)
	present := make(map[string]bool, len(need))
	for _, entry := range ix.Entries {
		if len(entry.Name) > len(objectsPrefix) && entry.Name[:len(objectsPrefix)] == objectsPrefix {
			present[entry.Name[len(objectsPrefix):]] = true
		}
	}
	for _, hash := range need {
		if !present[hash] {
			return nil, fmt.Errorf("archive is missing referenced object %s", hash)
		}
	}

	var total int64
	items := 0
	for _, entry := range ix.Entries {
		if entry.Name != checksumsEntry {
			total += entry.Size
			items++
		}
	}
	meter.Phase("verifying archive", filepath.Base(path), total, progress.TotalExact, items)

	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	seen := make(map[string]bool, items)
	tr := tar.NewReader(f)
	done := 0
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read archive: %w", err)
		}
		if hdr.Name == checksumsEntry {
			continue
		}
		want, ok := ix.Checksums[hdr.Name]
		if !ok {
			return nil, fmt.Errorf("%s has no checksum", hdr.Name)
		}
		meter.Item(hdr.Name, done, items)
		h := sha256.New()
		counted := &progress.CountingReader{Reader: tr, Advance: meter.AddBytes}
		if _, err := io.Copy(h, counted); err != nil {
			return nil, fmt.Errorf("read %s: %w", hdr.Name, err)
		}
		got := hex.EncodeToString(h.Sum(nil))
		if got != want {
			return nil, fmt.Errorf("%s checksum mismatch: got %s, want %s", hdr.Name, got, want)
		}
		seen[hdr.Name] = true
		done++
		meter.Item(hdr.Name, done, items)
	}
	for name := range ix.Checksums {
		if name != checksumsEntry && !seen[name] {
			return nil, fmt.Errorf("checksum names missing entry %s", name)
		}
	}
	return ix, nil
}

// WithObject finds one compressed CAS object inside an archive, authenticates
// its bytes against the object hash, and exposes its raw stored stream to fn.
// The callback must decode the object if it needs its logical contents.
func WithObject(path, hash string, fn func(io.Reader) error) error {
	if err := validHashName(hash); err != nil {
		return err
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	name := objectsPrefix + hash
	tr := tar.NewReader(f)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return fmt.Errorf("archive has no object %s", hash)
		}
		if err != nil {
			return fmt.Errorf("read archive: %w", err)
		}
		if hdr.Name != name {
			continue
		}
		h := sha256.New()
		authenticated := io.TeeReader(tr, h)
		if err := fn(authenticated); err != nil {
			return err
		}
		// A decoder may stop after its logical value while retaining compressed
		// bytes in an internal buffer. Drain the tar entry before comparing.
		if _, err := io.Copy(io.Discard, authenticated); err != nil {
			return fmt.Errorf("read object %s: %w", hash, err)
		}
		if got := hex.EncodeToString(h.Sum(nil)); got != hash {
			return fmt.Errorf("object %s checksum mismatch (got %s)", hash, got)
		}
		return nil
	}
}
