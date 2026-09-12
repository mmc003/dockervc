// import: the receiving side of export. Three passes over the archive file —
// index (what is this?), adopt (verify + place objects verbatim), register
// (the snapshot row) — so nothing is written before everything about the
// import is known. --apply then chains the public rollback machinery against
// the imported snapshot, one confirmation covering the whole operation.
package cli

import (
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"dockervc/internal/dockerapi"
	"dockervc/internal/portable"
	"dockervc/internal/rollback"
	"dockervc/internal/snapshot"
)

var importOpts struct {
	apply bool
}

var importCmd = &cobra.Command{
	Use:   "import <archive.dvca>",
	Short: "Add an exported archive's snapshot to this machine's store",
	Long: `Add an exported archive's snapshot to this machine's store.

Every object is verified against its content hash while streaming in, then
adopted into the local content-addressed store verbatim (no recompression),
and the snapshot is registered — after which show/diff/rollback work exactly
as on the machine that exported it. Importing a snapshot that is already
here, unchanged, is a no-op. --apply additionally rolls the engine back to
the imported snapshot (one confirmation, pre-apply checkpoint by default).`,
	Args:        cobra.ExactArgs(1),
	Annotations: map[string]string{needsStore: "true"}, // init → import is the migration path
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()
		path := expandHome(args[0])

		// Pass 1 — index without touching the store: a broken or tampered
		// archive dies here, before a byte is written.
		ix, err := portable.IndexArchive(path)
		if err != nil {
			return fmt.Errorf("not a valid .dvca archive: %w", err)
		}
		m := ix.Manifest

		// Pre-flight print: everything import is about to do.
		var total int64
		presentLocally := 0
		seen := map[string]bool{}
		for _, h := range m.ObjectHashes() {
			if seen[h] {
				continue
			}
			seen[h] = true
			total += ix.ObjectSize(h)
			if _, err := os.Stat(st.ObjectPath(h)); err == nil {
				presentLocally++
			}
		}
		fmt.Printf("archive holds snapshot %s — %q\n", m.ID, m.Message)
		fmt.Printf("  created %s · %d object(s) · %s · %d already present locally\n",
			m.CreatedAt.Local().Format(time.RFC1123), ix.ObjectCount(),
			snapshot.HumanBytes(total), presentLocally)

		// Pass 2 — verify + adopt, streaming.
		adopted, present, err := portable.Adopt(st, ix, func(n, total int, hash string) {
			if n == total || n%10 == 1 {
				fmt.Printf("  verifying object %d/%d…\n", n, total)
			}
		})
		if err != nil {
			return fmt.Errorf("import failed: %w", err)
		}

		// Pass 3 — register (collision policy inside). An unchanged re-import
		// is a no-op for the store, but --apply still falls through below —
		// "roll the engine back to it" must hold either way.
		alreadyHere := false
		if err := portable.Register(st, ix); err != nil {
			if !errors.Is(err, portable.ErrAlreadyImported) {
				return err
			}
			alreadyHere = true
			fmt.Printf("already imported — %s is in this store unchanged (%d object(s) verified, 0 new).\n",
				m.ID, present)
		}
		if !alreadyHere {
			fmt.Printf("imported %s (%d new object(s), %d already present)\n", m.ID, adopted, present)
		}

		if !importOpts.apply {
			return nil
		}

		// --apply: the same sequence rollback.go runs, built only on the
		// public rollback API — fetch live state, plan the full restore, one
		// confirmation for the whole import+apply.
		dcli, err := dockerapi.New()
		if err != nil {
			return err
		}
		if err := dcli.Ping(ctx); err != nil {
			return fmt.Errorf("import succeeded, but the engine is unreachable for --apply: %w", err)
		}
		live, err := rollback.FetchLiveState(ctx, dcli)
		if err != nil {
			return fmt.Errorf("inspect live engine: %w", err)
		}
		steps, warnings := rollback.BuildPlan(m, live, rollback.Scope{All: true})
		plan := &rollback.Plan{SnapshotID: m.ID, Steps: steps, Warnings: warnings}
		if len(plan.Steps) == 0 {
			fmt.Println("Nothing to restore — the engine already matches the snapshot.")
			return nil
		}

		// Pre-apply checkpoint: identical guarantee to rollback's — the
		// restore itself is reversible. A failed checkpoint aborts.
		cap := &snapshot.Capturer{
			Cli: dcli, St: st,
			Opt: snapshot.Options{Message: "pre-apply checkpoint before imported " + m.ID},
		}
		cp, err := cap.Run(ctx)
		if err != nil {
			return fmt.Errorf("pre-apply checkpoint failed, aborting --apply (the import itself succeeded): %w", err)
		}
		fmt.Printf("pre-apply checkpoint %s created\n\n", cp.ID)
		for _, w := range cap.Warnings() {
			fmt.Fprintf(cmd.ErrOrStderr(), "warning: %s\n", w)
		}

		plan.WriteTo(os.Stdout)
		done := "Import done."
		if alreadyHere {
			done = "Already imported."
		}
		if !forceYes && !confirm(fmt.Sprintf("%s %s", done, rollbackPrompt(plan))) {
			fmt.Println("Aborted — the snapshot stays imported; restore it later with `dockervc rollback`.")
			return nil
		}

		ex := &rollback.Executor{Cli: dcli, St: st, Out: os.Stdout}
		if err := ex.Apply(ctx, plan); err != nil {
			printRollbackWarnings(cmd, plan)
			return err
		}
		fmt.Printf("\nEngine rolled back to %s.\n", m.ID)
		printRollbackWarnings(cmd, plan)
		return nil
	},
}

func init() {
	f := importCmd.Flags()
	f.BoolVar(&importOpts.apply, "apply", false,
		"after importing, roll the engine back to the snapshot (asks once)")
	f.BoolVarP(&forceYes, "yes", "y", false, "skip the --apply confirmation")
	rootCmd.AddCommand(importCmd)
}
