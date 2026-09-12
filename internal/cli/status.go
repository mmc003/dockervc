package cli

import (
	"fmt"
	"sort"

	"github.com/spf13/cobra"

	"dockervc/internal/dockerapi"
	"dockervc/internal/model"
	"dockervc/internal/snapshot"
)

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Compare the live engine against the latest snapshot",
	Args:  cobra.NoArgs,
	Annotations: map[string]string{needsStore: "true"},
	RunE: func(cmd *cobra.Command, args []string) error {
		latest, err := st.LatestSnapshot()
		if err != nil {
			return err
		}
		if latest == nil {
			fmt.Println("No snapshots yet — nothing to compare against.")
			return nil
		}
		fmt.Printf("Comparing live engine against snapshot %s (%s)\n\n",
			latest.ID, latest.Message)

		cli, err := dockerapi.New()
		if err != nil {
			return err
		}
		drift, err := snapshot.ComputeDrift(cmd.Context(), cli, latest)
		if err != nil {
			return err
		}
		printDrift(drift)
		return nil
	},
}

var diffFiles string

var diffCmd = &cobra.Command{
	Use:   "diff <snapshotA> [snapshotB]",
	Short: "Compare two snapshots (or a snapshot vs. the live engine)",
	Args:  cobra.RangeArgs(1, 2),
	Annotations: map[string]string{needsStore: "true"},
	RunE: func(cmd *cobra.Command, args []string) error {
		a, err := st.GetSnapshot(args[0])
		if err != nil {
			return err
		}

		if diffFiles != "" {
			if len(args) != 2 {
				return fmt.Errorf("--files compares two snapshots: diff <snapA> <snapB> --files <volume>")
			}
			b, err := st.GetSnapshot(args[1])
			if err != nil {
				return err
			}
			return printVolumeFileDiff(a, b, diffFiles)
		}

		var drift *snapshot.Drift
		if len(args) == 2 {
			b, err := st.GetSnapshot(args[1])
			if err != nil {
				return err
			}
			drift = snapshot.DiffManifests(a, b, st.OpenObject)
			fmt.Printf("Comparing %s → %s\n\n", a.ID, b.ID)
		} else {
			cli, err := dockerapi.New()
			if err != nil {
				return err
			}
			drift, err = snapshot.ComputeDrift(cmd.Context(), cli, a)
			if err != nil {
				return err
			}
			fmt.Printf("Comparing %s → live engine\n\n", a.ID)
		}
		printDrift(drift)
		return nil
	},
}

func printDrift(d *snapshot.Drift) {
	for _, section := range []snapshot.DriftSection{d.Containers, d.Volumes, d.Images} {
		if section.Empty() {
			fmt.Printf("%s: unchanged\n", section.Kind)
			continue
		}
		fmt.Printf("%s:\n", section.Kind)
		for _, name := range section.Added {
			fmt.Printf("  + %s\n", name)
		}
		for _, name := range section.Changed {
			fmt.Printf("  ~ %s\n", name)
		}
		for _, name := range section.Removed {
			fmt.Printf("  - %s\n", name)
		}
	}
	if d.Empty() {
		fmt.Println("\nNo drift — engine matches the snapshot.")
	}
}

// ── per-file volume diff (--files) ───────────────────────────────────────────

func findVolume(m *model.Manifest, name string) (model.VolumeRecord, bool) {
	for _, r := range m.Volumes {
		if r.Name == name {
			return r, true
		}
	}
	return model.VolumeRecord{}, false
}

// fileEntry is one rendered line of the file diff.
type fileEntry struct {
	status string // A | M | D
	path   string
	meta   snapshot.FileMeta
}

// printVolumeFileDiff lists, git name-status style, every file created,
// modified or deleted in one volume between two snapshots.
func printVolumeFileDiff(a, b *model.Manifest, vol string) error {
	recA, inA := findVolume(a, vol)
	recB, inB := findVolume(b, vol)
	if !inA && !inB {
		return fmt.Errorf("volume %q appears in neither snapshot", vol)
	}
	if inA && !inB {
		fmt.Printf("volume %s was deleted in %s — all its files:\n\n", vol, b.ID)
	} else if !inA && inB {
		fmt.Printf("volume %s was created in %s — all its files:\n\n", vol, b.ID)
	} else {
		fmt.Printf("volume %s, %s → %s:\n\n", vol, a.ID, b.ID)
	}

	var entries []fileEntry
	switch {
	case inA && inB:
		ia, err := snapshot.LoadFileIndex(st.OpenObject, recA)
		if err != nil {
			return err
		}
		ib, err := snapshot.LoadFileIndex(st.OpenObject, recB)
		if err != nil {
			return err
		}
		fc := snapshot.DiffIndexes(vol, ia, ib)
		if fc == nil {
			fmt.Println("  file detail unavailable — at least one of these snapshots")
			fmt.Println("  predates file indexing. Plain `diff` still reports the change;")
			fmt.Println("  take a fresh snapshot to start collecting file indexes.")
			return nil
		}
		if fc.Empty() {
			fmt.Println("  no file changes (only volume metadata/timestamps moved)")
			return nil
		}
		for _, p := range fc.Created {
			entries = append(entries, fileEntry{"A", p, ib.Files[p]})
		}
		for _, p := range fc.Modified {
			entries = append(entries, fileEntry{"M", p, ib.Files[p]})
		}
		for _, p := range fc.Deleted {
			entries = append(entries, fileEntry{"D", p, ia.Files[p]})
		}
	case inB: // created: everything from B's index
		idx, err := snapshot.LoadFileIndex(st.OpenObject, recB)
		if err != nil {
			return err
		}
		for p, meta := range idx.Files {
			entries = append(entries, fileEntry{"A", p, meta})
		}
	default: // deleted: everything from A's index
		idx, err := snapshot.LoadFileIndex(st.OpenObject, recA)
		if err != nil {
			return err
		}
		for p, meta := range idx.Files {
			entries = append(entries, fileEntry{"D", p, meta})
		}
	}

	sort.Slice(entries, func(i, j int) bool { return entries[i].path < entries[j].path })
	for _, e := range entries {
		fmt.Println(fileStatusLine(e))
	}
	fmt.Printf("\n%d file(s)\n", len(entries))
	return nil
}

func fileStatusLine(e fileEntry) string {
	if e.meta.Type == "symlink" || e.meta.Type == "hardlink" {
		return fmt.Sprintf("  %s  %s → %s", e.status, e.path, e.meta.Link)
	}
	return fmt.Sprintf("  %s  %-52s %8s", e.status, e.path, snapshot.HumanBytes(e.meta.Size))
}

func init() {
	diffCmd.Flags().StringVar(&diffFiles, "files", "",
		"list per-file changes for one volume (requires two snapshots)")
}
