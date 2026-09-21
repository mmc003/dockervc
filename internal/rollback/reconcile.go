package rollback

import (
	"context"
	"fmt"
	"sort"

	"dockervc/internal/model"
	"dockervc/internal/progress"
	"dockervc/internal/snapshot"
)

// ReconcileVolumes deep-compares only the volumes required by the selected
// rollback scope. Results are stored on live for BuildPlan: unchanged volumes
// can be kept, changed volumes are restored after confirmation, missing ones
// are created, and old snapshots or scan failures are explicitly marked
// unverifiable rather than guessed equal.
func ReconcileVolumes(ctx context.Context, openTar snapshot.VolumeTarOpener, openBlob snapshot.BlobOpener,
	m *model.Manifest, scope Scope, live *LiveState, reporter progress.Reporter) error {
	needed := requiredVolumeNames(m, scope)
	for _, rec := range m.Volumes {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !needed[rec.Name] {
			continue
		}
		if !live.VolumeNames[rec.Name] {
			live.VolumeStates[rec.Name] = EntityMissing
			live.VolumeDiffs[rec.Name] = "not present"
			continue
		}
		if rec.IndexObject == "" {
			live.VolumeStates[rec.Name] = EntityUnverifiable
			live.VolumeDiffs[rec.Name] = "snapshot has no file index"
			continue
		}
		change, _, _, err := snapshot.DiffLiveVolumeTracked(
			ctx, openTar, openBlob, rec, reporter)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			live.VolumeStates[rec.Name] = EntityUnverifiable
			live.VolumeDiffs[rec.Name] = fmt.Sprintf("deep scan failed: %v", err)
			continue
		}
		if change == nil {
			live.VolumeStates[rec.Name] = EntityUnverifiable
			live.VolumeDiffs[rec.Name] = "snapshot has no comparable file index"
			continue
		}
		if change.Empty() {
			live.VolumeStates[rec.Name] = EntityUnchanged
			live.VolumeDiffs[rec.Name] = "file contents match"
			continue
		}
		live.VolumeStates[rec.Name] = EntityChanged
		live.VolumeDiffs[rec.Name] = change.Summarize()
	}
	return nil
}

func requiredVolumeNames(m *model.Manifest, scope Scope) map[string]bool {
	needed := map[string]bool{}
	if scope.All {
		for i := range m.Volumes {
			needed[m.Volumes[i].Name] = true
		}
	} else {
		for _, name := range scope.Volumes {
			needed[name] = true
		}
	}
	for _, rec := range selectContainers(m, scope) {
		deps, err := containerDeps(rec.InspectJSON)
		if err != nil {
			continue
		}
		for _, name := range deps.VolumeNames {
			if hasVolume(m, name) {
				needed[name] = true
			}
		}
	}
	return needed
}

func sortedVolumeUsers(users []ContainerUse) []ContainerUse {
	out := append([]ContainerUse(nil), users...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name == out[j].Name {
			return out[i].ID < out[j].ID
		}
		return out[i].Name < out[j].Name
	})
	return out
}
