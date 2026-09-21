package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	pathpkg "path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/spf13/cobra"

	"dockervc/internal/model"
	"dockervc/internal/portable"
	"dockervc/internal/snapshot"
)

type archiveListRow struct {
	Path     string
	Size     int64
	Modified time.Time
	Created  time.Time
	Snapshot string
	Type     string
	Manifest *model.Manifest
	Err      error
}

var archiveCmd = &cobra.Command{
	Use:   "archive",
	Short: "Inspect and verify exported .dvca archives",
	Long: `Inspect exported .dvca archives without importing them.

Listing and showing read only the leading manifest and are fast even for large
archives. Verify performs a full structural and checksum pass. Files lists the
captured file index for one volume.`,
}

var archiveListCmd = &cobra.Command{
	Use:   "list [directory]",
	Short: "List exported archives and selective artifacts",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		dir := defaultExportDir()
		explicit := false
		if len(args) == 1 {
			dir = expandHome(args[0])
			explicit = true
		}
		rows, err := scanArchives(dir)
		if err != nil {
			if os.IsNotExist(err) && !explicit {
				fmt.Printf("No exported archives in %s.\n", dir)
				return nil
			}
			return err
		}
		if len(rows) == 0 {
			fmt.Printf("No exported archives in %s.\n", dir)
			return nil
		}
		fmt.Printf("archives in %s (including selective exports)\n", dir)
		fmt.Printf("%-20s %-10s %-28s %-12s %s\n", "CREATED", "SIZE", "SNAPSHOT", "TYPE", "FILE")
		for _, row := range rows {
			if row.Err != nil {
				fmt.Printf("%-20s %-10s %-28s %-12s %s  [unreadable: %v]\n",
					row.Modified.Local().Format("2006-01-02 15:04"), snapshot.HumanBytes(row.Size), row.Snapshot,
					row.Type, displayExportPath(dir, row.Path), row.Err)
				continue
			}
			fmt.Printf("%-20s %-10s %-28s %-12s %s\n",
				row.Created.Local().Format("2006-01-02 15:04"), snapshot.HumanBytes(row.Size),
				row.Snapshot, row.Type, displayExportPath(dir, row.Path))
		}
		return nil
	},
}

var archiveShowCmd = &cobra.Command{
	Use:   "show <archive.dvca>",
	Short: "Show an exported archive's snapshot manifest",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		archivePath := expandHome(args[0])
		m, err := portable.PeekArchive(archivePath)
		if err != nil {
			return fmt.Errorf("inspect %s: %w", archivePath, err)
		}
		fi, err := os.Stat(archivePath)
		if err != nil {
			return err
		}
		printArchiveManifest(archivePath, fi.Size(), m)
		return nil
	},
}

var archiveVerifyCmd = &cobra.Command{
	Use:   "verify <archive.dvca>",
	Short: "Fully verify archive structure and object checksums",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		archivePath := expandHome(args[0])
		ix, err := portable.VerifyArchive(archivePath, commandProgress(cmd.ErrOrStderr()))
		if err != nil {
			return fmt.Errorf("verify %s: %w", archivePath, err)
		}
		fi, _ := os.Stat(archivePath)
		size := int64(0)
		if fi != nil {
			size = fi.Size()
		}
		fmt.Printf("Verified %s — snapshot %s, %d object(s), %s.\n",
			archivePath, ix.Manifest.ID, ix.ObjectCount(), snapshot.HumanBytes(size))
		return nil
	},
}

var archiveFilesCmd = &cobra.Command{
	Use:   "files <archive.dvca> <volume> [prefix]",
	Short: "List files captured for one archived volume",
	Args:  cobra.RangeArgs(2, 3),
	RunE: func(cmd *cobra.Command, args []string) error {
		archivePath := expandHome(args[0])
		m, err := portable.PeekArchive(archivePath)
		if err != nil {
			return fmt.Errorf("inspect %s: %w", archivePath, err)
		}
		volume, err := archivedVolume(m, args[1])
		if err != nil {
			return err
		}
		if volume.IndexObject == "" {
			return fmt.Errorf("volume %s has no file index (snapshot predates file indexing)", volume.Name)
		}
		prefix := ""
		if len(args) == 3 {
			prefix, err = cleanArchivePrefix(args[2])
			if err != nil {
				return err
			}
		}
		idx, err := archiveVolumeIndex(archivePath, volume.IndexObject)
		if err != nil {
			return fmt.Errorf("read file index for %s: %w", volume.Name, err)
		}
		return printArchiveFiles(m.ID, volume.Name, prefix, idx)
	},
}

func init() {
	archiveCmd.AddCommand(archiveListCmd, archiveShowCmd, archiveVerifyCmd, archiveFilesCmd)
	rootCmd.AddCommand(archiveCmd)
}

func scanArchives(dir string) ([]archiveListRow, error) {
	dir = expandHome(dir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read archive directory %s: %w", dir, err)
	}
	rows := make([]archiveListRow, 0)
	for _, entry := range entries {
		if entry.IsDir() || !strings.EqualFold(filepath.Ext(entry.Name()), ".dvca") {
			continue
		}
		archivePath := filepath.Join(dir, entry.Name())
		fi, err := entry.Info()
		if err != nil {
			rows = append(rows, archiveListRow{Path: archivePath, Err: err})
			continue
		}
		m, peekErr := portable.PeekArchive(archivePath)
		created := fi.ModTime()
		snapshotID := "-"
		if m != nil {
			created = m.CreatedAt
			snapshotID = m.ID
		}
		rows = append(rows, archiveListRow{
			Path: archivePath, Size: fi.Size(), Modified: fi.ModTime(), Created: created,
			Snapshot: snapshotID, Type: "snapshot", Manifest: m, Err: peekErr,
		})
	}
	rows = append(rows, scanPartialExports(dir, entries)...)
	sort.Slice(rows, func(i, j int) bool { return rows[i].Modified.After(rows[j].Modified) })
	return rows, nil
}

func scanPartialExports(root string, entries []os.DirEntry) []archiveListRow {
	var rows []archiveListRow
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		base := filepath.Join(root, entry.Name())
		indexPath := filepath.Join(base, "export-index.json")
		b, err := os.ReadFile(indexPath)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			rows = append(rows, archiveListRow{Path: indexPath, Snapshot: entry.Name(), Type: "index", Err: err})
			continue
		}
		var idx exportIndex
		if err := json.Unmarshal(b, &idx); err != nil {
			rows = append(rows, archiveListRow{Path: indexPath, Snapshot: entry.Name(), Type: "index", Err: err})
			continue
		}
		for _, item := range idx.Artifacts {
			artifactPath := filepath.Join(base, filepath.FromSlash(item.File))
			rel, relErr := filepath.Rel(base, artifactPath)
			if relErr != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(item.File) {
				rows = append(rows, archiveListRow{Path: indexPath, Snapshot: idx.SnapshotID, Type: item.Type, Err: fmt.Errorf("unsafe indexed path %q", item.File)})
				continue
			}
			fi, statErr := os.Stat(artifactPath)
			modified := item.CreatedAt
			size := item.Size
			if fi != nil {
				modified = fi.ModTime()
				size = fi.Size()
			}
			rows = append(rows, archiveListRow{
				Path: artifactPath, Size: size, Modified: modified, Created: item.CreatedAt,
				Snapshot: idx.SnapshotID, Type: item.Type + "/" + item.Format, Err: statErr,
			})
		}
	}
	return rows
}

func displayExportPath(root, value string) string {
	if rel, err := filepath.Rel(root, value); err == nil {
		return rel
	}
	return value
}

func printArchiveManifest(archivePath string, archiveSize int64, m *model.Manifest) {
	objects := map[string]bool{}
	for _, hash := range m.ObjectHashes() {
		if hash != "" {
			objects[hash] = true
		}
	}
	fmt.Printf("archive %s\n", archivePath)
	fmt.Printf("  snapshot:       %s\n", m.ID)
	fmt.Printf("  created:        %s\n", m.CreatedAt.Local().Format(time.DateTime))
	fmt.Printf("  message:        %s\n", m.Message)
	fmt.Printf("  engine:         docker %s (%s)\n", m.DockerVersion, m.EngineID)
	fmt.Printf("  archive size:   %s\n", snapshot.HumanBytes(archiveSize))
	fmt.Printf("  captured size:  %s across %d object(s)\n", snapshot.HumanBytes(m.TotalSize), len(objects))
	fmt.Printf("  contents:       %d container(s), %d image(s), %d volume(s), %d network(s), %d bind mount(s)\n",
		len(m.Containers), len(m.Images), len(m.Volumes), len(m.Networks), len(m.BindMounts))
	fmt.Println("  verification:   not run (use `dockervc archive verify` for a full checksum pass)")
	if len(m.Volumes) > 0 {
		fmt.Println("volumes:")
		for _, volume := range m.Volumes {
			fileInfo := "file index unavailable"
			if volume.IndexObject != "" {
				fileInfo = fmt.Sprintf("%d indexed file(s)", volume.Files)
			}
			fmt.Printf("  %-30s %s  %s\n", volume.Name, snapshot.HumanBytes(volume.Size), fileInfo)
		}
	}
	if len(m.Images) > 0 {
		fmt.Println("images:")
		for _, image := range m.Images {
			name := image.Digest
			if len(image.Refs) > 0 {
				name = strings.Join(image.Refs, ", ")
			}
			fmt.Printf("  %-30s %s\n", name, snapshot.HumanBytes(image.Size))
		}
	}
}

func archivedVolume(m *model.Manifest, name string) (model.VolumeRecord, error) {
	for _, volume := range m.Volumes {
		if volume.Name == name {
			return volume, nil
		}
	}
	names := make([]string, 0, len(m.Volumes))
	for _, volume := range m.Volumes {
		names = append(names, volume.Name)
	}
	if len(names) == 0 {
		return model.VolumeRecord{}, fmt.Errorf("archive snapshot %s contains no volumes", m.ID)
	}
	return model.VolumeRecord{}, fmt.Errorf("archive has no volume %q (available: %s)", name, strings.Join(names, ", "))
}

func cleanArchivePrefix(value string) (string, error) {
	value = strings.ReplaceAll(strings.TrimSpace(value), "\\", "/")
	if value == "" || value == "." {
		return "", nil
	}
	if strings.HasPrefix(value, "/") {
		return "", fmt.Errorf("file prefix must be relative to the volume")
	}
	for _, part := range strings.Split(value, "/") {
		if part == ".." {
			return "", fmt.Errorf("file prefix must not contain ..")
		}
	}
	clean := pathpkg.Clean(value)
	if clean == "." {
		return "", nil
	}
	return strings.TrimSuffix(clean, "/"), nil
}

func archiveVolumeIndex(archivePath, hash string) (*snapshot.FileIndex, error) {
	var idx snapshot.FileIndex
	err := portable.WithObject(archivePath, hash, func(raw io.Reader) error {
		zr, err := zstd.NewReader(raw)
		if err != nil {
			return err
		}
		defer zr.Close()
		if err := json.NewDecoder(zr).Decode(&idx); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if idx.Files == nil {
		idx.Files = map[string]snapshot.FileMeta{}
	}
	return &idx, nil
}

func printArchiveFiles(snapshotID, volume, prefix string, idx *snapshot.FileIndex) error {
	names := make([]string, 0, len(idx.Files))
	for name := range idx.Files {
		if prefix == "" || name == prefix || strings.HasPrefix(name, prefix+"/") {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	label := volume
	if prefix != "" {
		label += ":" + prefix
	}
	fmt.Printf("files in %s (%s, snapshot %s)\n", label, snapshot.HumanBytes(fileBytes(idx, names)), snapshotID)
	if len(names) == 0 {
		fmt.Println("  (no indexed files match)")
		return nil
	}
	for _, name := range names {
		meta := idx.Files[name]
		detail := snapshot.HumanBytes(meta.Size)
		if meta.Type == "symlink" || meta.Type == "hardlink" {
			detail = "→ " + meta.Link
		}
		mtime := time.Unix(meta.Mtime, 0).Local().Format("2006-01-02 15:04")
		fmt.Printf("  %-8s %10s  %s  %s\n", meta.Type, detail, mtime, name)
	}
	return nil
}

func fileBytes(idx *snapshot.FileIndex, names []string) int64 {
	var total int64
	for _, name := range names {
		if meta := idx.Files[name]; meta.Type == "file" {
			total += meta.Size
		}
	}
	return total
}
