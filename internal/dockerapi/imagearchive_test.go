package dockerapi

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"io"
	"testing"
)

func TestTagNeutralImageArchiveRemovesMutableTags(t *testing.T) {
	var input bytes.Buffer
	tw := tar.NewWriter(&input)
	write := func(name, body string) {
		t.Helper()
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(tw, body); err != nil {
			t.Fatal(err)
		}
	}
	write("manifest.json", `[{"Config":"config.json","RepoTags":["public/app:latest"],"Layers":["layer/layer.tar"]}]`)
	write("repositories", `{"public/app":{"latest":"layer"}}`)
	write("config.json", `{"architecture":"amd64"}`)
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	out := TagNeutralImageArchive(io.NopCloser(bytes.NewReader(input.Bytes())))
	defer out.Close()
	tr := tar.NewReader(out)
	seenConfig := false
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		switch hdr.Name {
		case "repositories":
			t.Fatal("legacy repositories metadata survived neutralization")
		case "manifest.json":
			var entries []struct {
				RepoTags []string `json:"RepoTags"`
			}
			if err := json.Unmarshal(data, &entries); err != nil {
				t.Fatal(err)
			}
			if len(entries) != 1 || len(entries[0].RepoTags) != 0 {
				t.Fatalf("RepoTags = %#v, want none", entries)
			}
		case "config.json":
			seenConfig = string(data) == `{"architecture":"amd64"}`
		}
	}
	if !seenConfig {
		t.Fatal("non-tag archive content was not preserved")
	}
}
