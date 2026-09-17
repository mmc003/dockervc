package store

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/klauspost/compress/zstd"

	"dockervc/internal/model"
	"dockervc/internal/progress"
)

// ObjectInfoResult reports what PutBlob did.
type ObjectInfoResult struct {
	Hash string
	Kind string
	Size int64 // stored (compressed) bytes
	New  bool  // true if the blob was written by this call (not a dedup hit)
}

// PutBlob streams r into the store as one zstd-compressed, content-addressed
// object. It hashes the stored (compressed) bytes, publishes the file
// atomically (staging write + rename), and registers the object row.
// Snapshot references are written later, transactionally, by
// InsertSnapshot — never here, because the snapshot row must exist first
// (foreign keys are enforced).
func (s *Store) PutBlob(kind, imageKey string, r io.Reader) (ObjectInfoResult, error) {
	return s.PutBlobTracked(kind, imageKey, r, nil, nil)
}

// PutBlobTracked is PutBlob with callbacks for uncompressed input bytes and
// compressed stored bytes. Callbacks may be nil.
func (s *Store) PutBlobTracked(kind, imageKey string, r io.Reader,
	inputAdvance, storedAdvance func(int64)) (ObjectInfoResult, error) {
	var res ObjectInfoResult
	res.Kind = kind

	tmp, err := os.CreateTemp(s.TmpDir(), "obj-*")
	if err != nil {
		return res, err
	}
	tmpName := tmp.Name()
	defer func() {
		tmp.Close()
		os.Remove(tmpName) // no-op after a successful rename
	}()

	// Hash the bytes as stored (compressed), so verification re-hashes the
	// file on disk directly.
	h := sha256.New()
	stored := &progress.CountingWriter{Writer: io.MultiWriter(tmp, h), Advance: storedAdvance}
	zw, err := zstd.NewWriter(stored,
		zstd.WithEncoderLevel(zstd.EncoderLevelFromZstd(s.zstdLevel)))
	if err != nil {
		return res, err
	}
	input := &progress.CountingReader{Reader: r, Advance: inputAdvance}
	if _, err := io.Copy(zw, input); err != nil {
		zw.Close()
		return res, fmt.Errorf("read %s stream: %w", kind, err)
	}
	if err := zw.Close(); err != nil {
		return res, err
	}
	if err := tmp.Sync(); err != nil {
		return res, err
	}
	if err := tmp.Close(); err != nil {
		return res, err
	}

	hash := hex.EncodeToString(h.Sum(nil))
	res.Hash = hash
	dst := s.ObjectPath(hash)
	if _, err := os.Stat(dst); err == nil {
		// Dedup hit: identical content already stored. Still ensure the row
		// exists (a second digest may map to identical bytes).
		res.Size = fileSize(dst)
		if err := s.PutObject(model.ObjectInfo{Hash: hash, Kind: kind, Size: res.Size, ImageKey: imageKey}); err != nil {
			return res, err
		}
	} else {
		if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
			return res, err
		}
		if err := os.Rename(tmpName, dst); err != nil {
			return res, err
		}
		res.Size = fileSize(dst)
		res.New = true
		if err := s.PutObject(model.ObjectInfo{Hash: hash, Kind: kind, Size: res.Size, ImageKey: imageKey}); err != nil {
			return res, err
		}
	}
	return res, nil
}

// objectReader ties the zstd decoder's lifetime to the backing file.
type objectReader struct {
	z *zstd.Decoder
	f *os.File
}

func (o *objectReader) Read(p []byte) (int, error) { return o.z.Read(p) }
func (o *objectReader) Close() error {
	o.z.Close()
	return o.f.Close()
}

// OpenObject returns a zstd-decompressing reader over one stored object.
// The caller must Close it.
func (s *Store) OpenObject(hash string) (io.ReadCloser, error) {
	return s.OpenObjectTracked(hash, nil)
}

// OpenObjectTracked is OpenObject with callbacks for compressed bytes read
// from the CAS file. Counting below the decoder makes ObjectSize an exact
// progress total even though callers consume decompressed bytes.
func (s *Store) OpenObjectTracked(hash string, advance func(int64)) (io.ReadCloser, error) {
	f, err := os.Open(s.ObjectPath(hash))
	if err != nil {
		return nil, err
	}
	counted := &progress.CountingReader{Reader: f, Advance: advance}
	zr, err := zstd.NewReader(counted)
	if err != nil {
		f.Close()
		return nil, err
	}
	return &objectReader{z: zr, f: f}, nil
}

// HashFile re-hashes a stored object file (compressed bytes).
func HashFile(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

func fileSize(path string) int64 {
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return fi.Size()
}

func removeFileIfExists(path string) error {
	err := os.Remove(path)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}
