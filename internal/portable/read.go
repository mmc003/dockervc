package portable

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"dockervc/internal/model"
	"dockervc/internal/progress"
	"dockervc/internal/store"
)

// ErrAlreadyImported is returned by Register when the snapshot id is already
// in the store with the identical manifest hash — a no-op success, distinct
// from a hard collision (same id, different content), which is an error.
var ErrAlreadyImported = errors.New("snapshot already imported unchanged")

// Entry is one archive member.
type Entry struct {
	Name string // "manifest.json" | "objects/<64-hex>" | "checksums.sha256"
	Size int64
}

// Index is the result of the first import pass: the archive's table of
// contents plus the decoded manifest, produced without touching the store —
// so import can show exactly what it is about to do before writing a byte.
type Index struct {
	Path         string // archive file the index was read from (Adopt re-opens it)
	Entries      []Entry
	Manifest     *model.Manifest
	ManifestHash string            // canonical re-hash, the same value db.InsertSnapshot stores
	Checksums    map[string]string // entry name → sha256, parsed leniently
	Kinds        map[string]string // object hash → kind: image|volume|volindex|bindmount
}

// entrySize returns the advertised size of one entry name (0 when absent).
func (ix *Index) entrySize(name string) int64 {
	for _, e := range ix.Entries {
		if e.Name == name {
			return e.Size
		}
	}
	return 0
}

// ObjectCount returns how many unique objects the manifest references (the
// set import will adopt). Duplicate references in the manifest collapse.
func (ix *Index) ObjectCount() int {
	return len(sortedObjectHashes(ix.Manifest))
}

// ObjectSize returns the advertised size of one object's entry (0 when the
// archive lacks it — pre-flight Adopt will refuse such archives anyway).
func (ix *Index) ObjectSize(hash string) int64 {
	return ix.entrySize(objectsPrefix + hash)
}

// IndexArchive reads the archive's structure and manifest (pass 1 of import).
// It rejects anything that is not a well-formed .dvca: non-regular entries,
// duplicate names, malformed object names, missing manifest.json or
// checksums.sha256, undecodable manifests, and checksums that disagree with
// the manifest's canonical hash or the entry names.
func IndexArchive(path string) (*Index, error) {
	return indexArchive(path, nil)
}

// indexArchive is IndexArchive with an optional raw archive-byte callback.
// Verification uses it to keep the structural pass visible on large files.
func indexArchive(path string, advance func(int64)) (*Index, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	ix := &Index{Path: path, Checksums: map[string]string{}}
	var manifestBlob, checksumsBlob []byte
	seen := map[string]bool{}
	var source io.Reader = f
	if advance != nil {
		source = &progress.CountingReader{Reader: f, Advance: advance}
	}
	tr := tar.NewReader(source)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read archive: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			return nil, fmt.Errorf("entry %s: not a regular file", hdr.Name)
		}
		if seen[hdr.Name] {
			return nil, fmt.Errorf("duplicate entry %s", hdr.Name)
		}
		seen[hdr.Name] = true

		switch {
		case hdr.Name == manifestEntry:
			if manifestBlob, err = io.ReadAll(tr); err != nil {
				return nil, fmt.Errorf("read %s: %w", manifestEntry, err)
			}
		case hdr.Name == checksumsEntry:
			if checksumsBlob, err = io.ReadAll(tr); err != nil {
				return nil, fmt.Errorf("read %s: %w", checksumsEntry, err)
			}
		case strings.HasPrefix(hdr.Name, objectsPrefix):
			hash := strings.TrimPrefix(hdr.Name, objectsPrefix)
			if err := validHashName(hash); err != nil {
				return nil, fmt.Errorf("entry %s: %w", hdr.Name, err)
			}
		default:
			return nil, fmt.Errorf("unexpected entry %q", hdr.Name)
		}
		ix.Entries = append(ix.Entries, Entry{Name: hdr.Name, Size: hdr.Size})
	}

	if manifestBlob == nil {
		return nil, fmt.Errorf("no %s in archive", manifestEntry)
	}
	if checksumsBlob == nil {
		return nil, fmt.Errorf("no %s in archive", checksumsEntry)
	}

	var m model.Manifest
	if err := json.Unmarshal(manifestBlob, &m); err != nil {
		return nil, fmt.Errorf("%s is not a valid manifest: %w", manifestEntry, err)
	}
	ix.Manifest = &m

	// Canonical re-hash (json.Marshal → sha256) — the exact computation
	// db.InsertSnapshot used for manifest_hash, so an id imported on another
	// machine collides (or dedups) by content, not by luck.
	canon, err := json.Marshal(&m)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(canon)
	ix.ManifestHash = hex.EncodeToString(sum[:])

	checksums, err := parseChecksums(string(checksumsBlob))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", checksumsEntry, err)
	}
	ix.Checksums = checksums

	// The manifest line is the tamper gate for manifest.json: edited bytes
	// either fail to decode above or re-hash to something else here.
	if got, ok := checksums[manifestEntry]; !ok || got != ix.ManifestHash {
		return nil, fmt.Errorf("%s does not match %s — archive tampered or truncated", checksumsEntry, manifestEntry)
	}
	// Every object entry needs a line, and it must agree with the name (the
	// name is what AdoptObject will verify the bytes against).
	for _, e := range ix.Entries {
		if !strings.HasPrefix(e.Name, objectsPrefix) {
			continue
		}
		got, ok := checksums[e.Name]
		if !ok {
			return nil, fmt.Errorf("%s has no line in %s", e.Name, checksumsEntry)
		}
		if want := strings.TrimPrefix(e.Name, objectsPrefix); got != want {
			return nil, fmt.Errorf("%s disagrees with entry name %s", checksumsEntry, e.Name)
		}
	}

	ix.Kinds = objectKinds(&m)
	return ix, nil
}

// validHashName enforces the objects/<hash> shape: 64 lowercase hex chars.
func validHashName(hash string) error {
	if len(hash) != 64 {
		return fmt.Errorf("object name is not a 64-char sha256")
	}
	if _, err := hex.DecodeString(hash); err != nil || hash != strings.ToLower(hash) {
		return fmt.Errorf("object name is not hex")
	}
	return nil
}

// parseChecksums parses the checksum file leniently: both the GNU shape
// "<hash>  <path>" and the older "sha256 <hash>  <path>" are accepted, as is
// GNU's binary-mode "<hash> *<path>" marker.
func parseChecksums(data string) (map[string]string, error) {
	out := map[string]string{}
	for _, line := range strings.Split(data, "\n") {
		line = strings.TrimRight(line, "\r")
		if line = strings.TrimSpace(line); line == "" {
			continue
		}
		fields := strings.Fields(line)
		if fields[0] == "sha256" { // old "sha256 <hash>  <path>" shape
			fields = fields[1:]
		}
		if len(fields) < 2 || len(fields[0]) != 64 {
			return nil, fmt.Errorf("unrecognized line %q", line)
		}
		hash, path := fields[0], strings.TrimPrefix(fields[1], "*")
		if _, err := hex.DecodeString(hash); err != nil {
			return nil, fmt.Errorf("unrecognized line %q", line)
		}
		if _, dup := out[path]; dup {
			return nil, fmt.Errorf("duplicate line for %s", path)
		}
		out[path] = hash
	}
	return out, nil
}

// objectKinds maps each referenced object hash to its kind — the same kinds
// capture registered, so a migrated store's rows are indistinguishable from
// native ones (HANDOFF §5): images + container filesystems → image, volume
// content → volume, volume file indexes → volindex, bind mounts → bindmount.
func objectKinds(m *model.Manifest) map[string]string {
	kinds := map[string]string{}
	for i := range m.Images {
		if h := m.Images[i].Object; h != "" {
			kinds[h] = "image"
		}
	}
	for i := range m.Containers {
		if h := m.Containers[i].ImageObject; h != "" {
			kinds[h] = "image"
		}
	}
	for i := range m.Volumes {
		if h := m.Volumes[i].Object; h != "" {
			kinds[h] = "volume"
		}
		if h := m.Volumes[i].IndexObject; h != "" {
			kinds[h] = "volindex"
		}
	}
	for i := range m.BindMounts {
		if h := m.BindMounts[i].Object; h != "" {
			kinds[h] = "bindmount"
		}
	}
	return kinds
}

// Adopt verifies and places every object the manifest references into the
// store (pass 2 of import): streaming, never buffering an object whole.
// Pre-flight, every referenced hash must be present as an entry — export
// always includes them all, so anything less means a broken archive. Extra
// object entries the manifest does not reference are ignored (future
// multi-snapshot archives). Returns how many objects were newly adopted and
// how many were already present. Objects adopted before a later failure stay:
// they are valid, self-verifying CAS files that prune reclaims — an orphan,
// never corruption.
func Adopt(st *store.Store, ix *Index, progress func(n, total int, hash string)) (adopted, present int, err error) {
	need := sortedObjectHashes(ix.Manifest)
	have := map[string]bool{}
	for _, e := range ix.Entries {
		if strings.HasPrefix(e.Name, objectsPrefix) {
			have[strings.TrimPrefix(e.Name, objectsPrefix)] = true
		}
	}
	var missing []string
	for _, h := range need {
		if !have[h] {
			missing = append(missing, h)
		}
	}
	if len(missing) > 0 {
		list := missing[0]
		if len(missing) > 1 {
			list = fmt.Sprintf("%s (+%d more)", missing[0], len(missing)-1)
		}
		return 0, 0, fmt.Errorf("archive is missing %d referenced object(s): %s", len(missing), list)
	}

	total := len(need)
	needSet := make(map[string]bool, total)
	for _, h := range need {
		needSet[h] = true
	}
	f, err := os.Open(ix.Path)
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()

	tr := tar.NewReader(f)
	done := 0
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return adopted, present, fmt.Errorf("read archive: %w", err)
		}
		if !strings.HasPrefix(hdr.Name, objectsPrefix) {
			continue
		}
		hash := strings.TrimPrefix(hdr.Name, objectsPrefix)
		if !needSet[hash] {
			continue // extra entry the manifest does not reference
		}
		ok, err := st.AdoptObject(hash, ix.Kinds[hash], hdr.Size, tr)
		if err != nil {
			return adopted, present, fmt.Errorf("entry %s: %w", hdr.Name, err)
		}
		if ok {
			adopted++
		} else {
			present++
		}
		done++
		if progress != nil {
			progress(done, total, hash)
		}
	}
	if done != total {
		// Unreachable when entries were indexed and validated — kept as a
		// belt-and-braces count (a tar that mutates under us, say).
		return adopted, present, fmt.Errorf("verified %d of %d objects — archive changed under import", done, total)
	}
	return adopted, present, nil
}

// Register persists the imported manifest (pass 3 of import). ID-collision
// policy: an existing snapshot with the identical manifest hash is the
// idempotent no-op ErrAlreadyImported; the same id with different content is
// a hard error (delete the local one, or import into another store).
func Register(st *store.Store, ix *Index) error {
	m := ix.Manifest
	// GetSnapshot prefix-resolves; an id that only prefix-matches a longer
	// local id is not a collision, the exact match is.
	if existing, err := st.GetSnapshot(m.ID); err == nil && existing.ID == m.ID {
		want, err := existing.Hash()
		if err != nil {
			return err
		}
		if want == ix.ManifestHash {
			return fmt.Errorf("%w: %s", ErrAlreadyImported, m.ID)
		}
		return fmt.Errorf("snapshot %s already exists here with DIFFERENT content — delete it first (`dockervc delete %s`) or import into another store (--store <dir>)",
			m.ID, m.ID)
	} else if err != nil && !strings.Contains(err.Error(), "no snapshot matching") {
		return err // a real lookup failure (unreadable manifest row, db error)
	}
	return st.InsertSnapshot(m)
}
