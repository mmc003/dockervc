// Package artifact exports individual resources from a local snapshot.
package artifact

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path"
	"strings"

	"github.com/klauspost/compress/zstd"

	"dockervc/internal/progress"
	"dockervc/internal/store"
)

// Selection identifies one stored resource and an optional path within a
// volume tar. Raw is valid only with Path and emits one regular file body.
type Selection struct {
	Kind       string // volume | image
	Name       string
	ObjectHash string
	Path       string
	Raw        bool
}

// Result describes the emitted artifact.
type Result struct {
	Bytes  int64
	SHA256 string
	Files  int
}

// Exporter streams resources from the local CAS without requiring Docker.
type Exporter struct {
	Store    *store.Store
	Reporter progress.Reporter
}

// Export writes a selected resource to w while authenticating the compressed
// CAS object named by Selection.ObjectHash.
func (e *Exporter) Export(ctx context.Context, sel Selection, w io.Writer) (res Result, retErr error) {
	if e.Store == nil {
		return res, fmt.Errorf("artifact exporter has no store")
	}
	if sel.Kind != "volume" && sel.Kind != "image" {
		return res, fmt.Errorf("unsupported resource kind %q", sel.Kind)
	}
	if sel.Path != "" && sel.Kind != "volume" {
		return res, fmt.Errorf("--path is only valid with --volume")
	}
	if sel.Raw && sel.Path == "" {
		return res, fmt.Errorf("--raw requires --path")
	}
	selectedPath, err := NormalizePath(sel.Path)
	if err != nil {
		return res, err
	}

	f, err := os.Open(e.Store.ObjectPath(sel.ObjectHash))
	if err != nil {
		return res, fmt.Errorf("open %s object: %w", sel.Kind, err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return res, err
	}

	meter := progress.NewMeter(e.Reporter, "export")
	defer func() { meter.Finish(retErr != nil) }()
	phase := "exporting " + sel.Kind
	if selectedPath != "" {
		phase = "exporting volume selection"
	}
	meter.Phase(phase, sel.Name, fi.Size(), progress.TotalExact, 1)

	storedHash := sha256.New()
	stored := io.TeeReader(&contextReader{ctx: ctx, r: f}, storedHash)
	counted := &progress.CountingReader{Reader: stored, Advance: meter.AddBytes}
	zr, err := zstd.NewReader(counted)
	if err != nil {
		return res, fmt.Errorf("decode object %s: %w", sel.ObjectHash, err)
	}
	defer zr.Close()

	outHash := sha256.New()
	countedOut := &countingWriter{w: io.MultiWriter(w, outHash)}
	if selectedPath == "" {
		_, err = io.Copy(countedOut, zr)
		res.Files = 1
	} else if sel.Raw {
		res.Files, err = exportRawFile(zr, countedOut, selectedPath)
	} else {
		res.Files, err = exportTarSelection(zr, countedOut, selectedPath)
	}
	if err != nil {
		return res, err
	}
	// Consume any compressed bytes buffered beyond the logical zstd frame so
	// the digest covers the complete stored object.
	if _, err := io.Copy(io.Discard, counted); err != nil {
		return res, fmt.Errorf("finish reading object %s: %w", sel.ObjectHash, err)
	}
	if got := hex.EncodeToString(storedHash.Sum(nil)); got != sel.ObjectHash {
		return res, fmt.Errorf("object %s failed checksum verification (got %s)", sel.ObjectHash, got)
	}
	res.Bytes = countedOut.n
	res.SHA256 = hex.EncodeToString(outHash.Sum(nil))
	meter.Item(sel.Name, 1, 1)
	return res, nil
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r *contextReader) Read(p []byte) (int, error) {
	select {
	case <-r.ctx.Done():
		return 0, r.ctx.Err()
	default:
		return r.r.Read(p)
	}
}

type countingWriter struct {
	w io.Writer
	n int64
}

func (w *countingWriter) Write(p []byte) (int, error) {
	n, err := w.w.Write(p)
	w.n += int64(n)
	return n, err
}

// NormalizePath validates and canonicalizes a volume-root-relative path.
func NormalizePath(value string) (string, error) {
	value = strings.ReplaceAll(value, "\\", "/")
	for strings.HasPrefix(value, "./") {
		value = strings.TrimPrefix(value, "./")
	}
	if value == "" || value == "." {
		return "", nil
	}
	if strings.HasPrefix(value, "/") || (len(value) >= 2 && value[1] == ':') {
		return "", fmt.Errorf("path must be relative to the volume root")
	}
	for _, part := range strings.Split(value, "/") {
		if part == ".." {
			return "", fmt.Errorf("path must not contain ..")
		}
		if strings.ContainsRune(part, 0) {
			return "", fmt.Errorf("path must not contain NUL")
		}
	}
	clean := path.Clean(value)
	if clean == "." {
		return "", nil
	}
	return clean, nil
}

func safeTarName(value string) (string, error) {
	if strings.HasPrefix(value, "/") {
		return "", fmt.Errorf("tar contains absolute path %q", value)
	}
	clean, err := NormalizePath(value)
	if err != nil {
		return "", err
	}
	// Docker's volume archive starts with a conventional "./" directory
	// header. It represents the volume root, not an unsafe or empty member.
	if clean == "" {
		return "", nil
	}
	return clean, nil
}

func matchesPath(name, selected string) bool {
	return name == selected || strings.HasPrefix(name, selected+"/")
}

func exportTarSelection(r io.Reader, w io.Writer, selected string) (int, error) {
	tr := tar.NewReader(r)
	tw := tar.NewWriter(w)
	matched := 0
	seen := map[string]bool{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return matched, fmt.Errorf("read volume tar: %w", err)
		}
		name, err := safeTarName(hdr.Name)
		if err != nil {
			return matched, err
		}
		if name == "" {
			continue
		}
		if !matchesPath(name, selected) {
			continue
		}
		if seen[name] {
			return matched, fmt.Errorf("volume tar contains duplicate path %q", name)
		}
		seen[name] = true
		clone := *hdr
		clone.Name = name
		if err := tw.WriteHeader(&clone); err != nil {
			return matched, err
		}
		if _, err := io.Copy(tw, tr); err != nil {
			return matched, err
		}
		matched++
	}
	if matched == 0 {
		return 0, fmt.Errorf("path %q does not exist in the captured volume", selected)
	}
	if err := tw.Close(); err != nil {
		return matched, err
	}
	return matched, nil
}

func exportRawFile(r io.Reader, w io.Writer, selected string) (int, error) {
	tr := tar.NewReader(r)
	matched := false
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return 0, fmt.Errorf("read volume tar: %w", err)
		}
		name, err := safeTarName(hdr.Name)
		if err != nil {
			return 0, err
		}
		if name == "" {
			continue
		}
		if name != selected {
			if strings.HasPrefix(name, selected+"/") {
				return 0, fmt.Errorf("--raw path %q is a directory", selected)
			}
			continue
		}
		if matched {
			return 0, fmt.Errorf("volume tar contains duplicate path %q", selected)
		}
		if hdr.Typeflag != tar.TypeReg && hdr.Typeflag != tar.TypeRegA {
			return 0, fmt.Errorf("--raw path %q is not a regular file", selected)
		}
		if _, err := io.Copy(w, tr); err != nil {
			return 0, err
		}
		matched = true
	}
	if !matched {
		return 0, fmt.Errorf("path %q does not exist in the captured volume", selected)
	}
	return 1, nil
}
