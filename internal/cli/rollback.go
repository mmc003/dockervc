// rollback: restore the engine (fully or partially) to a snapshot. The plan
// is built up front and dry-runnable; a pre-rollback checkpoint (a normal
// snapshot of the current state) is taken by default so every rollback is
// itself reversible; application is sequential and fail-fast.
package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"dockervc/internal/dockerapi"
	"dockervc/internal/model"
	"dockervc/internal/rollback"
	"dockervc/internal/snapshot"
)

var rollbackOpts struct {
	all         bool
	containers  []string
	volumes     []string
	images      []string
	networks    []string
	dryRun      bool
	keepCurrent bool
	reuseByName bool
}

var rollbackCmd = &cobra.Command{
	Use:   "rollback <snapshot>",
	Short: "Restore the engine (fully or partially) to a snapshot",
	Long: `Restore the engine (fully or partially) to a snapshot.

Selects entities by name (plus their dependencies: a container pulls in its
filesystem image, mounted volumes and networks), or --all for everything.
Existing container configuration and required volumes are compared first.
Unchanged entities are reused, changed volume contents are replaced exactly
(clear-then-copy), and missing entities are created. Containers are recreated
only when their configuration or image changed. Unless --keep-current, a
pre-rollback checkpoint snapshot is taken first, so the rollback itself can
be rolled back.`,
	Args:        cobra.ExactArgs(1),
	Annotations: map[string]string{needsStore: "true"},
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()
		dcli, err := dockerapi.New()
		if err != nil {
			return err
		}
		if err := dcli.Ping(ctx); err != nil {
			return fmt.Errorf("cannot reach docker engine: %w", err)
		}

		m, err := st.GetSnapshot(args[0])
		if err != nil {
			return err
		}

		// Broken snapshots restore nothing — refuse before any planning, the
		// same check doctor reports with.
		if missing := missingObjects(st, m); len(missing) > 0 {
			return fmt.Errorf("snapshot %s is broken: %d object file(s) missing — it cannot be restored (see `dockervc doctor`)",
				m.ID, len(missing))
		}

		scope, err := resolveRollbackScope(m)
		if err != nil {
			return err
		}
		if rollbackOpts.reuseByName {
			if (!scope.All && len(scope.Containers) == 0) ||
				len(rollbackOpts.volumes)+len(rollbackOpts.images)+len(rollbackOpts.networks) > 0 {
				return fmt.Errorf("--reuse-existing-by-name applies only to selected containers; use --containers or --all")
			}
		}

		live, err := rollback.FetchLiveState(ctx, dcli)
		if err != nil {
			return fmt.Errorf("inspect live engine: %w", err)
		}
		if rollbackOpts.reuseByName {
			scope.ReuseExistingByName = true
			if err := rollback.ValidateReuseExistingByName(m, live, scope); err != nil {
				return err
			}
		} else {
			if err := rollback.ReconcileVolumes(ctx, dcli.VolumeTarStream, st.OpenObject, m, scope, live,
				commandProgress(cmd.ErrOrStderr())); err != nil {
				return fmt.Errorf("compare required volumes: %w", err)
			}
		}

		steps, warnings := rollback.BuildPlan(m, live, scope)
		plan := &rollback.Plan{SnapshotID: m.ID, Steps: steps, Warnings: warnings}
		if len(plan.Steps) == 0 {
			fmt.Println("Nothing to restore — the engine already matches the selection.")
			return nil
		}

		if rollbackOpts.dryRun {
			plan.WriteTo(os.Stdout)
			return nil
		}

		// Pre-rollback checkpoint: every rollback is itself reversible. A
		// failed checkpoint aborts — restoring over an unrecorded state would
		// be the one truly destructive thing this command can do.
		if !rollbackOpts.keepCurrent {
			cap := &snapshot.Capturer{
				Cli: dcli, St: st, Progress: commandProgress(cmd.ErrOrStderr()),
				Opt: snapshot.Options{Message: "pre-rollback checkpoint before " + m.ID},
			}
			cp, err := cap.Run(ctx)
			if err != nil {
				return fmt.Errorf("pre-rollback checkpoint failed, aborting rollback: %w", err)
			}
			fmt.Printf("pre-rollback checkpoint %s created (--keep-current skips this)\n\n", cp.ID)
			for _, w := range cap.Warnings() {
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: %s\n", w)
			}
		}

		plan.WriteTo(os.Stdout)

		if !forceYes && !confirm(rollbackPrompt(plan)) {
			fmt.Println("Aborted.")
			return nil
		}

		ex := &rollback.Executor{
			Cli: dcli, St: st, Out: os.Stdout,
			Progress: commandProgress(cmd.ErrOrStderr()),
		}
		if err := ex.Apply(ctx, plan); err != nil {
			printRollbackWarnings(cmd, plan)
			return err
		}
		fmt.Printf("\nEngine rolled back to %s.\n", m.ID)
		printRollbackWarnings(cmd, plan)
		return nil
	},
}

// rollbackPrompt names the destructive parts explicitly — confirmation IS
// the force flag, so the prompt must carry what "yes" means.
func rollbackPrompt(p *rollback.Plan) string {
	var removes, refills int
	for _, s := range p.Steps {
		switch s.Kind {
		case rollback.StepRemoveContainer:
			removes++
		case rollback.StepRestoreVolume:
			refills++
		}
	}
	return fmt.Sprintf("Restore snapshot %s — %d step(s), including %d container removal(s) and %d volume content replacement(s)?",
		p.SnapshotID, len(p.Steps), removes, refills)
}

func printRollbackWarnings(cmd *cobra.Command, p *rollback.Plan) {
	for _, w := range p.Warnings {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: %s\n", w)
	}
}

// resolveRollbackScope turns the scope flags into a Scope, prompting per
// kind when no scope flags were given at all.
func resolveRollbackScope(m *model.Manifest) (rollback.Scope, error) {
	if rollbackOpts.all || len(rollbackOpts.containers) > 0 || len(rollbackOpts.volumes) > 0 ||
		len(rollbackOpts.images) > 0 || len(rollbackOpts.networks) > 0 {
		return rollback.ParseScopeArgs(m, rollbackOpts.all,
			rollbackOpts.containers, rollbackOpts.volumes,
			rollbackOpts.images, rollbackOpts.networks)
	}

	if tuiActive {
		// confirm() is suppressed in the TUI's raw mode; asking would hang
		// invisibly. The ':' mode requires an explicit scope.
		return rollback.Scope{}, fmt.Errorf("pass a scope inside the TUI: --all, or --containers/--volumes/--images/--networks with names")
	}

	var sc rollback.Scope
	if len(m.Containers) > 0 && confirm(fmt.Sprintf("Restore all %d container(s)?", len(m.Containers))) {
		for _, c := range m.Containers {
			sc.Containers = append(sc.Containers, c.Name)
		}
	}
	if len(m.Volumes) > 0 && confirm(fmt.Sprintf("Restore all %d volume(s)?", len(m.Volumes))) {
		for _, v := range m.Volumes {
			sc.Volumes = append(sc.Volumes, v.Name)
		}
	}
	if len(m.Images) > 0 && confirm(fmt.Sprintf("Restore all %d image(s)?", len(m.Images))) {
		for _, img := range m.Images {
			if len(img.Refs) > 0 {
				sc.Images = append(sc.Images, img.Refs[0])
			} else {
				sc.Images = append(sc.Images, img.Digest)
			}
		}
	}
	if len(m.Networks) > 0 && confirm(fmt.Sprintf("Restore all %d network(s)?", len(m.Networks))) {
		for _, n := range m.Networks {
			sc.Networks = append(sc.Networks, n.Name)
		}
	}
	if len(sc.Containers)+len(sc.Volumes)+len(sc.Images)+len(sc.Networks) == 0 {
		return sc, fmt.Errorf("no entities selected — pass --all or --containers/--volumes/--images/--networks with names")
	}
	return rollback.ParseScopeArgs(m, false,
		sc.Containers, sc.Volumes, sc.Images, sc.Networks)
}

func init() {
	f := rollbackCmd.Flags()
	f.BoolVar(&rollbackOpts.all, "all", false, "restore everything captured in the snapshot")
	f.StringSliceVar(&rollbackOpts.containers, "containers", nil, "comma-separated container names to restore")
	f.StringSliceVar(&rollbackOpts.volumes, "volumes", nil, "comma-separated volume names to restore")
	f.StringSliceVar(&rollbackOpts.images, "images", nil, "comma-separated image refs or digests to restore")
	f.StringSliceVar(&rollbackOpts.networks, "networks", nil, "comma-separated network names to restore")
	f.BoolVar(&rollbackOpts.dryRun, "dry-run", false, "print the restore plan and exit without changing anything")
	f.BoolVar(&rollbackOpts.keepCurrent, "keep-current", false, "skip the pre-rollback checkpoint snapshot")
	f.BoolVar(&rollbackOpts.reuseByName, "reuse-existing-by-name", false,
		"create missing containers using existing named dependencies without comparing or replacing them")
	f.BoolVarP(&forceYes, "yes", "y", false, "skip confirmation")
	rootCmd.AddCommand(rollbackCmd)
}
