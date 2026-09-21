package cli

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dockervc/internal/model"
	"dockervc/internal/snapshot"
	"dockervc/internal/store"
)

func buildSelectiveExportStore(t *testing.T, root, exportRoot string) string {
	t.Helper()
	storePath = root
	s, err := store.Init(root)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var volume bytes.Buffer
	tw := tar.NewWriter(&volume)
	for name, body := range map[string]string{"etc/app.conf": "enabled=true\n", "data/a.txt": "alpha\n"} {
		if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(tw, body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	vol, err := s.PutBlob("volume", "", bytes.NewReader(volume.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	idx, err := snapshot.NewFileIndex(bytes.NewReader(volume.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	idxObj, err := s.PutBlob("volindex", "", idx.Reader())
	if err != nil {
		t.Fatal(err)
	}
	image, err := s.PutBlob("image", "sha256:abc123", strings.NewReader("docker-save-tar"))
	if err != nil {
		t.Fatal(err)
	}
	id := "snap-selective"
	m := &model.Manifest{
		ID: id, CreatedAt: time.Now().UTC(),
		Volumes: []model.VolumeRecord{{Name: "app_data", Object: vol.Hash, IndexObject: idxObj.Hash, Files: len(idx.Files), Size: vol.Size}},
		Images:  []model.ImageRecord{{Refs: []string{"demo/app:latest"}, Digest: "sha256:abc123", Object: image.Hash, Size: image.Size}},
	}
	if err := s.InsertSnapshot(m); err != nil {
		t.Fatal(err)
	}
	if err := s.ConfigSet("export.folder", exportRoot); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestSelectiveExportsAndIndex(t *testing.T) {
	root := t.TempDir()
	exports := filepath.Join(t.TempDir(), "exports")
	id := buildSelectiveExportStore(t, root, exports)

	out := captureOutput(func() { runArgs("export", "latest", "--volume", "app_data", "--path", "etc/app.conf", "--raw") })
	if !strings.Contains(out, "Exported volume path") {
		t.Fatalf("raw export output = %q", out)
	}
	rawMatches, _ := filepath.Glob(filepath.Join(exports, id, "raw", "app_data", "etc", "app--*.conf"))
	if len(rawMatches) != 1 {
		t.Fatalf("raw matches = %v", rawMatches)
	}
	if b, err := os.ReadFile(rawMatches[0]); err != nil || string(b) != "enabled=true\n" {
		t.Fatalf("raw artifact = %q, %v", b, err)
	}

	out = captureOutput(func() { runArgs("export", id, "--image", "demo/app:latest") })
	if !strings.Contains(out, "Exported image") {
		t.Fatalf("image export output = %q", out)
	}
	indexPath := filepath.Join(exports, id, "export-index.json")
	b, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	var idx exportIndex
	if err := json.Unmarshal(b, &idx); err != nil {
		t.Fatal(err)
	}
	if idx.SnapshotID != id || len(idx.Artifacts) != 2 {
		t.Fatalf("index = %+v", idx)
	}

	rows, err := scanArchives(exports)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].Snapshot != id || rows[1].Snapshot != id {
		t.Fatalf("partial export rows = %+v", rows)
	}
}

func TestSelectiveExportFlagValidation(t *testing.T) {
	root := t.TempDir()
	exports := filepath.Join(t.TempDir(), "exports")
	id := buildSelectiveExportStore(t, root, exports)
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"export", id, "--volume", "app_data", "--image", "demo/app:latest"}, "mutually exclusive"},
		{[]string{"export", id, "--path", "etc"}, "requires --volume"},
		{[]string{"export", id, "--volume", "app_data", "--raw"}, "requires --path"},
	} {
		out := captureOutput(func() { runArgs(tc.args...) })
		if !strings.Contains(out, tc.want) {
			t.Fatalf("%v output %q does not contain %q", tc.args, out, tc.want)
		}
	}
}
