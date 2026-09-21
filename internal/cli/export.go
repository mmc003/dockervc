// export: package one snapshot into a single-file .dvca archive that any
// other machine's dockervc can import (verify + adopt + register). The
// archive is self-describing and self-verifying — checksums.sha256 lets
// stock `shasum -c` check an unpacked copy without dockervc at all.
package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"dockervc/internal/model"
	"dockervc/internal/portable"
	"dockervc/internal/snapshot"
)

var exportOpts struct {
	out    string
	latest bool
	split  string
	volume string
	image  string
	path   string
	raw    bool
}

var exportCmd = &cobra.Command{
	Use:   "export <snapshot|latest> [resource flags] [-o <file>]",
	Short: "Export a snapshot or one captured resource",
	Long: `Export a whole snapshot or one captured resource.

With no resource selector, the output is a portable .dvca archive: an
uncompressed outer tar holding the manifest, every object
it references (verbatim) and a GNU-sha256sum-style checksums file, so it can
be copied anywhere and imported with ` + "`dockervc import`" + `. Without -o it
lands in an exports/ folder in the current directory (created on demand) as
<snapshot-id>.dvca; set a different default folder with
` + "`dockervc config set export.folder <dir>`" + `.

Use --volume or --image to export one resource as a standard tar. Combine
--volume with --path for a filtered tar, or add --raw to emit one regular
file. Default selective outputs are organized below exports/<snapshot-id>/
and recorded in export-index.json. Selective outputs are not importable .dvca
snapshot bundles.`,
	Args:        cobra.MaximumNArgs(1),
	Annotations: map[string]string{needsStore: "true"},
	RunE: func(cmd *cobra.Command, args []string) error {
		if exportOpts.split != "" {
			return fmt.Errorf("--split is deferred; archives are single-file (use exFAT/APFS/NTFS destinations, or split(1) + cat on import)")
		}

		// Exactly one of a snapshot id (prefix-resolved like show/rollback)
		// or --latest.
		var m *model.Manifest
		switch {
		case exportOpts.latest && len(args) == 1:
			return fmt.Errorf("pass either a snapshot id or --latest, not both")
		case exportOpts.latest:
			latest, err := st.LatestSnapshot()
			if err != nil {
				return err
			}
			if latest == nil {
				return fmt.Errorf("no snapshots yet — take one first (`dockervc snapshot -m …`)")
			}
			m = latest
		case len(args) == 1 && strings.EqualFold(args[0], "latest"):
			latest, err := st.LatestSnapshot()
			if err != nil {
				return err
			}
			if latest == nil {
				return fmt.Errorf("no snapshots yet — take one first (`dockervc snapshot -m …`)")
			}
			m = latest
		case len(args) == 1:
			got, err := st.GetSnapshot(args[0])
			if err != nil {
				return err
			}
			m = got
		default:
			return fmt.Errorf("pass a snapshot id or --latest")
		}

		if exportOpts.volume != "" || exportOpts.image != "" || exportOpts.path != "" || exportOpts.raw {
			return exportSelected(cmd, m)
		}

		// A complete portable snapshot must have every referenced object.
		// Selective exports above only require (and authenticate) their chosen
		// object, which is useful when recovering data from a partially broken
		// snapshot.
		if missing := missingObjects(st, m); len(missing) > 0 {
			return fmt.Errorf("snapshot %s is broken: %d object file(s) missing — it cannot be exported (see `dockervc doctor`)",
				m.ID, len(missing))
		}

		out := exportOpts.out
		if out == "" {
			// export.folder setting when configured, else exports/<id>.dvca
			out = defaultExportPath(m.ID)
			fmt.Printf("no -o given — writing %s\n", out)
		}
		out = expandHome(out)
		// The destination folder is created on demand — exports/ (the
		// default) or any -o path shouldn't fail on a missing directory.
		if dir := filepath.Dir(out); dir != "." {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return fmt.Errorf("create %s: %w", dir, err)
			}
		}
		if _, err := os.Stat(out); err == nil && !forceYes {
			return fmt.Errorf("%s already exists — pass --yes to overwrite it", out)
		}

		// One loop answers both the size summary and the free-space check.
		var total int64
		n := 0
		seen := map[string]bool{}
		for _, h := range m.ObjectHashes() {
			if seen[h] {
				continue // the same bytes referenced twice export once
			}
			seen[h] = true
			fi, err := os.Stat(st.ObjectPath(h))
			if err != nil {
				return fmt.Errorf("object %s vanished mid-export: %w", h, err)
			}
			total += fi.Size()
			n++
		}
		// Tar overhead: one 512-byte header per entry, each padded to a
		// 512-byte boundary, plus slack for the archive footer.
		needed := total + int64(n+2)*1024 + 4096
		if free, ok := freeSpace(filepath.Dir(out)); ok && needed > int64(free) {
			return fmt.Errorf("%s has %s free, export needs ~%s — not enough space",
				filepath.Dir(out), snapshot.HumanBytes(int64(free)), snapshot.HumanBytes(needed))
		}

		// Stage next to the target, then rename: an interrupted export must
		// never leave a half-written file under the real name (a truncated
		// .dvca reads as corrupt — exactly what import's gate would say).
		tmp, err := os.CreateTemp(filepath.Dir(out), ".dvca-*")
		if err != nil {
			return err
		}
		tmpName := tmp.Name()
		cleanup := func() { // only on failure paths; rename consumes the file
			tmp.Close()
			os.Remove(tmpName)
		}
		exp := &portable.Exporter{
			St:       st,
			Reporter: commandProgress(cmd.ErrOrStderr()),
		}
		if err := exp.Export(m, tmp); err != nil {
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
		fi, _ := os.Stat(out)
		fmt.Printf("Exported %s → %s (%d object(s), %s)\n", m.ID, out, n, snapshot.HumanBytes(fi.Size()))
		return nil
	},
}

func init() {
	f := exportCmd.Flags()
	f.StringVarP(&exportOpts.out, "out", "o", "", "output path (default: organized under the export folder)")
	f.BoolVar(&exportOpts.latest, "latest", false, "export the newest snapshot")
	f.StringVar(&exportOpts.split, "split", "", "split the archive into parts (deferred)")
	f.StringVar(&exportOpts.volume, "volume", "", "export one named volume instead of the whole snapshot")
	f.StringVar(&exportOpts.image, "image", "", "export one image reference or digest instead of the whole snapshot")
	f.StringVar(&exportOpts.path, "path", "", "export one file or directory from the selected volume")
	f.BoolVar(&exportOpts.raw, "raw", false, "write one selected regular file without a tar wrapper")
	f.BoolVarP(&forceYes, "yes", "y", false, "overwrite an existing output file")
	rootCmd.AddCommand(exportCmd)
}

// expandHome resolves a leading ~ to the user's home directory; other paths
// pass through untouched.
func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	return p
}
