package store

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"strings"
	"testing"
)

// TestAdoptObjectRoundTrip mirrors the import path: the bytes an export
// carries are the stored (compressed) file bytes, and adopting them into a
// fresh store must reproduce the same hash, size and content — without
// PutBlob's re-compression changing the identity.
func TestAdoptObjectRoundTrip(t *testing.T) {
	src := newTestStore(t)
	res, err := src.PutBlob("volume", "", strings.NewReader("adopt me"))
	if err != nil {
		t.Fatalf("PutBlob: %v", err)
	}
	// The file on disk, verbatim — NOT OpenObject output (it decompresses).
	raw, err := os.ReadFile(src.ObjectPath(res.Hash))
	if err != nil {
		t.Fatal(err)
	}

	dst := newTestStore(t)
	adopted, err := dst.AdoptObject(res.Hash, "image", int64(len(raw)), bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("AdoptObject: %v", err)
	}
	if !adopted {
		t.Fatal("first adopt into an empty store must adopt")
	}

	objs, err := dst.AllObjects()
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != 1 || objs[0].Hash != res.Hash || objs[0].Kind != "image" || objs[0].Size != int64(len(raw)) {
		t.Fatalf("adopted row = %+v, want hash=%s kind=image size=%d", objs[0], res.Hash, len(raw))
	}
	// The file bytes are identical, so the CAS invariant holds: re-hashing the
	// on-disk file yields the hash that named it.
	got, size, err := HashFile(dst.ObjectPath(res.Hash))
	if err != nil || got != res.Hash || size != int64(len(raw)) {
		t.Fatalf("HashFile = %s/%d err=%v, want %s/%d", got, size, err, res.Hash, len(raw))
	}
	// And the decompressing reader still yields the original payload.
	rc, err := dst.OpenObject(res.Hash)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	body := make([]byte, len("adopt me"))
	if _, err := rc.Read(body); err != nil && err.Error() != "EOF" {
		t.Fatalf("OpenObject read: %v", err)
	}
	if string(body) != "adopt me" {
		t.Fatalf("decompressed content = %q", body)
	}
}

func TestAdoptObjectDedupHit(t *testing.T) {
	src := newTestStore(t)
	res, err := src.PutBlob("volume", "", strings.NewReader("same"))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(src.ObjectPath(res.Hash))
	if err != nil {
		t.Fatal(err)
	}

	// Adopt into the store that already holds the file: verify-only, row
	// ensured, adopted=false.
	adopted, err := src.AdoptObject(res.Hash, "volindex", int64(len(raw)), bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("AdoptObject dedup: %v", err)
	}
	if adopted {
		t.Fatal("dedup hit must return adopted=false")
	}
	objs, _ := src.AllObjects()
	if len(objs) != 1 {
		t.Fatalf("dedup hit wrote a second row/file: %d rows", len(objs))
	}
}

func TestAdoptObjectRejectsMismatchedBytes(t *testing.T) {
	s := newTestStore(t)
	other := sha256.Sum256([]byte("not these bytes"))
	otherHash := hex.EncodeToString(other[:])

	// Fresh hash, wrong bytes: error, and neither file nor row may land.
	_, err := s.AdoptObject(otherHash, "image", 4, strings.NewReader("evil"))
	if err == nil {
		t.Fatal("mismatched content must be rejected")
	}
	if _, statErr := os.Stat(s.ObjectPath(otherHash)); statErr == nil {
		t.Fatal("rejected object must not leave a file")
	}
	if objs, _ := s.AllObjects(); len(objs) != 0 {
		t.Fatalf("rejected object must not leave a row: %+v", objs)
	}

	// Already-present hash, wrong incoming bytes: still rejected (an archive
	// disagreeing with its names is bad even when everything is local).
	res, err := s.PutBlob("volume", "", strings.NewReader("good"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.AdoptObject(res.Hash, "volume", 4, strings.NewReader("evil"))
	if err == nil {
		t.Fatal("dedup hit with tampered bytes must be rejected")
	}
	if got, _, _ := HashFile(s.ObjectPath(res.Hash)); got != res.Hash {
		t.Fatal("existing object was damaged by a rejected adopt")
	}
}
