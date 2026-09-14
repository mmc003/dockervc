// Applying a plan: sequential, fail-fast, honest summary. Applied steps stay
// on failure — every step is idempotent enough that re-running the same
// command is the documented recovery (image loads skip via the restore tag,
// clear+copy is idempotent, remove-then-create re-succeeds).
package rollback

import (
	"context"
	"fmt"
	"io"

	"dockervc/internal/dockerapi"
	"dockervc/internal/store"
)

// Executor applies a Plan to the engine. Out receives one line per step as
// it completes (the TUI captures stdout, so never prompt in here).
type Executor struct {
	Cli *dockerapi.Client
	St  *store.Store
	Out io.Writer
}

// Apply runs the plan in order. The first hard failure stops everything and
// is reported with completed-vs-failed counts; runtime warnings (create
// warnings, unclean stops) are appended to the plan's warning list for the
// caller to print.
func (e *Executor) Apply(ctx context.Context, p *Plan) error {
	// Containers are created under their deterministic restore tag
	// (<name>-restored-from-<snap>), so the hash→image binding lives in
	// the engine itself — no in-memory ID map to keep coherent.
	containerIDs := map[string]string{} // container name → newly created ID

	applied, skipped := 0, 0
	for i, s := range p.Steps {
		if s.Skip {
			skipped++
			fmt.Fprintf(e.Out, "  %2d. %s\n", i+1, s)
			continue
		}
		if err := e.applyStep(ctx, p, s, containerIDs); err != nil {
			return fmt.Errorf("%d of %d steps completed, failed at: %s: %w",
				i, len(p.Steps), s, err)
		}
		applied++
		fmt.Fprintf(e.Out, "  %2d. ✓ %s\n", i+1, s)
	}
	fmt.Fprintf(e.Out, "rollback applied: %d step(s)%s.\n", applied,
		pluralSkipped(skipped))
	return nil
}

func pluralSkipped(n int) string {
	if n == 0 {
		return ""
	}
	return fmt.Sprintf(" (%d skipped as already done)", n)
}

func (e *Executor) applyStep(ctx context.Context, p *Plan, s Step,
	containerIDs map[string]string) error {
	switch s.Kind {
	case StepRemoveContainer:
		// Graceful stop first; a stop failure (already stopped, wedge) is a
		// warning — the force-remove below is the real action.
		if err := e.Cli.StopContainer(ctx, s.ContainerID, 30); err != nil {
			p.Warnings = append(p.Warnings,
				fmt.Sprintf("container %s did not stop cleanly (%v); force-removing anyway", s.Name, err))
		}
		return e.Cli.ContainerRemove(ctx, s.ContainerID, true)

	case StepCreateNetwork:
		return e.Cli.NetworkCreateFromInspect(ctx, s.InspectJSON)

	case StepLoadImage:
		if s.ObjectHash == "" {
			// Tag-only step: the image is present (Name is its ref), just
			// recorded repo tags went missing.
			for _, ref := range s.Refs {
				if err := e.Cli.ImageTag(ctx, s.Name, ref); err != nil {
					return fmt.Errorf("re-tag as %s: %w", ref, err)
				}
			}
			return nil
		}
		id, err := e.loadObject(ctx, s.ObjectHash)
		if err != nil {
			return err
		}
		for _, ref := range s.Refs {
			if err := e.Cli.ImageTag(ctx, id, ref); err != nil {
				return fmt.Errorf("tag loaded image as %s: %w", ref, err)
			}
		}
		return nil

	case StepRestoreVolume:
		if err := e.Cli.VolumeCreate(ctx, s.Vol.Name, s.Vol.Driver, s.Vol.Options); err != nil {
			return err
		}
		obj, err := e.St.OpenObject(s.ObjectHash)
		if err != nil {
			return fmt.Errorf("open stored volume content: %w", err)
		}
		err = e.Cli.VolumeRestore(ctx, s.Vol.Name, obj)
		obj.Close()
		return err

	case StepCreateContainer:
		// The container runs under its deterministic restore tag — present
		// whether the image was just loaded or already existed.
		id, warns, err := e.Cli.ContainerCreateFromInspect(ctx, s.InspectJSON, RestoreTag(p.SnapshotID, s.Name))
		if err != nil {
			return err
		}
		containerIDs[s.Name] = id
		for _, w := range warns {
			p.Warnings = append(p.Warnings, fmt.Sprintf("container %s: %s", s.Name, w))
		}
		return nil

	case StepStartContainer:
		id := containerIDs[s.Name]
		if id == "" {
			return fmt.Errorf("no container named %s was created in this run", s.Name)
		}
		return e.Cli.StartContainer(ctx, id)
	}
	return fmt.Errorf("unknown step kind %d", s.Kind)
}

// loadObject streams one stored image object into the engine and returns the
// loaded image's ID.
func (e *Executor) loadObject(ctx context.Context, hash string) (string, error) {
	obj, err := e.St.OpenObject(hash)
	if err != nil {
		return "", fmt.Errorf("open stored image object: %w", err)
	}
	id, err := e.Cli.ImageLoadID(ctx, obj)
	obj.Close()
	return id, err
}
