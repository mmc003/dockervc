package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"dockervc/internal/model"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Init(filepath.Join(t.TempDir(), "store"))
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestPutBlobDedups(t *testing.T) {
	s := newTestStore(t)

	a, err := s.PutBlob("volume", "", strings.NewReader("same content"))
	if err != nil {
		t.Fatalf("PutBlob: %v", err)
	}
	if !a.New {
		t.Fatal("first write should be new")
	}
	// Identical input must dedup even under a different kind/image key.
	b, err := s.PutBlob("image", "sha256:abc", strings.NewReader("same content"))
	if err != nil {
		t.Fatalf("PutBlob: %v", err)
	}
	if b.New || b.Hash != a.Hash {
		t.Fatalf("identical content should dedup: got new=%v hash=%s want hash=%s", b.New, b.Hash, a.Hash)
	}
	c, err := s.PutBlob("volume", "", strings.NewReader("different"))
	if err != nil {
		t.Fatalf("PutBlob: %v", err)
	}
	if !c.New || c.Hash == a.Hash {
		t.Fatal("different content must produce a new object")
	}

	objs, err := s.AllObjects()
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != 2 {
		t.Fatalf("expected 2 objects, got %d", len(objs))
	}
}

func TestPutBlobDetectsCorruptionViaHash(t *testing.T) {
	s := newTestStore(t)
	res, err := s.PutBlob("volume", "", strings.NewReader("payload"))
	if err != nil {
		t.Fatal(err)
	}
	// Tamper with the stored file; verification hashing must notice.
	if err := os.WriteFile(s.ObjectPath(res.Hash), []byte("evil"), 0o640); err != nil {
		t.Fatal(err)
	}
	got, size, err := HashFile(s.ObjectPath(res.Hash))
	if err != nil {
		t.Fatal(err)
	}
	if got == res.Hash || size != int64(len("evil")) {
		t.Fatal("HashFile should reflect tampered content")
	}
}

func TestObjectByImageKey(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.PutBlob("image", "sha256:feed", strings.NewReader("img")); err != nil {
		t.Fatal(err)
	}
	o, ok, err := s.ObjectByImageKey("sha256:feed")
	if err != nil || !ok {
		t.Fatalf("expected dedup hit: ok=%v err=%v", ok, err)
	}
	if o.Kind != "image" {
		t.Fatalf("kind = %s", o.Kind)
	}
	if _, ok, _ := s.ObjectByImageKey("sha256:other"); ok {
		t.Fatal("unknown digest must not hit")
	}
}

func TestSnapshotRefsAndPrune(t *testing.T) {
	s := newTestStore(t)

	// A snapshot whose volume record references a stored object.
	res, err := s.PutBlob("volume", "", strings.NewReader("data"))
	if err != nil {
		t.Fatal(err)
	}
	m := &model.Manifest{
		ID:      "snap-x",
		Volumes: []model.VolumeRecord{{Name: "v", Object: res.Hash, Size: res.Size}},
	}
	if err := s.InsertSnapshot(m); err != nil {
		t.Fatalf("InsertSnapshot: %v", err)
	}

	// A stray object no snapshot references.
	if _, err := s.PutBlob("volume", "", strings.NewReader("orphan")); err != nil {
		t.Fatal(err)
	}
	unref, err := s.UnreferencedObjects()
	if err != nil {
		t.Fatal(err)
	}
	if len(unref) != 1 {
		t.Fatalf("expected 1 unreferenced object, got %d", len(unref))
	}
	if err := s.DeleteObject(unref[0].Hash); err != nil {
		t.Fatal(err)
	}
	if unref, _ = s.UnreferencedObjects(); len(unref) != 0 {
		t.Fatal("orphan should be gone")
	}
	// Referenced object must survive.
	if _, err := s.ObjectSize(res.Hash); err != nil {
		t.Fatalf("referenced object deleted: %v", err)
	}

	// Deleting the snapshot orphans its object, making it prunable.
	if err := s.DeleteSnapshot("snap-x"); err != nil {
		t.Fatal(err)
	}
	if unref, _ := s.UnreferencedObjects(); len(unref) != 1 {
		t.Fatalf("expected snapshot object to become unreferenced, got %d", len(unref))
	}
}

func TestGetSnapshotByPrefix(t *testing.T) {
	s := newTestStore(t)
	mk := func(id string) *model.Manifest {
		return &model.Manifest{ID: id, Message: "m-" + id}
	}
	for _, id := range []string{"snap-20260911-100000-aaaa", "snap-20260911-110000-bbbb"} {
		if err := s.InsertSnapshot(mk(id)); err != nil {
			t.Fatal(err)
		}
	}
	if m, err := s.GetSnapshot("snap-20260911-100000-aaaa"); err != nil || m.ID != "snap-20260911-100000-aaaa" {
		t.Fatalf("exact lookup failed: %v", err)
	}
	if m, err := s.GetSnapshot("snap-20260911-11"); err != nil || m.ID != "snap-20260911-110000-bbbb" {
		t.Fatalf("prefix lookup failed: %v", err)
	}
	if _, err := s.GetSnapshot("snap-20260911"); err == nil {
		t.Fatal("ambiguous prefix should error")
	}
	if _, err := s.GetSnapshot("nope"); err == nil {
		t.Fatal("unknown id should error")
	}
}

func TestConfigDelete(t *testing.T) {
	s := newTestStore(t)
	if err := s.ConfigSet("export.folder", "/tmp/exports"); err != nil {
		t.Fatalf("ConfigSet: %v", err)
	}
	if err := s.ConfigDelete("export.folder"); err != nil {
		t.Fatalf("ConfigDelete: %v", err)
	}
	if v, ok, err := s.ConfigGet("export.folder"); err != nil || ok || v != "" {
		t.Fatalf("after delete: got (%q, %v, %v), want unset", v, ok, err)
	}
	// deleting an unset key is a no-op, and unrelated keys survive
	if err := s.ConfigDelete("export.folder"); err != nil {
		t.Fatalf("second delete must not error: %v", err)
	}
	if v, ok, err := s.ConfigGet("zstd_level"); err != nil || !ok || v == "" {
		t.Fatalf("zstd_level (set at init) should survive: got (%q, %v, %v)", v, ok, err)
	}
}
