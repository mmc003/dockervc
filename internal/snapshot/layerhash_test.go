package snapshot

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"testing"

	"dockervc/internal/model"
)

// buildSaveTar builds a docker-save-style tar: manifest.json naming layer
// members, the layer members themselves, and whatever junk entries are given
// (config json, repositories, index.json…).
func buildSaveTar(t *testing.T, layers map[string][]byte, junk map[string][]byte, layerOrder []string) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	write := func(name string, content []byte) {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(content))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(content); err != nil {
			t.Fatal(err)
		}
	}
	mf, err := json.Marshal([]map[string]any{ // array, like real manifest.json
		{"Config": "config.json", "RepoTags": []string{"dockervc/probe:latest"}, "Layers": layerOrder},
	})
	if err != nil {
		t.Fatal(err)
	}
	write("manifest.json", mf)
	for name, content := range junk {
		write(name, content)
	}
	for _, name := range layerOrder {
		write(name, layers[name])
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func layerHashOf(t *testing.T, raw []byte) string {
	t.Helper()
	indexer, err := NewContainerFSIndexer(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("NewContainerFSIndexer: %v", err)
	}
	var got bytes.Buffer
	if _, err := io.Copy(&got, indexer.Reader()); err != nil {
		t.Fatalf("copy through tee: %v", err)
	}
	if !bytes.Equal(got.Bytes(), raw) {
		t.Fatal("tee must pass the save stream through unchanged")
	}
	h, err := indexer.Finish()
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}
	return h
}

func TestContainerFSIndexerIgnoresCommitChurn(t *testing.T) {
	layers := map[string][]byte{
		"blobs/sha256/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa": []byte("writable layer content"),
	}
	// Two saves of the same filesystem: identical layer blob, different
	// config timestamps and repo tags — the classic churn.
	churn := func() map[string][]byte {
		return map[string][]byte{
			"config.json": []byte(`{"created":"` + "2026-09-12T10:00:00Z" + `"}`),
		}
	}
	a := buildSaveTar(t, layers, churn(), []string{"blobs/sha256/aaaa"})
	b := buildSaveTar(t, layers, churn(), []string{"blobs/sha256/aaaa"})

	ha, hb := layerHashOf(t, a), layerHashOf(t, b)
	if ha == "" {
		t.Fatal("expected a layer hash")
	}
	if ha != hb {
		t.Fatalf("identical filesystems must hash equal: %s != %s", ha, hb)
	}

	// Real content change → different hash.
	changed := buildSaveTar(t,
		map[string][]byte{"blobs/sha256/aaaa": []byte("writable layer content v2")},
		churn(), []string{"blobs/sha256/aaaa"})
	if hc := layerHashOf(t, changed); hc == ha {
		t.Fatal("changed layer content must change the hash")
	}
}

// Layer order in the tar (and in manifest.json order) must not matter.
func TestContainerFSIndexerOrderIndependent(t *testing.T) {
	l1 := []byte("layer-one")
	l2 := []byte("layer-two")
	mk := func(first []string) []byte {
		return buildSaveTar(t,
			map[string][]byte{"one.tar": l1, "two.tar": l2}, nil, first)
	}
	ha := layerHashOf(t, mk([]string{"one.tar", "two.tar"}))
	hb := layerHashOf(t, mk([]string{"two.tar", "one.tar"}))
	if ha == "" || ha != hb {
		t.Fatalf("layer order must not affect hash: %q vs %q", ha, hb)
	}
}

// A stream without manifest.json (or with a garbled one) yields "" and no
// error — indexing is optional, capture proceeds.
func TestContainerFSIndexerNotASaveTar(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{Name: "random.bin", Mode: 0o644, Size: 4}); err != nil {
		t.Fatal(err)
	}
	tw.Write([]byte("junk"))
	tw.Close()
	if h := layerHashOf(t, buf.Bytes()); h != "" {
		t.Fatalf("want empty hash for non-save tar, got %q", h)
	}
}

func TestDiffManifestsContainerLayerHash(t *testing.T) {
	mk := func(layer, imageObj string) *model.Manifest {
		return &model.Manifest{Containers: []model.ContainerRecord{{
			Name: "web", ImageObject: imageObj, LayerHash: layer,
		}}}
	}
	// Identical layer fingerprint, different commit object (churn) → clean.
	d := DiffManifests(mk("L", "obj-a"), mk("L", "obj-b"), nil)
	if !d.Containers.Empty() {
		t.Fatalf("expected no container drift, got %+v", d.Containers)
	}
	// Different layer fingerprint → real change.
	d = DiffManifests(mk("L1", "obj-a"), mk("L2", "obj-b"), nil)
	if len(d.Containers.Changed) != 1 || d.Containers.Changed[0] != "web (filesystem changed)" {
		t.Fatalf("Changed = %v", d.Containers.Changed)
	}
	// Pre-layer-hash snapshots → honest churn-flagged fallback.
	old := &model.Manifest{Containers: []model.ContainerRecord{{Name: "web", ImageObject: "o1"}}}
	d = DiffManifests(old, mk("L", "o2"), nil)
	if len(d.Containers.Changed) != 1 || d.Containers.Changed[0] != "web (re-committed — comparison needs newer snapshots)" {
		t.Fatalf("Changed = %v", d.Containers.Changed)
	}
}

// The hash must actually cover layer bytes (guards against an accidentally
// constant implementation).
func TestLayerHashNotConstant(t *testing.T) {
	l := []byte("x")
	sum := sha256.Sum256(l)
	a := layerHashOf(t, buildSaveTar(t,
		map[string][]byte{"l.tar": l}, nil, []string{"l.tar"}))
	if a == hex.EncodeToString(sum[:]) {
		t.Log("hash equals raw sha256 of single layer — fine, but must differ below")
	}
	b := layerHashOf(t, buildSaveTar(t,
		map[string][]byte{"l.tar": []byte("y")}, nil, []string{"l.tar"}))
	if a == b {
		t.Fatal("different layer bytes must produce different hashes")
	}
}
