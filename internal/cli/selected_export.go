package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"dockervc/internal/artifact"
	"dockervc/internal/model"
	"dockervc/internal/snapshot"
)

type exportIndex struct {
	Version    int                `json:"version"`
	SnapshotID string             `json:"snapshot_id"`
	UpdatedAt  time.Time          `json:"updated_at"`
	Artifacts  []exportIndexEntry `json:"artifacts"`
}

type exportIndexEntry struct {
	Type      string    `json:"type"`
	Resource  string    `json:"resource"`
	Path      string    `json:"path,omitempty"`
	Format    string    `json:"format"`
	File      string    `json:"file"`
	SHA256    string    `json:"sha256"`
	Size      int64     `json:"size"`
	CreatedAt time.Time `json:"created_at"`
}

func exportSelected(cmd *cobra.Command, m *model.Manifest) error {
	if exportOpts.volume != "" && exportOpts.image != "" {
		return fmt.Errorf("--volume and --image are mutually exclusive")
	}
	if exportOpts.path != "" && exportOpts.volume == "" {
		return fmt.Errorf("--path requires --volume")
	}
	if exportOpts.raw && exportOpts.path == "" {
		return fmt.Errorf("--raw requires --path")
	}
	if exportOpts.image != "" && exportOpts.raw {
		return fmt.Errorf("--raw is only valid with --volume and --path")
	}

	var sel artifact.Selection
	var image model.ImageRecord
	var err error
	entryType := "volume"
	resource := exportOpts.volume
	if exportOpts.volume != "" {
		sel, _, err = artifact.SelectVolume(m, exportOpts.volume)
		sel.Path = exportOpts.path
		sel.Raw = exportOpts.raw
	} else if exportOpts.image != "" {
		entryType = "image"
		resource = exportOpts.image
		sel, image, err = artifact.SelectImage(m, exportOpts.image)
	} else {
		return fmt.Errorf("select a resource with --volume or --image")
	}
	if err != nil {
		return err
	}

	out := exportOpts.out
	indexed := false
	format := "tar"
	if exportOpts.raw {
		format = "raw"
	}
	if out == "" {
		out, err = defaultSelectedExportPath(m.ID, sel, image)
		if err != nil {
			return err
		}
		indexed = true
		fmt.Printf("no -o given — writing %s\n", out)
	}
	out = expandHome(out)
	objectPath := st.ObjectPath(sel.ObjectHash)
	outAbs, outAbsErr := filepath.Abs(out)
	objectAbs, objectAbsErr := filepath.Abs(objectPath)
	if outAbsErr == nil && objectAbsErr == nil && strings.EqualFold(filepath.Clean(outAbs), filepath.Clean(objectAbs)) {
		return fmt.Errorf("output path must not be the selected object's CAS file")
	}
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(out), err)
	}
	if fi, err := os.Lstat(out); err == nil {
		objectInfo, objectErr := os.Stat(objectPath)
		if objectErr == nil && os.SameFile(fi, objectInfo) {
			return fmt.Errorf("output path must not alias the selected object's CAS file")
		}
		if fi.Mode()&os.ModeSymlink != 0 || !fi.Mode().IsRegular() {
			return fmt.Errorf("output path %s exists and is not a regular file", out)
		}
		if !forceYes {
			return fmt.Errorf("%s already exists — pass --yes to overwrite it", out)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("inspect output path %s: %w", out, err)
	}

	tmp, err := os.CreateTemp(filepath.Dir(out), ".dockervc-export-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() {
		tmp.Close()
		os.Remove(tmpName)
	}
	exp := &artifact.Exporter{Store: st, Reporter: commandProgress(cmd.ErrOrStderr())}
	result, err := exp.Export(cmd.Context(), sel, tmp)
	if err != nil {
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, out); err != nil {
		os.Remove(tmpName)
		return err
	}

	if indexed {
		entry := exportIndexEntry{
			Type: entryType, Resource: resource, Path: exportOpts.path,
			Format: format, SHA256: result.SHA256, Size: result.Bytes, CreatedAt: time.Now().UTC(),
		}
		base := filepath.Join(defaultExportDir(), m.ID)
		if rel, relErr := filepath.Rel(base, out); relErr == nil {
			entry.File = filepath.ToSlash(rel)
		} else {
			entry.File = filepath.Base(out)
		}
		if err := updateExportIndex(base, m.ID, entry); err != nil {
			return fmt.Errorf("artifact exported, but export index update failed: %w", err)
		}
	}
	label := entryType
	if exportOpts.path != "" {
		label = "volume path"
	}
	fmt.Printf("Exported %s %s from %s → %s (%s, sha256:%s)\n",
		label, resource, m.ID, out, snapshot.HumanBytes(result.Bytes), result.SHA256[:12])
	return nil
}

func defaultSelectedExportPath(snapshotID string, sel artifact.Selection, image model.ImageRecord) (string, error) {
	base := filepath.Join(defaultExportDir(), snapshotID)
	volume := safeExportComponent(sel.Name)
	if sel.Kind == "image" {
		digest := strings.TrimPrefix(image.Digest, "sha256:")
		if len(digest) > 12 {
			digest = digest[:12]
		}
		return filepath.Join(base, "images", safeExportComponent(sel.Name)+"--"+safeExportComponent(digest)+".tar"), nil
	}
	if sel.Path == "" {
		return filepath.Join(base, "volumes", volume+".tar"), nil
	}
	normalized, err := artifact.NormalizePath(sel.Path)
	if err != nil {
		return "", err
	}
	parts := strings.Split(normalized, "/")
	for i := range parts {
		parts[i] = safeExportComponent(parts[i])
	}
	if sel.Raw {
		leaf := parts[len(parts)-1]
		ext := filepath.Ext(leaf)
		stem := strings.TrimSuffix(leaf, ext)
		sum := sha256.Sum256([]byte(normalized))
		parts[len(parts)-1] = stem + "--" + hex.EncodeToString(sum[:5]) + ext
		return filepath.Join(append([]string{base, "raw", volume}, parts...)...), nil
	}
	parts[len(parts)-1] += ".tar"
	return filepath.Join(append([]string{base, "selections", volume}, parts...)...), nil
}

var unsafeExportChars = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

func safeExportComponent(value string) string {
	value = strings.Trim(unsafeExportChars.ReplaceAllString(value, "_"), "._-")
	if value == "" {
		return "unnamed"
	}
	return value
}

func updateExportIndex(base, snapshotID string, entry exportIndexEntry) error {
	if err := os.MkdirAll(base, 0o755); err != nil {
		return err
	}
	path := filepath.Join(base, "export-index.json")
	idx := exportIndex{Version: 1, SnapshotID: snapshotID}
	if b, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(b, &idx); err != nil {
			return fmt.Errorf("read existing %s: %w", path, err)
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	replaced := false
	for i := range idx.Artifacts {
		if idx.Artifacts[i].File == entry.File {
			idx.Artifacts[i] = entry
			replaced = true
			break
		}
	}
	if !replaced {
		idx.Artifacts = append(idx.Artifacts, entry)
	}
	idx.Version = 1
	idx.SnapshotID = snapshotID
	idx.UpdatedAt = time.Now().UTC()
	b, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	tmp, err := os.CreateTemp(base, ".export-index-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}
