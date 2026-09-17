package portable

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"

	"dockervc/internal/progress"
)

func TestPeekAndVerifyArchive(t *testing.T) {
	s, manifest := newFixtureStore(t)
	archive := exportFixture(t, s, manifest)

	peeked, err := PeekArchive(archive)
	if err != nil {
		t.Fatalf("PeekArchive: %v", err)
	}
	if peeked.ID != manifest.ID || peeked.Message != manifest.Message {
		t.Fatalf("peeked manifest = %+v, want %s/%q", peeked, manifest.ID, manifest.Message)
	}

	var events []progress.Event
	ix, err := VerifyArchive(archive, progress.ReporterFunc(func(e progress.Event) {
		events = append(events, e)
	}))
	if err != nil {
		t.Fatalf("VerifyArchive: %v", err)
	}
	if ix.Manifest.ID != manifest.ID || ix.ObjectCount() != 3 {
		t.Fatalf("verified index = %+v", ix)
	}
	if len(events) == 0 || !events[len(events)-1].Finished || events[len(events)-1].Failed {
		t.Fatalf("terminal progress event = %+v", events)
	}
}

func TestVerifyArchiveDetectsTamperedObject(t *testing.T) {
	s, manifest := newFixtureStore(t)
	archive := exportFixture(t, s, manifest)
	_, content := readTarEntries(t, archive)
	name := objectsPrefix + manifest.Volumes[0].Object
	tampered := append([]byte(nil), content[name]...)
	tampered[len(tampered)/2] ^= 0xff
	rebuildArchive(t, archive, map[string][]byte{name: tampered}, false)

	if _, err := VerifyArchive(archive, nil); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("VerifyArchive tamper error = %v", err)
	}
}

func TestWithObjectReturnsAuthenticatedStoredBytes(t *testing.T) {
	s, manifest := newFixtureStore(t)
	archive := exportFixture(t, s, manifest)
	hash := manifest.Volumes[0].IndexObject
	want, err := os.ReadFile(s.ObjectPath(hash))
	if err != nil {
		t.Fatal(err)
	}
	var got []byte
	if err := WithObject(archive, hash, func(r io.Reader) error {
		var err error
		got, err = io.ReadAll(r)
		return err
	}); err != nil {
		t.Fatalf("WithObject: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("object bytes differ: got %d, want %d", len(got), len(want))
	}
}
