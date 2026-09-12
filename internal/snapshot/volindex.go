// Volume file indexes: while a volume's tar streams into the object store,
// its entries (path, size, mtime, mode, content hash) are indexed — at no
// extra Docker I/O — so two snapshots can be diffed at file granularity
// (created / modified / deleted), git-style.
package snapshot

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"

	"dockervc/internal/model"
)

// FileMeta describes one non-directory entry in a volume.
type FileMeta struct {
	Type  string `json:"type"`           // "file", "symlink", "hardlink", "other"
	Size  int64  `json:"size"`           // bytes of content (files)
	Mtime int64  `json:"mtime"`          // unix seconds
	Mode  int64  `json:"mode"`           // permission bits
	Hash  string `json:"hash,omitempty"` // sha256 of content (regular files)
	Link  string `json:"link,omitempty"` // symlink/hardlink target
}

// FileIndex maps entry paths (as they appear in the volume tar) to metadata.
type FileIndex struct {
	Files map[string]FileMeta `json:"files"`
}

// NewFileIndex parses a tar stream — the exact bytes stored as the volume
// blob — into a FileIndex.
func NewFileIndex(r io.Reader) (*FileIndex, error) {
	idx := &FileIndex{Files: map[string]FileMeta{}}
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		name := cleanEntry(hdr.Name)
		if name == "" || name == "." {
			continue
		}
		meta := FileMeta{
			Mtime: hdr.ModTime.Unix(),
			Mode:  int64(hdr.Mode),
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			continue // directories are reconstructable from entries
		case tar.TypeReg:
			h := sha256.New()
			n, err := io.Copy(h, tr)
			if err != nil {
				return nil, err
			}
			meta.Type = "file"
			meta.Size = n
			meta.Hash = hex.EncodeToString(h.Sum(nil))
		case tar.TypeSymlink:
			meta.Type = "symlink"
			meta.Link = hdr.Linkname
		case tar.TypeLink:
			meta.Type = "hardlink"
			meta.Link = hdr.Linkname
		default:
			meta.Type = "other"
			meta.Size = hdr.Size
		}
		idx.Files[name] = meta
	}
	return idx, nil
}

// Reader returns the index as a JSON stream for storage via PutBlob.
func (idx *FileIndex) Reader() io.Reader {
	b, _ := json.Marshal(idx) // FileIndex is all basic types; cannot fail
	return bytes.NewReader(b)
}

// cleanEntry normalizes a tar entry name the same way for every snapshot, so
// indexes stay comparable (docker archive paths may carry "./" prefixes).
func cleanEntry(name string) string {
	return path.Clean("/" + name)[1:] // strip leading "./", avoid "../" games
}

// ── capture-side tee ─────────────────────────────────────────────────────────

type teeResult struct {
	idx *FileIndex
	err error
}

// VolumeIndexer tees a volume's tar stream: the bytes PutBlob reads are
// unchanged (so the volume blob hash is identical with or without indexing),
// while a side channel parses the same bytes into a FileIndex.
type VolumeIndexer struct {
	tee io.Reader
	pw  *io.PipeWriter
	ch  <-chan teeResult
}

// NewVolumeIndexer starts indexing r in the background.
func NewVolumeIndexer(r io.Reader) *VolumeIndexer {
	pr, pw := io.Pipe()
	ch := make(chan teeResult, 1)
	go func() {
		idx, err := NewFileIndex(pr)
		ch <- teeResult{idx, err}
	}()
	return &VolumeIndexer{tee: io.TeeReader(r, pw), pw: pw, ch: ch}
}

// Reader is what the blob writer should consume instead of the raw stream.
func (v *VolumeIndexer) Reader() io.Reader { return v.tee }

// Finish signals EOF to the parser and returns the completed index. Call it
// only after the reader has been fully consumed (or failed).
func (v *VolumeIndexer) Finish() (*FileIndex, error) {
	v.pw.Close()
	res := <-v.ch
	return res.idx, res.err
}

// ── diff side ────────────────────────────────────────────────────────────────

// BlobOpener opens a stored object by hash (wired to Store.OpenObject).
type BlobOpener func(hash string) (io.ReadCloser, error)

// LoadFileIndex decodes one volume's stored file index; nil (no error) when
// the snapshot predates indexing.
func LoadFileIndex(open BlobOpener, rec model.VolumeRecord) (*FileIndex, error) {
	if open == nil || rec.IndexObject == "" {
		return nil, nil
	}
	r, err := open(rec.IndexObject)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	var idx FileIndex
	if err := json.NewDecoder(r).Decode(&idx); err != nil {
		return nil, err
	}
	if idx.Files == nil {
		idx.Files = map[string]FileMeta{}
	}
	return &idx, nil
}

// FileChange is the file-level diff of one volume between two snapshots.
type FileChange struct {
	Volume   string
	Created  []string
	Modified []string
	Deleted  []string
	HasIndex bool // false when either side predates indexing
}

// Empty reports no file-level difference.
func (fc *FileChange) Empty() bool {
	return len(fc.Created) == 0 && len(fc.Modified) == 0 && len(fc.Deleted) == 0
}

// DiffVolumeFiles compares the file indexes of the same volume in two
// snapshots. Returns nil (no error) when either side lacks an index — e.g.
// the volume is new, or a snapshot predates indexing.
func DiffVolumeFiles(open BlobOpener, a, b model.VolumeRecord) (*FileChange, error) {
	ia, err := LoadFileIndex(open, a)
	if err != nil || ia == nil {
		return nil, err
	}
	ib, err := LoadFileIndex(open, b)
	if err != nil || ib == nil {
		return nil, err
	}
	return DiffIndexes(a.Name, ia, ib), nil
}

// DiffIndexes compares two already-loaded file indexes; nil when either is
// nil (caller decides how to report a missing side).
func DiffIndexes(volume string, ia, ib *FileIndex) *FileChange {
	if ia == nil || ib == nil {
		return nil
	}
	fc := &FileChange{Volume: volume, HasIndex: true}
	for p, mb := range ib.Files {
		ma, ok := ia.Files[p]
		if !ok {
			fc.Created = append(fc.Created, p)
			continue
		}
		if ma.Hash != mb.Hash || ma.Type != mb.Type || ma.Link != mb.Link || ma.Mode != mb.Mode {
			fc.Modified = append(fc.Modified, p)
		}
	}
	for p := range ia.Files {
		if _, ok := ib.Files[p]; !ok {
			fc.Deleted = append(fc.Deleted, p)
		}
	}
	sort.Strings(fc.Created)
	sort.Strings(fc.Modified)
	sort.Strings(fc.Deleted)
	return fc
}

// Summarize renders the "+n created, ~n modified, -n deleted" suffix used in
// diff output.
func (fc *FileChange) Summarize() string {
	if !fc.HasIndex {
		return "content changed (file detail unavailable — snapshot predates indexing)"
	}
	if fc.Empty() {
		return "no file changes"
	}
	var parts []string
	if len(fc.Created) > 0 {
		parts = append(parts, fmt.Sprintf("+%d created", len(fc.Created)))
	}
	if len(fc.Modified) > 0 {
		parts = append(parts, fmt.Sprintf("~%d modified", len(fc.Modified)))
	}
	if len(fc.Deleted) > 0 {
		parts = append(parts, fmt.Sprintf("-%d deleted", len(fc.Deleted)))
	}
	return strings.Join(parts, ", ")
}
