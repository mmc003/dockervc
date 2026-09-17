package rollback

import (
	"io"
	"path/filepath"
	"strings"
	"testing"

	"dockervc/internal/progress"
	"dockervc/internal/store"
)

func TestTrackedRestoreObjectReportsCompressedBytes(t *testing.T) {
	s, err := store.Init(filepath.Join(t.TempDir(), "store"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	obj, err := s.PutBlob("volume", "", strings.NewReader(strings.Repeat("restore-data", 1024)))
	if err != nil {
		t.Fatal(err)
	}
	var events []progress.Event
	meter := progress.NewMeter(progress.ReporterFunc(func(e progress.Event) {
		events = append(events, e)
	}), "restore")
	meter.Phase("applying restore plan", "volume", obj.Size, progress.TotalExact, 1)
	ex := &Executor{St: s, meter: meter}
	r, err := ex.openTrackedObject(obj.Hash)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(io.Discard, r); err != nil {
		t.Fatal(err)
	}
	r.Close()
	meter.Finish(false)

	var max int64
	for _, event := range events {
		if event.BytesDone > max {
			max = event.BytesDone
		}
	}
	if max != obj.Size {
		t.Fatalf("reported bytes = %d, object size = %d", max, obj.Size)
	}
}
