// Package model defines the snapshot manifest: the portable, self-describing
// record of one point-in-time Docker engine state. The manifest references
// data only indirectly, via content-addressed object hashes stored by the
// object store.
package model

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"
)

// Manifest describes everything captured by one snapshot.
type Manifest struct {
	ID            string           `json:"id"`
	CreatedAt     time.Time        `json:"created_at"`
	Message       string           `json:"message"`
	DockerVersion string           `json:"docker_version"`
	EngineID      string           `json:"engine_id"`
	Consistent    bool             `json:"consistent"` // taken with --stop (app-consistent)
	Containers    []ContainerRecord `json:"containers"`
	Images        []ImageRecord     `json:"images"`
	Volumes       []VolumeRecord    `json:"volumes"`
	Networks      []NetworkRecord   `json:"networks"`
	BindMounts    []BindMountRecord `json:"bind_mounts,omitempty"`
	TotalSize     int64            `json:"total_size"` // sum of object sizes (compressed)
	Stats         SnapshotStats     `json:"stats"`
}

// ContainerRecord is one container: its full inspect JSON plus the object
// holding its committed filesystem.
type ContainerRecord struct {
	Name        string          `json:"name"`
	ID          string          `json:"id"`
	InspectJSON json.RawMessage `json:"inspect_json"`
	ImageObject string          `json:"image_object"` // CAS hash of docker-commit'ed image tar
	// LayerHash fingerprints the filesystem CONTENT inside that tar (layer
	// tars only, not the commit config, which is regenerated — and re-hashed —
	// on every commit). It is metadata, not a CAS object: identical LayerHash
	// means the container's files did not change between snapshots.
	LayerHash string `json:"layer_hash,omitempty"`
	Running   bool   `json:"running"` // run state at snapshot time
	Size      int64  `json:"size"`
}

// ImageRecord is one image present in the engine.
type ImageRecord struct {
	Refs   []string `json:"refs"` // repo tags (may be empty for dangling)
	Digest string    `json:"digest"`
	Object string    `json:"object"` // CAS hash of docker-save tar
	Size   int64     `json:"size"`
}

// VolumeRecord is one named volume.
type VolumeRecord struct {
	Name        string            `json:"name"`
	Driver      string            `json:"driver"`
	Options     map[string]string `json:"options,omitempty"`
	Object      string            `json:"object"`                 // CAS hash of zstd-compressed tar
	IndexObject string            `json:"index_object,omitempty"` // CAS hash of file index JSON
	Files       int               `json:"files,omitempty"`        // entries in the index
	Size        int64             `json:"size"`
}

// NetworkRecord is one user-defined network.
type NetworkRecord struct {
	Name        string          `json:"name"`
	InspectJSON json.RawMessage `json:"inspect_json"`
}

// BindMountRecord is one host bind-mount path captured with --include-bind-mounts.
type BindMountRecord struct {
	HostPath string `json:"host_path"`
	Object   string `json:"object"`
	Size     int64  `json:"size"`
}

// SnapshotStats summarizes storage efficiency for this snapshot.
type SnapshotStats struct {
	NewObjects  int   `json:"new_objects"`  // objects written this run
	ReusedObjects int `json:"reused_objects"` // objects already present (deduped)
	NewBytes    int64 `json:"new_bytes"`    // bytes written this run
}

// ObjectInfo describes one stored object.
type ObjectInfo struct {
	Hash     string
	Kind     string // image | volume | bindmount
	Size     int64  // stored (compressed) size in bytes
	ImageKey string // docker image digest, for image dedup; "" otherwise
}

// Hash returns the SHA-256 of the canonical JSON encoding of the manifest.
// Go's json.Marshal emits struct fields in declaration order, so this is
// deterministic.
func (m *Manifest) Hash() (string, error) {
	b, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// ObjectHashes returns every object hash referenced by the manifest.
func (m *Manifest) ObjectHashes() []string {
	var out []string
	for i := range m.Images {
		out = append(out, m.Images[i].Object)
	}
	for i := range m.Containers {
		if m.Containers[i].ImageObject != "" {
			out = append(out, m.Containers[i].ImageObject)
		}
	}
	for i := range m.Volumes {
		out = append(out, m.Volumes[i].Object)
		if m.Volumes[i].IndexObject != "" {
			out = append(out, m.Volumes[i].IndexObject)
		}
	}
	for i := range m.BindMounts {
		out = append(out, m.BindMounts[i].Object)
	}
	return out
}
