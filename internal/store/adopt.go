package store

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"dockervc/internal/model"
)

// AdoptObject places content-addressed bytes into the store VERBATIM: no
// compression, no re-derivation of identity — the hash names exactly these
// bytes, and PutBlob would re-compress and change it (import path only).
//
// Bytes stream to tmp/, are re-hashed in flight (defense in depth: mismatch →
// error, tmp removed), then rename into objects/<xx>/<hash> and INSERT OR
// IGNORE the objects row with the actual byte count. Returns adopted=false on
// a dedup hit (file already present), where nothing is written and only the
// row is ensured — the incoming bytes are still streamed and re-hashed, so an
// archive whose content disagrees with its names is rejected even when every
// object is already local. size is the advertised byte count (the archive's
// tar header); disagreement with the bytes actually read is an error.
func (s *Store) AdoptObject(hash, kind string, size int64, src io.Reader) (adopted bool, err error) {
	// A present file turns the pass into verify-only: no tmp write, no
	// rename, just the row (which may be missing under a new kind).
	if _, err := os.Stat(s.ObjectPath(hash)); err == nil {
		n, got, err := hashStream(src)
		if err != nil {
			return false, fmt.Errorf("read %s: %w", hash, err)
		}
		if err := sizeMatches(size, n); err != nil {
			return false, fmt.Errorf("%s: %w", hash, err)
		}
		if got != hash {
			return false, fmt.Errorf("%s: content hashes to %s — archive tampered or truncated", hash, got)
		}
		if err := s.PutObject(model.ObjectInfo{Hash: hash, Kind: kind, Size: fileSize(s.ObjectPath(hash))}); err != nil {
			return false, err
		}
		return false, nil
	}

	tmp, err := os.CreateTemp(s.TmpDir(), "adopt-*")
	if err != nil {
		return false, err
	}
	tmpName := tmp.Name()
	defer func() {
		tmp.Close()
		os.Remove(tmpName) // no-op after a successful rename
	}()

	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), src)
	if err != nil {
		return false, fmt.Errorf("read %s: %w", hash, err)
	}
	if err := sizeMatches(size, n); err != nil {
		return false, fmt.Errorf("%s: %w", hash, err)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != hash {
		return false, fmt.Errorf("%s: content hashes to %s — archive tampered or truncated", hash, got)
	}
	if err := tmp.Sync(); err != nil {
		return false, err
	}
	if err := tmp.Close(); err != nil {
		return false, err
	}

	dst := s.ObjectPath(hash)
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return false, err
	}
	if err := os.Rename(tmpName, dst); err != nil {
		return false, err
	}
	if err := s.PutObject(model.ObjectInfo{Hash: hash, Kind: kind, Size: n}); err != nil {
		return false, err
	}
	return true, nil
}

// hashStream hashes src while discarding it, returning byte count and hex sum.
func hashStream(src io.Reader) (int64, string, error) {
	h := sha256.New()
	n, err := io.Copy(h, src)
	if err != nil {
		return 0, "", err
	}
	return n, hex.EncodeToString(h.Sum(nil)), nil
}

// sizeMatches cross-checks the advertised size; a size of 0 or less means the
// caller had no expectation to check.
func sizeMatches(size, got int64) error {
	if size > 0 && size != got {
		return fmt.Errorf("size mismatch: %d bytes advertised, %d read", size, got)
	}
	return nil
}
