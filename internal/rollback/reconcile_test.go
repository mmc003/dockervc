package rollback

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"dockervc/internal/model"
	"dockervc/internal/snapshot"
)

func reconcileTar(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(tw, body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestReconcileVolumesClassifiesRequiredVolumes(t *testing.T) {
	snapshotTar := reconcileTar(t, map[string]string{"same.txt": "same", "changed.txt": "old"})
	idx, err := snapshot.NewFileIndex(bytes.NewReader(snapshotTar))
	if err != nil {
		t.Fatal(err)
	}
	var idxJSON bytes.Buffer
	if _, err := io.Copy(&idxJSON, idx.Reader()); err != nil {
		t.Fatal(err)
	}
	m := &model.Manifest{Volumes: []model.VolumeRecord{
		{Name: "same", IndexObject: "same-index"},
		{Name: "changed", IndexObject: "changed-index"},
		{Name: "missing", IndexObject: "missing-index"},
		{Name: "legacy"},
	}}
	live := liveEmpty()
	for _, name := range []string{"same", "changed", "legacy"} {
		live.VolumeNames[name] = true
	}
	openTar := func(_ context.Context, name string) (io.ReadCloser, error) {
		files := map[string]string{"same.txt": "same", "changed.txt": "old"}
		if name == "changed" {
			files["changed.txt"] = "new"
		}
		return io.NopCloser(bytes.NewReader(reconcileTar(t, files))), nil
	}
	openBlob := func(string) (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(idxJSON.Bytes())), nil
	}
	if err := ReconcileVolumes(context.Background(), openTar, openBlob, m,
		Scope{All: true}, live, nil); err != nil {
		t.Fatal(err)
	}
	if live.VolumeStates["same"] != EntityUnchanged {
		t.Fatalf("same state = %q", live.VolumeStates["same"])
	}
	if live.VolumeStates["changed"] != EntityChanged || live.VolumeDiffs["changed"] != "~1 modified" {
		t.Fatalf("changed state/diff = %q / %q", live.VolumeStates["changed"], live.VolumeDiffs["changed"])
	}
	if live.VolumeStates["missing"] != EntityMissing {
		t.Fatalf("missing state = %q", live.VolumeStates["missing"])
	}
	if live.VolumeStates["legacy"] != EntityUnverifiable {
		t.Fatalf("legacy state = %q", live.VolumeStates["legacy"])
	}
}

func TestReconcileVolumesHonorsScopeAndCancellation(t *testing.T) {
	m := &model.Manifest{Volumes: []model.VolumeRecord{
		{Name: "selected", IndexObject: "index"},
		{Name: "ignored", IndexObject: "index"},
	}}
	live := liveEmpty()
	live.VolumeNames["selected"] = true
	live.VolumeNames["ignored"] = true
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := ReconcileVolumes(ctx,
		func(context.Context, string) (io.ReadCloser, error) {
			return nil, errors.New("must not open")
		},
		func(string) (io.ReadCloser, error) {
			return nil, errors.New("must not open")
		},
		m, Scope{Volumes: []string{"selected"}}, live, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context cancellation", err)
	}
	if _, ok := live.VolumeStates["ignored"]; ok {
		t.Fatal("out-of-scope volume was classified")
	}
}
