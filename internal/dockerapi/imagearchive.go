package dockerapi

import (
	"archive/tar"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// TagNeutralImageArchive removes mutable repository tags from a docker-save
// stream. Loading the returned archive can make the image content available,
// but cannot move or replace a public tag as a side effect.
func TagNeutralImageArchive(src io.ReadCloser) io.ReadCloser {
	pr, pw := io.Pipe()
	go func() {
		defer src.Close()
		tw := tar.NewWriter(pw)
		err := rewriteTagNeutralArchive(tar.NewReader(src), tw)
		if closeErr := tw.Close(); err == nil {
			err = closeErr
		}
		pw.CloseWithError(err)
	}()
	return pr
}

func rewriteTagNeutralArchive(tr *tar.Reader, tw *tar.Writer) error {
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read docker image archive: %w", err)
		}
		name := strings.TrimPrefix(hdr.Name, "./")
		if name == "repositories" {
			if _, err := io.Copy(io.Discard, tr); err != nil {
				return err
			}
			continue
		}
		if name == "manifest.json" {
			if hdr.Size > 16<<20 {
				return fmt.Errorf("docker image manifest is unexpectedly large: %d bytes", hdr.Size)
			}
			data, err := io.ReadAll(tr)
			if err != nil {
				return err
			}
			var entries []map[string]json.RawMessage
			if err := json.Unmarshal(data, &entries); err != nil {
				return fmt.Errorf("decode docker image manifest: %w", err)
			}
			for _, entry := range entries {
				entry["RepoTags"] = json.RawMessage("null")
			}
			data, err = json.Marshal(entries)
			if err != nil {
				return err
			}
			hdr.Size = int64(len(data))
			if err := tw.WriteHeader(hdr); err != nil {
				return err
			}
			if _, err := tw.Write(data); err != nil {
				return err
			}
			continue
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if _, err := io.Copy(tw, tr); err != nil {
			return err
		}
	}
}
