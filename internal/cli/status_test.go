package cli

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"dockervc/internal/model"
	"dockervc/internal/snapshot"
)

func statusTestTar(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for name, content := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(content))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func statusTestIndex(t *testing.T, files map[string]string) []byte {
	t.Helper()
	idx, err := snapshot.NewFileIndex(bytes.NewReader(statusTestTar(t, files)))
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, idx.Reader()); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestAddLiveVolumeDrift(t *testing.T) {
	index := statusTestIndex(t, map[string]string{"same": "same", "mod": "old"})
	m := &model.Manifest{Volumes: []model.VolumeRecord{
		{Name: "common", IndexObject: "common-index"},
		{Name: "legacy"},
		{Name: "removed", IndexObject: "removed-index"},
	}}
	drift := &snapshot.Drift{
		Containers: snapshot.DriftSection{Kind: "containers"},
		Volumes: snapshot.DriftSection{
			Kind: "volumes", Added: []string{"live-new"}, Removed: []string{"removed"},
		},
		Images: snapshot.DriftSection{Kind: "images"},
	}
	var progress bytes.Buffer
	var scanned []string
	err := addLiveVolumeDrift(
		context.Background(),
		func(_ context.Context, name string) (io.ReadCloser, error) {
			scanned = append(scanned, name)
			return io.NopCloser(bytes.NewReader(statusTestTar(t, map[string]string{
				"same": "same", "mod": "new", "added": "yes",
			}))), nil
		},
		m, drift, nil,
		func(string) (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(index)), nil
		},
		&progress,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(scanned) != 1 || scanned[0] != "common" {
		t.Fatalf("scanned = %v, want common only", scanned)
	}
	if got := drift.Volumes.Changed; len(got) != 1 || got[0] != "common (+1 created, ~1 modified)" {
		t.Fatalf("changed = %v", got)
	}
	if got := drift.Volumes.Unavailable; len(got) != 1 || !strings.HasPrefix(got[0], "legacy ") {
		t.Fatalf("unavailable = %v", got)
	}
	if got := progress.String(); got != "scanning volume common...\n" {
		t.Fatalf("progress = %q", got)
	}
}

func TestAddLiveVolumeDriftContinuesAfterFailure(t *testing.T) {
	index := statusTestIndex(t, map[string]string{"file": "old"})
	m := &model.Manifest{Volumes: []model.VolumeRecord{
		{Name: "a-fail", IndexObject: "index"},
		{Name: "b-good", IndexObject: "index"},
	}}
	drift := &snapshot.Drift{Volumes: snapshot.DriftSection{Kind: "volumes"}}
	boom := errors.New("boom")
	err := addLiveVolumeDrift(
		context.Background(),
		func(_ context.Context, name string) (io.ReadCloser, error) {
			if name == "a-fail" {
				return nil, boom
			}
			return io.NopCloser(bytes.NewReader(statusTestTar(t, map[string]string{"file": "new"}))), nil
		},
		m, drift, nil,
		func(string) (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(index)), nil },
		io.Discard,
	)
	if err == nil || !errors.Is(err, boom) {
		t.Fatalf("error = %v, want scan failure", err)
	}
	if len(drift.Volumes.Unavailable) != 1 || !strings.HasPrefix(drift.Volumes.Unavailable[0], "a-fail ") {
		t.Fatalf("unavailable = %v", drift.Volumes.Unavailable)
	}
	if len(drift.Volumes.Changed) != 1 || !strings.HasPrefix(drift.Volumes.Changed[0], "b-good ") {
		t.Fatalf("later volume was not compared: %v", drift.Volumes.Changed)
	}
}

func TestAddLiveVolumeDriftRejectsUnknownFilter(t *testing.T) {
	drift := &snapshot.Drift{Volumes: snapshot.DriftSection{Kind: "volumes"}}
	err := addLiveVolumeDrift(
		context.Background(),
		func(context.Context, string) (io.ReadCloser, error) {
			t.Fatal("unknown filter must fail before scanning")
			return nil, nil
		},
		&model.Manifest{}, drift, []string{"missing"}, nil, io.Discard,
	)
	if err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("error = %v", err)
	}
}

func TestAddLiveVolumeDriftScansOnlySelectedVolumes(t *testing.T) {
	index := statusTestIndex(t, map[string]string{"file": "same"})
	m := &model.Manifest{Volumes: []model.VolumeRecord{
		{Name: "large", IndexObject: "index"},
		{Name: "selected", IndexObject: "index"},
	}}
	drift := &snapshot.Drift{Volumes: snapshot.DriftSection{Kind: "volumes"}}
	var scanned []string
	err := addLiveVolumeDrift(
		context.Background(),
		func(_ context.Context, name string) (io.ReadCloser, error) {
			scanned = append(scanned, name)
			return io.NopCloser(bytes.NewReader(statusTestTar(t, map[string]string{"file": "same"}))), nil
		},
		m, drift, []string{"selected"},
		func(string) (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(index)), nil },
		io.Discard,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(scanned) != 1 || scanned[0] != "selected" {
		t.Fatalf("scanned = %v, want selected only", scanned)
	}
}

func TestResetCommandFlagsStatus(t *testing.T) {
	statusOpts.deep = true
	statusOpts.volumes = []string{"leaked"}
	diffFiles = "leaked"
	resetCommandFlags()
	if statusOpts.deep || len(statusOpts.volumes) != 0 || diffFiles != "" {
		t.Fatalf("status flags leaked: status=%+v diffFiles=%q", statusOpts, diffFiles)
	}
}

func TestPrintDriftShowsUnavailable(t *testing.T) {
	out := captureOutput(func() {
		printDrift(&snapshot.Drift{
			Containers: snapshot.DriftSection{Kind: "containers"},
			Volumes: snapshot.DriftSection{
				Kind:        "volumes",
				Unavailable: []string{"demo (scan failed: denied)"},
			},
			Images: snapshot.DriftSection{Kind: "images"},
		})
	})
	if !strings.Contains(out, "? demo (scan failed: denied)") {
		t.Fatalf("output = %q", out)
	}
	if strings.Contains(out, "No drift") {
		t.Fatalf("unavailable comparison must not report no drift: %q", out)
	}
}
