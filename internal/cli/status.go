package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"dockervc/internal/dockerapi"
	"dockervc/internal/model"
	"dockervc/internal/snapshot"
)

var statusOpts struct {
	deep    bool
	volumes []string
}

var statusCmd = &cobra.Command{
	Use:         "status",
	Short:       "Compare the live engine against the latest snapshot",
	Args:        cobra.NoArgs,
	Annotations: map[string]string{needsStore: "true"},
	RunE: func(cmd *cobra.Command, args []string) error {
		if len(statusOpts.volumes) > 0 && !statusOpts.deep {
			return fmt.Errorf("--volumes requires --deep")
		}
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
		var deepErr error
		if statusOpts.deep {
			deepErr = addLiveVolumeDrift(cmd.Context(), cli.VolumeTarStream, latest,
				drift, statusOpts.volumes, st.OpenObject, cmd.ErrOrStderr())
		}
		printDrift(drift)
		return deepErr
	},
}

var diffFiles string

var diffCmd = &cobra.Command{
	Use:         "diff <snapshotA> [snapshotB]",
	Short:       "Compare two snapshots (or a snapshot vs. the live engine)",
	Args:        cobra.RangeArgs(1, 2),
	Annotations: map[string]string{needsStore: "true"},
	RunE: func(cmd *cobra.Command, args []string) error {
		a, err := st.GetSnapshot(args[0])
		if err != nil {
			return err
		}

		if diffFiles != "" {
			if len(args) == 2 {
				b, err := st.GetSnapshot(args[1])
				if err != nil {
					return err
				}
				return printVolumeFileDiff(a, b, diffFiles)
			}
			cli, err := dockerapi.New()
			if err != nil {
				return err
			}
			return printLiveVolumeFileDiff(cmd.Context(), cli, a, diffFiles, cmd.ErrOrStderr())
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
		for _, name := range section.Unavailable {
			fmt.Printf("  ? %s\n", name)
		}
	}
	if d.Empty() {
		fmt.Println("\nNo drift — engine matches the snapshot.")
	}
}

// addLiveVolumeDrift enriches the fast name-only volume inventory with file
// changes from read-only live scans. It finishes every selected volume and
// joins scan errors so one failure does not hide later results.
func addLiveVolumeDrift(ctx context.Context, openTar snapshot.VolumeTarOpener,
	m *model.Manifest, drift *snapshot.Drift, selected []string,
	openBlob snapshot.BlobOpener, progress io.Writer) error {

	records := make(map[string]model.VolumeRecord, len(m.Volumes))
	for _, rec := range m.Volumes {
		records[rec.Name] = rec
	}
	added := make(map[string]bool, len(drift.Volumes.Added))
	for _, name := range drift.Volumes.Added {
		added[name] = true
	}
	removed := make(map[string]bool, len(drift.Volumes.Removed))
	for _, name := range drift.Volumes.Removed {
		removed[name] = true
	}

	wanted := map[string]bool{}
	if len(selected) == 0 {
		for name := range records {
			wanted[name] = true
		}
	} else {
		var unknown []string
		for _, raw := range selected {
			name := strings.TrimSpace(raw)
			if name == "" || wanted[name] {
				continue
			}
			if _, inSnapshot := records[name]; !inSnapshot && !added[name] {
				unknown = append(unknown, name)
				continue
			}
			wanted[name] = true
		}
		if len(unknown) > 0 {
			sort.Strings(unknown)
			return fmt.Errorf("volume(s) not found in the snapshot or live engine: %s", strings.Join(unknown, ", "))
		}
	}

	names := make([]string, 0, len(wanted))
	for name := range wanted {
		if _, inSnapshot := records[name]; inSnapshot && !removed[name] {
			names = append(names, name)
		}
	}
	sort.Strings(names)

	var scanErrs []error
	for _, name := range names {
		rec := records[name]
		if rec.IndexObject == "" {
			drift.Volumes.Unavailable = append(drift.Volumes.Unavailable,
				name+" (file comparison unavailable; take a new snapshot)")
			continue
		}
		fmt.Fprintf(progress, "scanning volume %s...\n", name)
		fc, _, _, err := snapshot.DiffLiveVolume(ctx, openTar, openBlob, rec)
		if err != nil {
			drift.Volumes.Unavailable = append(drift.Volumes.Unavailable,
				fmt.Sprintf("%s (scan failed: %v)", name, err))
			scanErrs = append(scanErrs, fmt.Errorf("scan volume %s: %w", name, err))
			continue
		}
		if fc != nil && !fc.Empty() {
			drift.Volumes.Changed = append(drift.Volumes.Changed,
				name+" ("+fc.Summarize()+")")
		}
	}
	sort.Strings(drift.Volumes.Changed)
	sort.Strings(drift.Volumes.Unavailable)
	return errors.Join(scanErrs...)
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
		if idx == nil {
			printFileIndexUnavailable()
			return nil
		}
		for p, meta := range idx.Files {
			entries = append(entries, fileEntry{"A", p, meta})
		}
	default: // deleted: everything from A's index
		idx, err := snapshot.LoadFileIndex(st.OpenObject, recA)
		if err != nil {
			return err
		}
		if idx == nil {
			printFileIndexUnavailable()
			return nil
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

// printLiveVolumeFileDiff lists path-level changes between a snapshot and one
// live volume. Added/deleted volumes are rendered as all-A/all-D respectively.
func printLiveVolumeFileDiff(ctx context.Context, cli *dockerapi.Client,
	a *model.Manifest, vol string, progress io.Writer) error {

	recA, inA := findVolume(a, vol)
	vols, err := cli.ListVolumes(ctx)
	if err != nil {
		return err
	}
	inLive := false
	for _, v := range vols {
		if v.Name == vol {
			inLive = true
			break
		}
	}
	if !inA && !inLive {
		return fmt.Errorf("volume %q appears in neither snapshot %s nor the live engine", vol, a.ID)
	}

	var before, live *snapshot.FileIndex
	var entries []fileEntry
	switch {
	case inA && inLive:
		before, err = snapshot.LoadFileIndex(st.OpenObject, recA)
		if err != nil {
			return err
		}
		fmt.Printf("volume %s, %s → live:\n\n", vol, a.ID)
		if before == nil {
			printFileIndexUnavailable()
			return nil
		}
		fmt.Fprintf(progress, "scanning volume %s...\n", vol)
		live, err = snapshot.IndexLiveVolume(ctx, cli.VolumeTarStream, vol)
		if err != nil {
			return err
		}
		fc := snapshot.DiffIndexes(vol, before, live)
		if fc.Empty() {
			fmt.Println("  no file changes")
			return nil
		}
		for _, p := range fc.Created {
			entries = append(entries, fileEntry{"A", p, live.Files[p]})
		}
		for _, p := range fc.Modified {
			entries = append(entries, fileEntry{"M", p, live.Files[p]})
		}
		for _, p := range fc.Deleted {
			entries = append(entries, fileEntry{"D", p, before.Files[p]})
		}
	case inLive:
		fmt.Printf("volume %s was created in the live engine — all its files:\n\n", vol)
		fmt.Fprintf(progress, "scanning volume %s...\n", vol)
		live, err = snapshot.IndexLiveVolume(ctx, cli.VolumeTarStream, vol)
		if err != nil {
			return err
		}
		for p, meta := range live.Files {
			entries = append(entries, fileEntry{"A", p, meta})
		}
	default:
		fmt.Printf("volume %s was deleted from the live engine — all its files:\n\n", vol)
		before, err = snapshot.LoadFileIndex(st.OpenObject, recA)
		if err != nil {
			return err
		}
		if before == nil {
			printFileIndexUnavailable()
			return nil
		}
		for p, meta := range before.Files {
			entries = append(entries, fileEntry{"D", p, meta})
		}
	}

	printFileEntries(entries)
	return nil
}

func printFileIndexUnavailable() {
	fmt.Println("  file detail unavailable — the snapshot predates file indexing.")
	fmt.Println("  Take a fresh snapshot to start collecting file indexes.")
}

func printFileEntries(entries []fileEntry) {
	sort.Slice(entries, func(i, j int) bool { return entries[i].path < entries[j].path })
	for _, e := range entries {
		fmt.Println(fileStatusLine(e))
	}
	fmt.Printf("\n%d file(s)\n", len(entries))
}

func fileStatusLine(e fileEntry) string {
	if e.meta.Type == "symlink" || e.meta.Type == "hardlink" {
		return fmt.Sprintf("  %s  %s → %s", e.status, e.path, e.meta.Link)
	}
	return fmt.Sprintf("  %s  %-52s %8s", e.status, e.path, snapshot.HumanBytes(e.meta.Size))
}

func init() {
	statusCmd.Flags().BoolVar(&statusOpts.deep, "deep", false,
		"scan live volume contents and compare them with the latest snapshot")
	statusCmd.Flags().StringSliceVar(&statusOpts.volumes, "volumes", nil,
		"with --deep, scan only these comma-separated volume names")
	diffCmd.Flags().StringVar(&diffFiles, "files", "",
		"list per-file changes for one volume (snapshot-to-snapshot or snapshot-to-live)")
}
