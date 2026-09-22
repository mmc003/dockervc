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
	"dockervc/internal/progress"
	"dockervc/internal/store"
)

// Executor applies a Plan to the engine. Out receives one line per step as
// it completes (the TUI captures stdout, so never prompt in here).
type Executor struct {
	Cli      *dockerapi.Client
	St       *store.Store
	Out      io.Writer
	Progress progress.Reporter

	meter *progress.Meter
}

// Apply runs the plan in order. The first hard failure stops everything and
// is reported with completed-vs-failed counts; runtime warnings (create
// warnings, unclean stops) are appended to the plan's warning list for the
// caller to print.
func (e *Executor) Apply(ctx context.Context, p *Plan) (retErr error) {
	var totalBytes int64
	for _, step := range p.Steps {
		if step.Skip || step.ObjectHash == "" ||
			(step.Kind != StepLoadImage && step.Kind != StepRestoreVolume) {
			continue
		}
		size, err := e.St.ObjectSize(step.ObjectHash)
		if err != nil {
			return fmt.Errorf("size restore object %s: %w", step.ObjectHash, err)
		}
		totalBytes += size
	}
	e.meter = progress.NewMeter(e.Progress, "restore")
	e.meter.Phase("applying restore plan", "", totalBytes, progress.TotalExact, len(p.Steps))
	defer func() {
		e.meter.Finish(retErr != nil)
		e.meter = nil
	}()

	// Containers are created under their deterministic restore tag
	// (<name>-restored-from-<snap>), so the hash→image binding lives in
	// the engine itself — no in-memory ID map to keep coherent.
	containerIDs := map[string]string{} // container name → newly created ID

	applied, skipped := 0, 0
	for i, s := range p.Steps {
		e.meter.Item(s.String(), i, len(p.Steps))
		if s.Skip {
			skipped++
			fmt.Fprintf(e.Out, "  %2d. %s\n", i+1, s)
			e.meter.Item(s.String(), i+1, len(p.Steps))
			continue
		}
		if err := e.applyStep(ctx, p, s, containerIDs); err != nil {
			return fmt.Errorf("%d of %d steps completed, failed at: %s: %w",
				i, len(p.Steps), s, err)
		}
		applied++
		fmt.Fprintf(e.Out, "  %2d. ✓ %s\n", i+1, s)
		e.meter.Item(s.String(), i+1, len(p.Steps))
	}
	e.meter.Phase("finalizing restore", "", 0, progress.TotalUnknown, 0)
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
	case StepStopContainer:
		return e.Cli.StopContainer(ctx, s.ContainerID, 30)

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
		obj, err := e.openTrackedObject(s.ObjectHash)
		if err != nil {
			return fmt.Errorf("open stored volume content: %w", err)
		}
		err = e.Cli.VolumeRestore(ctx, s.Vol.Name, obj)
		obj.Close()
		return err

	case StepCreateContainer:
		imageRef := s.ImageRef
		if imageRef == "" { // legacy committed-filesystem snapshot
			imageRef = RestoreTag(p.SnapshotID, s.Name)
		}
		id, warns, err := e.Cli.ContainerCreateFromInspect(ctx, s.InspectJSON, imageRef)
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
		if s.ContainerID != "" {
			id = s.ContainerID
		}
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
	obj, err := e.openTrackedObject(hash)
	if err != nil {
		return "", fmt.Errorf("open stored image object: %w", err)
	}
	id, err := e.Cli.ImageLoadID(ctx, obj)
	obj.Close()
	return id, err
}

func (e *Executor) openTrackedObject(hash string) (io.ReadCloser, error) {
	var advance func(int64)
	if e.meter != nil {
		advance = e.meter.AddBytes
	}
	return e.St.OpenObjectTracked(hash, advance)
}
