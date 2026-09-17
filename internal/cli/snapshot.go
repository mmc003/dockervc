package cli

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"dockervc/internal/dockerapi"
	"dockervc/internal/model"
	"dockervc/internal/snapshot"
)

var snapOpts struct {
	message    string
	stop       bool
	only       []string
	anonymous  bool
	bindMounts bool
}

var validScopes = map[string]bool{
	"containers": true, "volumes": true, "images": true, "networks": true, "bindmounts": true,
}

var snapshotCmd = &cobra.Command{
	Use:         "snapshot",
	Aliases:     []string{"commit"},
	Short:       "Capture the current Docker engine state as a snapshot",
	Args:        cobra.NoArgs,
	Annotations: map[string]string{needsStore: "true"},
	RunE: func(cmd *cobra.Command, args []string) error {
		for _, scope := range snapOpts.only {
			if !validScopes[scope] {
				return fmt.Errorf("invalid --only scope %q (valid: containers, volumes, images, networks, bindmounts)", scope)
			}
		}

		cli, err := dockerapi.New()
		if err != nil {
			return err
		}
		if err := cli.Ping(cmd.Context()); err != nil {
			return fmt.Errorf("cannot reach docker engine: %w", err)
		}

		capturer := &snapshot.Capturer{
			Cli:      cli,
			St:       st,
			Progress: commandProgress(cmd.ErrOrStderr()),
			Opt: snapshot.Options{
				Message:           snapOpts.message,
				Stop:              snapOpts.stop,
				Only:              snapOpts.only,
				IncludeAnonymous:  snapOpts.anonymous,
				IncludeBindMounts: snapOpts.bindMounts,
			},
		}
		m, err := capturer.Run(cmd.Context())
		if err != nil {
			return err
		}

		fmt.Printf("\nSnapshot %s created (%s) — %d container(s), %d image(s), %d volume(s), %d network(s)\n",
			m.ID, snapshot.HumanBytes(m.TotalSize),
			len(m.Containers), len(m.Images), len(m.Volumes), len(m.Networks))
		fmt.Printf("Storage: %d new object(s) (%s), %d reused from earlier snapshots\n",
			m.Stats.NewObjects, snapshot.HumanBytes(m.Stats.NewBytes), m.Stats.ReusedObjects)
		for _, w := range capturer.Warnings() {
			fmt.Fprintf(cmd.ErrOrStderr(), "warning: %s\n", w)
		}
		return nil
	},
}

var logCmd = &cobra.Command{
	Use:         "log",
	Short:       "List snapshots",
	Args:        cobra.NoArgs,
	Annotations: map[string]string{needsStore: "true"},
	RunE: func(cmd *cobra.Command, args []string) error {
		rows, err := st.ListSnapshots()
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			fmt.Println("No snapshots yet. Create one with: dockervc snapshot -m \"first\"")
			return nil
		}
		fmt.Printf("%-26s  %-19s  %-10s  %s\n", "SNAPSHOT", "WHEN", "SIZE", "MESSAGE")
		for i := len(rows) - 1; i >= 0; i-- { // newest first, like git log
			r := rows[i]
			line := fmt.Sprintf("%-26s  %-19s  %-10s  %s",
				r.ID, r.CreatedAt.Local().Format(time.DateTime), snapshot.HumanBytes(r.Size), r.Message)
			if mark := BrokenSuffix(st, r); mark != "" {
				line += "  " + mark
			}
			fmt.Println(line)
		}
		return nil
	},
}

var showCmd = &cobra.Command{
	Use:         "show <snapshot>",
	Short:       "Show the contents of a snapshot",
	Args:        cobra.ExactArgs(1),
	Annotations: map[string]string{needsStore: "true"},
	RunE: func(cmd *cobra.Command, args []string) error {
		m, err := st.GetSnapshot(args[0])
		if err != nil {
			return err
		}
		consistent := "crash-consistent"
		if m.Consistent {
			consistent = "app-consistent (containers stopped)"
		}
		storageInfo, err := st.SnapshotStorageInfo(m.ID)
		if err != nil {
			return fmt.Errorf("calculate snapshot storage: %w", err)
		}
		fmt.Printf("snapshot %s\n", m.ID)
		fmt.Printf("  created:        %s\n", m.CreatedAt.Local().Format(time.RFC1123))
		fmt.Printf("  message:        %s\n", m.Message)
		fmt.Printf("  engine:         docker %s (engine %s)\n", m.DockerVersion, shortID(m.EngineID))
		fmt.Printf("  consistency:    %s\n", consistent)
		fmt.Printf("  capture:        %d new object(s) (%s), %d reused object(s)\n",
			m.Stats.NewObjects, snapshot.HumanBytes(m.Stats.NewBytes), m.Stats.ReusedObjects)
		fmt.Printf("\nstorage:\n")
		fmt.Printf("  referenced:     %s across %d object(s)\n",
			snapshot.HumanBytes(storageInfo.ReferencedBytes), storageInfo.ReferencedObjects)
		fmt.Printf("  exclusive:      %s across %d object(s) (reclaimable after delete + prune)\n",
			snapshot.HumanBytes(storageInfo.ExclusiveBytes), storageInfo.ExclusiveObjects)
		fmt.Printf("  shared:         %s across %d object(s) (reused by other snapshots)\n",
			snapshot.HumanBytes(storageInfo.SharedBytes), storageInfo.SharedObjects)

		if len(m.Containers) > 0 {
			fmt.Printf("\ncontainers:\n")
			for _, c := range m.Containers {
				state := "stopped"
				if c.Running {
					state = "running"
				}
				fmt.Printf("  %-30s %-8s %s\n", c.Name, state, snapshot.HumanBytes(c.Size))
			}
		}
		if len(m.Volumes) > 0 {
			fmt.Printf("\nvolumes:\n")
			for _, v := range m.Volumes {
				fmt.Printf("  %-30s driver=%s %s\n", v.Name, v.Driver, snapshot.HumanBytes(v.Size))
			}
		}
		if len(m.Images) > 0 {
			fmt.Printf("\nimages:\n")
			for _, im := range m.Images {
				ref := strings.Join(im.Refs, ", ")
				if ref == "" {
					ref = "(dangling)"
				}
				fmt.Printf("  %-30s %s\n", ref, snapshot.HumanBytes(im.Size))
			}
		}
		if len(m.Networks) > 0 {
			fmt.Printf("\nnetworks:\n")
			for _, n := range m.Networks {
				fmt.Printf("  %s\n", n.Name)
			}
		}
		if len(m.BindMounts) > 0 {
			fmt.Printf("\nbind mounts:\n")
			for _, b := range m.BindMounts {
				fmt.Printf("  %-30s %s\n", b.HostPath, snapshot.HumanBytes(b.Size))
			}
		}
		return nil
	},
}

var deleteCmd = &cobra.Command{
	Use:         "delete <snapshot>…",
	Short:       "Delete one or more snapshots (objects are reclaimed by prune)",
	Args:        cobra.MinimumNArgs(1),
	Annotations: map[string]string{needsStore: "true"},
	RunE: func(cmd *cobra.Command, args []string) error {
		// Resolve every id up front (prefixes allowed, duplicates collapsed)
		// so one bad name deletes nothing.
		var manifests []*model.Manifest
		seen := map[string]bool{}
		for _, arg := range args {
			m, err := st.GetSnapshot(arg)
			if err != nil {
				return err
			}
			if seen[m.ID] {
				continue
			}
			seen[m.ID] = true
			manifests = append(manifests, m)
		}
		if !forceYes {
			what := fmt.Sprintf("snapshot %s (%q)", manifests[0].ID, manifests[0].Message)
			if len(manifests) > 1 {
				names := make([]string, len(manifests))
				for i, m := range manifests {
					names[i] = fmt.Sprintf("%s (%q)", m.ID, m.Message)
				}
				what = fmt.Sprintf("%d snapshots: %s", len(manifests), strings.Join(names, ", "))
			}
			if !confirm("Delete " + what + "?") {
				fmt.Println("Aborted.")
				return nil
			}
		}
		var fail int
		for _, m := range manifests {
			if err := st.DeleteSnapshot(m.ID); err != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "failed to delete %s: %v\n", m.ID, err)
				fail++
			}
		}
		if fail > 0 {
			return fmt.Errorf("%d of %d snapshot(s) failed to delete", fail, len(manifests))
		}
		ids := make([]string, len(manifests))
		for i, m := range manifests {
			ids[i] = m.ID
		}
		fmt.Printf("Deleted %s. Run `dockervc prune` to reclaim unreferenced objects.\n",
			strings.Join(ids, ", "))
		return nil
	},
}

func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

func init() {
	snapshotCmd.Flags().StringVarP(&snapOpts.message, "message", "m", "", "snapshot description")
	snapshotCmd.Flags().BoolVar(&snapOpts.stop, "stop", false,
		"stop containers before capture for app-consistent data (restarts them after)")
	snapshotCmd.Flags().StringSliceVar(&snapOpts.only, "only", nil,
		"comma-separated subset to capture: containers,volumes,images,networks,bindmounts")
	snapshotCmd.Flags().BoolVar(&snapOpts.anonymous, "include-anonymous", false,
		"also capture anonymous volumes")
	snapshotCmd.Flags().BoolVar(&snapOpts.bindMounts, "include-bind-mounts", false,
		"also archive host bind-mount paths (requires running on the Docker host)")

	rootCmd.AddCommand(snapshotCmd)
	rootCmd.AddCommand(logCmd)
	rootCmd.AddCommand(showCmd)
	rootCmd.AddCommand(deleteCmd)

	// shared flag
	deleteCmd.Flags().BoolVarP(&forceYes, "yes", "y", false, "skip confirmation")
}
