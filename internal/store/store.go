// Package store owns all on-disk state for dockervc: the SQLite catalog and
// the content-addressed object store. Layout:
//
//	<path>/
//	  dockervc.db   SQLite catalog (snapshots, objects, refs, config)
//	  objects/ab/<sha256>   content-addressed blobs (zstd-compressed)
//	  tmp/           staging; files atomically renamed into objects/
//	  lock           exclusive flock while a command holds the store
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	_ "modernc.org/sqlite" // registers the "sqlite" driver (pure Go, CGO-free)
)

// ErrNotInitialized is returned when the store directory has no catalog yet.
var ErrNotInitialized = errors.New("dockervc store is not initialized (run `dockervc init` first)")

// Store is a handle to an opened, locked store.
type Store struct {
	Path      string
	DB        *sql.DB
	lockFile  *os.File
	zstdLevel int
}

// DefaultPath returns the default store location: $DOCKERVC_HOME if set;
// otherwise a system location when usable (%ProgramData%\dockervc on
// Windows, /var/lib/dockervc elsewhere); otherwise ~/.dockervc.
func DefaultPath() string {
	if p := os.Getenv("DOCKERVC_HOME"); p != "" {
		return p
	}
	var systemPath string
	if runtime.GOOS == "windows" {
		pd := os.Getenv("ProgramData")
		if pd == "" {
			pd = `C:\ProgramData`
		}
		systemPath = filepath.Join(pd, "dockervc")
	} else {
		systemPath = "/var/lib/dockervc"
	}
	if isUsableDir(systemPath) {
		return systemPath
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return systemPath
	}
	return filepath.Join(home, ".dockervc")
}

func isUsableDir(path string) bool {
	if err := os.MkdirAll(path, 0o750); err != nil {
		return false
	}
	probe := filepath.Join(path, ".probe")
	if err := os.WriteFile(probe, nil, 0o640); err != nil {
		return false
	}
	os.Remove(probe)
	return true
}

// Init creates a fresh store at path. Fails if already initialized.
func Init(path string) (*Store, error) {
	dbPath := filepath.Join(path, "dockervc.db")
	if _, err := os.Stat(dbPath); err == nil {
		return nil, fmt.Errorf("store at %s is already initialized", path)
	}
	for _, d := range []string{path, filepath.Join(path, "objects"), filepath.Join(path, "tmp")} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			return nil, fmt.Errorf("create %s: %w", d, err)
		}
	}
	s, err := openLocked(path)
	if err != nil {
		return nil, err
	}
	if err := s.migrate(); err != nil {
		s.Close()
		return nil, err
	}
	// Defaults.
	if err := s.ConfigSet("zstd_level", "3"); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

// Open opens and locks an existing store.
func Open(path string) (*Store, error) {
	dbPath := filepath.Join(path, "dockervc.db")
	if _, err := os.Stat(dbPath); err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNotInitialized
		}
		return nil, err
	}
	s, err := openLocked(path)
	if err != nil {
		return nil, err
	}
	if err := s.migrate(); err != nil {
		s.Close()
		return nil, err
	}
	if lvl, ok, err := s.ConfigGet("zstd_level"); err != nil {
		s.Close()
		return nil, err
	} else if ok {
		fmt.Sscanf(lvl, "%d", &s.zstdLevel)
	}
	if s.zstdLevel == 0 {
		s.zstdLevel = 3
	}
	return s, nil
}

func openLocked(path string) (*Store, error) {
	lockPath := filepath.Join(path, "lock")
	lf, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o640)
	if err != nil {
		return nil, fmt.Errorf("open lock file: %w", err)
	}
	if err := lockFile(lf); err != nil {
		lf.Close()
		return nil, fmt.Errorf("another dockervc operation is holding the store at %s: %w", path, err)
	}
	dsn := "file:" + filepath.Join(path, "dockervc.db") +
		"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		lf.Close()
		return nil, err
	}
	// A single connection sidesteps SQLITE_BUSY entirely; our workloads are sequential.
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		db.Close()
		lf.Close()
		return nil, err
	}
	return &Store{Path: path, DB: db, lockFile: lf, zstdLevel: 3}, nil
}

// Close releases the store lock and database handle.
func (s *Store) Close() error {
	var firstErr error
	if s.DB != nil {
		firstErr = s.DB.Close()
	}
	if s.lockFile != nil {
		unlockFile(s.lockFile)
		if err := s.lockFile.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// ObjectPath returns the on-disk path for a content hash.
func (s *Store) ObjectPath(hash string) string {
	if len(hash) < 3 {
		return filepath.Join(s.Path, "objects", hash)
	}
	return filepath.Join(s.Path, "objects", hash[:2], hash)
}

// TmpDir is the staging directory for in-progress object writes.
func (s *Store) TmpDir() string { return filepath.Join(s.Path, "tmp") }

// CleanEmptyShards removes shard directories left empty by deletions (from
// stores predating DeleteObject's own shard cleanup).
func (s *Store) CleanEmptyShards() {
	entries, err := os.ReadDir(filepath.Join(s.Path, "objects"))
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() {
			os.Remove(filepath.Join(s.Path, "objects", e.Name())) // no-op unless empty
		}
	}
}
