package snapshot

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"dockervc/internal/model"
)

type failingReadCloser struct {
	r      *bytes.Reader
	err    error
	closed bool
}

func (r *failingReadCloser) Read(p []byte) (int, error) {
	if r.r.Len() == 0 {
		return 0, r.err
	}
	return r.r.Read(p)
}

func (r *failingReadCloser) Close() error {
	r.closed = true
	return nil
}

func TestIndexLiveVolume(t *testing.T) {
	raw := buildTar(t, map[string]string{"a.txt": "a", "dir/b.txt": "b"})
	var opened string
	idx, err := IndexLiveVolume(context.Background(), func(_ context.Context, name string) (io.ReadCloser, error) {
		opened = name
		return io.NopCloser(bytes.NewReader(raw)), nil
	}, "demo")
	if err != nil {
		t.Fatal(err)
	}
	if opened != "demo" || len(idx.Files) != 2 {
		t.Fatalf("opened=%q files=%d", opened, len(idx.Files))
	}
}

func TestIndexLiveVolumeObservesTrailingStreamError(t *testing.T) {
	raw := buildTar(t, map[string]string{"a.txt": "a"})
	r := &failingReadCloser{r: bytes.NewReader(raw), err: errors.New("helper failed")}
	_, err := IndexLiveVolume(context.Background(), func(context.Context, string) (io.ReadCloser, error) {
		return r, nil
	}, "demo")
	if err == nil || !errors.Is(err, r.err) {
		t.Fatalf("error = %v, want helper failure", err)
	}
	if !r.closed {
		t.Fatal("live stream was not closed after failure")
	}
}

func TestDiffLiveVolume(t *testing.T) {
	before, err := NewFileIndex(bytes.NewReader(buildTar(t, map[string]string{
		"same.txt": "same",
		"mod.txt":  "old",
	})))
	if err != nil {
		t.Fatal(err)
	}
	var stored bytes.Buffer
	if _, err := io.Copy(&stored, before.Reader()); err != nil {
		t.Fatal(err)
	}

	rec := model.VolumeRecord{Name: "demo", IndexObject: "index"}
	change, oldIndex, liveIndex, err := DiffLiveVolume(
		context.Background(),
		func(context.Context, string) (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(buildTar(t, map[string]string{
				"same.txt": "same",
				"mod.txt":  "new",
				"new.txt":  "added",
			}))), nil
		},
		func(string) (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(stored.Bytes())), nil
		},
		rec,
	)
	if err != nil {
		t.Fatal(err)
	}
	if oldIndex == nil || liveIndex == nil || change == nil {
		t.Fatal("expected both indexes and a change result")
	}
	if got := change.Summarize(); got != "+1 created, ~1 modified" {
		t.Fatalf("summary = %q", got)
	}
}

func TestDiffLiveVolumeMissingSnapshotIndexDoesNotScan(t *testing.T) {
	called := false
	change, oldIndex, liveIndex, err := DiffLiveVolume(
		context.Background(),
		func(context.Context, string) (io.ReadCloser, error) {
			called = true
			return nil, errors.New("must not scan")
		},
		nil,
		model.VolumeRecord{Name: "legacy"},
	)
	if err != nil || change != nil || oldIndex != nil || liveIndex != nil {
		t.Fatalf("got change=%v old=%v live=%v err=%v", change, oldIndex, liveIndex, err)
	}
	if called {
		t.Fatal("live volume was scanned without a snapshot index")
	}
}
