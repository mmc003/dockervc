// Container filesystem hashing: `docker commit` mints a fresh image config
// (with a new creation timestamp) on every run, so the saved-image object
// hash differs even when the container's filesystem is byte-identical —
// "snapshot twice, change nothing" showed every container as changed, and
// nothing ever deduped. Hashing only the layer content named by the save
// tar's manifest.json gives a stable fingerprint of the filesystem itself.
package snapshot

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path"
	"sort"
)

// saveManifest is the subset of docker-save's manifest.json we need. The
// file is an ARRAY of per-image entries — a save can carry multiple images —
// each naming its config blob and layer members. Layer paths appear in both
// the classic (<id>/layer.tar) and OCI (blobs/sha256/<digest>) layouts.
type saveManifestEntry struct {
	Config string   `json:"Config"`
	Layers []string `json:"Layers"`
}

// ContainerFSIndexer tees a `docker save` stream to a spill file so the blob
// PutBlob consumes is unchanged, then analyzes the file at leisure —
// manifest.json may appear after the blobs it references, so one ordered
// pass can't know which entries are layers.
type ContainerFSIndexer struct {
	tee io.Reader
	f   *os.File
}

// NewContainerFSIndexer starts spilling r to a temp file. On error the
// caller can fall back to indexing nothing (nil indexer, stream untouched).
func NewContainerFSIndexer(r io.Reader) (*ContainerFSIndexer, error) {
	f, err := os.CreateTemp("", "dockervc-save-")
	if err != nil {
		return nil, err
	}
	return &ContainerFSIndexer{tee: io.TeeReader(r, f), f: f}, nil
}

// Reader is what the blob writer should consume instead of the raw stream.
func (c *ContainerFSIndexer) Reader() io.Reader { return c.tee }

// Finish analyzes the spilled tar and returns the filesystem hash: sha256
// over the sorted content hashes of every layer named in manifest.json —
// independent of layer order, config churn and tar layout. "" (no error)
// when the stream doesn't look like a docker-save tar worth indexing; the
// spill file is removed either way.
func (c *ContainerFSIndexer) Finish() (string, error) {
	defer func() {
		c.f.Close()
		os.Remove(c.f.Name())
	}()

	want, err := c.layerPaths()
	if err != nil || len(want) == 0 {
		return "", err
	}

	if _, err := c.f.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	tr := tar.NewReader(c.f)
	hashes := map[string]bool{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
		if !want[cleanEntry(hdr.Name)] {
			continue
		}
		h := sha256.New()
		if _, err := io.Copy(h, tr); err != nil {
			return "", err
		}
		hashes[hex.EncodeToString(h.Sum(nil))] = true
	}
	if len(hashes) != len(want) {
		return "", nil // manifest named layers the tar doesn't carry — bail out
	}
	sorted := make([]string, 0, len(hashes))
	for h := range hashes {
		sorted = append(sorted, h)
	}
	sort.Strings(sorted)
	sum := sha256.Sum256([]byte(joinLines(sorted)))
	return hex.EncodeToString(sum[:]), nil
}

// layerPaths reads manifest.json from the spill file (pass 1) and returns
// the set of tar-member names holding layer content.
func (c *ContainerFSIndexer) layerPaths() (map[string]bool, error) {
	if _, err := c.f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	tr := tar.NewReader(c.f)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil, nil // no manifest.json — not indexable
		}
		if err != nil {
			return nil, err
		}
		if path.Clean(hdr.Name) != "manifest.json" {
			continue
		}
		raw, err := io.ReadAll(io.LimitReader(tr, 8<<20))
		if err != nil {
			return nil, err
		}
		var mf []saveManifestEntry
		if err := json.Unmarshal(raw, &mf); err != nil {
			return nil, nil // tolerate odd layouts: indexing off, not failed
		}
		out := map[string]bool{}
		for _, e := range mf {
			for _, l := range e.Layers {
				out[cleanEntry(l)] = true
			}
		}
		return out, nil
	}
}

func joinLines(ss []string) string {
	out := ""
	for i, s := range ss {
		if i > 0 {
			out += "\n"
		}
		out += s
	}
	return out + "\n"
}
