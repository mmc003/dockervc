package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"dockervc/internal/model"
)

const schema = `
CREATE TABLE IF NOT EXISTS snapshots (
	id           TEXT PRIMARY KEY,
	created_at   INTEGER NOT NULL,
	message      TEXT NOT NULL DEFAULT '',
	manifest     TEXT NOT NULL,
	manifest_hash TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS objects (
	hash      TEXT PRIMARY KEY,
	kind      TEXT NOT NULL,             -- image | volume | volindex | bindmount
	size      INTEGER NOT NULL,          -- stored (compressed) bytes
	created_at INTEGER NOT NULL,
	image_key TEXT                       -- docker image digest, for dedup; NULL otherwise
);
CREATE TABLE IF NOT EXISTS refs (
	snapshot_id TEXT NOT NULL REFERENCES snapshots(id) ON DELETE CASCADE,
	hash        TEXT NOT NULL REFERENCES objects(hash),
	PRIMARY KEY (snapshot_id, hash)
);
CREATE TABLE IF NOT EXISTS config (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_refs_hash ON refs(hash);
CREATE INDEX IF NOT EXISTS idx_objects_image_key ON objects(image_key);
`

func (s *Store) migrate() error {
	_, err := s.DB.Exec(schema)
	return err
}

// SnapshotRow is a lightweight listing entry.
type SnapshotRow struct {
	ID        string
	CreatedAt time.Time
	Message   string
	Size      int64 // sum of referenced object sizes
}

// InsertSnapshot persists a completed manifest and its object references.
func (s *Store) InsertSnapshot(m *model.Manifest) error {
	hash, err := m.Hash()
	if err != nil {
		return err
	}
	blob, err := json.Marshal(m)
	if err != nil {
		return err
	}
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(
		`INSERT INTO snapshots (id, created_at, message, manifest, manifest_hash) VALUES (?,?,?,?,?)`,
		m.ID, m.CreatedAt.Unix(), m.Message, string(blob), hash,
	); err != nil {
		return err
	}
	for _, h := range m.ObjectHashes() {
		if _, err := tx.Exec(`INSERT OR IGNORE INTO refs (snapshot_id, hash) VALUES (?,?)`, m.ID, h); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// GetSnapshot resolves a full or unambiguous-prefix snapshot ID to its manifest.
func (s *Store) GetSnapshot(idOrPrefix string) (*model.Manifest, error) {
	row := s.DB.QueryRow(`SELECT manifest FROM snapshots WHERE id = ?`, idOrPrefix)
	var blob string
	err := row.Scan(&blob)
	if errors.Is(err, sql.ErrNoRows) {
		rows, err := s.DB.Query(`SELECT manifest FROM snapshots WHERE id LIKE ?`, idOrPrefix+"%")
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var candidates []string
		for rows.Next() {
			if err := rows.Scan(&blob); err != nil {
				return nil, err
			}
			candidates = append(candidates, blob)
		}
		switch len(candidates) {
		case 0:
			return nil, fmt.Errorf("no snapshot matching %q", idOrPrefix)
		case 1:
			blob = candidates[0]
		default:
			return nil, fmt.Errorf("ambiguous snapshot prefix %q — be more specific", idOrPrefix)
		}
	} else if err != nil {
		return nil, err
	}
	var m model.Manifest
	if err := json.Unmarshal([]byte(blob), &m); err != nil {
		return nil, fmt.Errorf("corrupt manifest for snapshot: %w", err)
	}
	return &m, nil
}

// ListSnapshots returns all snapshots, oldest first.
func (s *Store) ListSnapshots() ([]SnapshotRow, error) {
	rows, err := s.DB.Query(`
		SELECT s.id, s.created_at, s.message, COALESCE(SUM(o.size), 0)
		FROM snapshots s
		LEFT JOIN refs r ON r.snapshot_id = s.id
		LEFT JOIN objects o ON o.hash = r.hash
		GROUP BY s.id
		ORDER BY s.created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SnapshotRow
	for rows.Next() {
		var r SnapshotRow
		var ts int64
		if err := rows.Scan(&r.ID, &ts, &r.Message, &r.Size); err != nil {
			return nil, err
		}
		r.CreatedAt = time.Unix(ts, 0)
		out = append(out, r)
	}
	return out, rows.Err()
}

// DeleteSnapshot removes a snapshot row and its references (objects are
// reclaimed later by Prune).
func (s *Store) DeleteSnapshot(id string) error {
	res, err := s.DB.Exec(`DELETE FROM snapshots WHERE id = ?`, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("no snapshot matching %q", id)
	}
	return nil
}

// LatestSnapshot returns the most recent manifest, or nil if none exist.
func (s *Store) LatestSnapshot() (*model.Manifest, error) {
	row := s.DB.QueryRow(`SELECT manifest FROM snapshots ORDER BY created_at DESC, id DESC LIMIT 1`)
	var blob string
	if err := row.Scan(&blob); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	var m model.Manifest
	if err := json.Unmarshal([]byte(blob), &m); err != nil {
		return nil, fmt.Errorf("corrupt manifest: %w", err)
	}
	return &m, nil
}

// PutObject registers object metadata (file already published by the CAS).
func (s *Store) PutObject(o model.ObjectInfo) error {
	_, err := s.DB.Exec(`
		INSERT OR IGNORE INTO objects (hash, kind, size, created_at, image_key) VALUES (?,?,?,?,?)`,
		o.Hash, o.Kind, o.Size, time.Now().Unix(), nilIfEmpty(o.ImageKey))
	return err
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// ObjectByImageKey returns a previously stored object for a docker image
// digest, enabling image dedup across snapshots.
func (s *Store) ObjectByImageKey(digest string) (model.ObjectInfo, bool, error) {
	row := s.DB.QueryRow(`SELECT hash, kind, size, COALESCE(image_key,'') FROM objects WHERE image_key = ? LIMIT 1`, digest)
	var o model.ObjectInfo
	err := row.Scan(&o.Hash, &o.Kind, &o.Size, &o.ImageKey)
	if errors.Is(err, sql.ErrNoRows) {
		return o, false, nil
	}
	if err != nil {
		return o, false, err
	}
	return o, true, nil
}

// SetImageKey retroactively keys an already-stored object (used to register
// container-filesystem hashes after the blob stream has been analyzed).
func (s *Store) SetImageKey(hash, key string) error {
	_, err := s.DB.Exec(`UPDATE objects SET image_key = ? WHERE hash = ?`, key, hash)
	return err
}

// UnreferencedObjects lists objects with zero snapshot references.
func (s *Store) UnreferencedObjects() ([]model.ObjectInfo, error) {
	rows, err := s.DB.Query(`
		SELECT o.hash, o.kind, o.size FROM objects o
		LEFT JOIN refs r ON r.hash = o.hash
		WHERE r.hash IS NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.ObjectInfo
	for rows.Next() {
		var o model.ObjectInfo
		if err := rows.Scan(&o.Hash, &o.Kind, &o.Size); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// DeleteObject removes an object row and its file. The shard directory is
// removed too when this was its last object, so pruning doesn't litter the
// objects tree with empty folders.
func (s *Store) DeleteObject(hash string) error {
	if _, err := s.DB.Exec(`DELETE FROM objects WHERE hash = ?`, hash); err != nil {
		return err
	}
	if err := removeFileIfExists(s.ObjectPath(hash)); err != nil {
		return err
	}
	os.Remove(filepath.Dir(s.ObjectPath(hash))) // no-op unless now empty
	return nil
}

// AllObjects returns every object row, for verification.
func (s *Store) AllObjects() ([]model.ObjectInfo, error) {
	rows, err := s.DB.Query(`SELECT hash, kind, size, COALESCE(image_key,'') FROM objects`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.ObjectInfo
	for rows.Next() {
		var o model.ObjectInfo
		if err := rows.Scan(&o.Hash, &o.Kind, &o.Size, &o.ImageKey); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// ObjectSize returns the stored size of one object hash.
func (s *Store) ObjectSize(hash string) (int64, error) {
	row := s.DB.QueryRow(`SELECT size FROM objects WHERE hash = ?`, hash)
	var n int64
	err := row.Scan(&n)
	return n, err
}

// QuickCheck runs SQLite's own consistency check.
func (s *Store) QuickCheck() error {
	row := s.DB.QueryRow(`PRAGMA quick_check`)
	var result string
	if err := row.Scan(&result); err != nil {
		return err
	}
	if result != "ok" {
		return fmt.Errorf("sqlite quick_check: %s", result)
	}
	return nil
}

// ConfigGet returns a config value and whether it exists.
func (s *Store) ConfigGet(key string) (string, bool, error) {
	row := s.DB.QueryRow(`SELECT value FROM config WHERE key = ?`, key)
	var v string
	err := row.Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return v, true, nil
}

// ConfigSet upserts a config value.
func (s *Store) ConfigSet(key, value string) error {
	_, err := s.DB.Exec(`INSERT INTO config (key, value) VALUES (?,?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

// ConfigAll returns all config entries.
func (s *Store) ConfigAll() (map[string]string, error) {
	rows, err := s.DB.Query(`SELECT key, value FROM config ORDER BY key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}
