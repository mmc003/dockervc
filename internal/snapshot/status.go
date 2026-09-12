package snapshot

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"dockervc/internal/dockerapi"
	"dockervc/internal/model"
)

// DriftSection groups one kind of difference between a snapshot and the
// live engine.
type DriftSection struct {
	Kind    string
	Added   []string
	Removed []string
	Changed []string // name: reason
}

// Empty reports whether nothing drifted in this section.
func (d DriftSection) Empty() bool {
	return len(d.Added) == 0 && len(d.Removed) == 0 && len(d.Changed) == 0
}

// Drift is the full comparison of a snapshot against the live engine.
type Drift struct {
	Containers DriftSection
	Volumes    DriftSection
	Images     DriftSection
}

// Empty reports whether the engine exactly matches the snapshot.
func (d Drift) Empty() bool {
	return d.Containers.Empty() && d.Volumes.Empty() && d.Images.Empty()
}

// minimal container facts extracted from a stored inspect JSON. Image is the
// top-level inspect field: the container's image ID (sha256:...), which
// changes when the same tag is re-pulled to a new digest.
type containerFacts struct {
	Image string `json:"Image"`
	State struct {
		Running bool `json:"Running"`
	} `json:"State"`
}

// ComputeDrift compares the given manifest against the current engine state.
func ComputeDrift(ctx context.Context, cli *dockerapi.Client, m *model.Manifest) (*Drift, error) {
	d := &Drift{Containers: DriftSection{Kind: "containers"},
		Volumes: DriftSection{Kind: "volumes"},
		Images:  DriftSection{Kind: "images"}}

	snapContainers := map[string]containerFacts{}
	for _, rec := range m.Containers {
		var f containerFacts
		if err := json.Unmarshal(rec.InspectJSON, &f); err == nil {
			snapContainers[rec.Name] = f
		}
	}
	liveContainers := map[string]containerFacts{}
	list, err := cli.ListContainers(ctx)
	if err != nil {
		return nil, err
	}
	for _, ct := range list {
		name := strings.TrimPrefix(first(ct.Names), "/")
		inspect, err := cli.InspectContainer(ctx, ct.ID)
		if err != nil {
			return nil, err
		}
		f := containerFacts{Image: inspect.Image}
		if inspect.State != nil {
			f.State.Running = inspect.State.Running
		}
		liveContainers[name] = f
	}
	for name, f := range liveContainers {
		old, ok := snapContainers[name]
		if !ok {
			d.Containers.Added = append(d.Containers.Added, name)
			continue
		}
		if old.Image != f.Image {
			d.Containers.Changed = append(d.Containers.Changed,
				fmt.Sprintf("%s (image %s → %s)", name, old.Image, f.Image))
		} else if old.State.Running != f.State.Running {
			state := "stopped"
			if f.State.Running {
				state = "running"
			}
			d.Containers.Changed = append(d.Containers.Changed, fmt.Sprintf("%s (now %s)", name, state))
		}
	}
	for name := range snapContainers {
		if _, ok := liveContainers[name]; !ok {
			d.Containers.Removed = append(d.Containers.Removed, name)
		}
	}

	snapVols := map[string]bool{}
	for _, rec := range m.Volumes {
		snapVols[rec.Name] = true
	}
	vols, err := cli.ListVolumes(ctx)
	if err != nil {
		return nil, err
	}
	liveVols := map[string]bool{}
	for _, v := range vols {
		liveVols[v.Name] = true
	}
	for name := range liveVols {
		if !snapVols[name] {
			d.Volumes.Added = append(d.Volumes.Added, name)
		}
	}
	for name := range snapVols {
		if !liveVols[name] {
			d.Volumes.Removed = append(d.Volumes.Removed, name)
		}
	}

	snapImgs := map[string]bool{}
	for _, rec := range m.Images {
		snapImgs[rec.Digest] = true
	}
	imgs, err := cli.ListImages(ctx)
	if err != nil {
		return nil, err
	}
	liveImgs := map[string]bool{}
	for _, img := range imgs {
		key := img.ID
		for _, dg := range img.RepoDigests {
			key = dg
			break
		}
		liveImgs[key] = true
	}
	for key := range liveImgs {
		if !snapImgs[key] {
			d.Images.Added = append(d.Images.Added, key)
		}
	}
	for key := range snapImgs {
		if !liveImgs[key] {
			d.Images.Removed = append(d.Images.Removed, key)
		}
	}

	sortDrift(d.Containers)
	sortDrift(d.Volumes)
	sortDrift(d.Images)
	return d, nil
}

func sortDrift(s DriftSection) {
	sort.Strings(s.Added)
	sort.Strings(s.Removed)
	sort.Strings(s.Changed)
}

// DiffManifests compares two snapshots: entries present in B but not A are
// "added", and vice versa "removed". Entries present in both count as
// "changed" when their stored content differs — containers by layer-content
// fingerprint (falling back to a churn-flagged object compare on pre-0.3.0
// snapshots), volumes with a file-level breakdown (+created, ~modified,
// -deleted) when both sides carry a file index. open may be nil, in which
// case volume changes fall back to a bare "content changed".
func DiffManifests(a, b *model.Manifest, open BlobOpener) *Drift {
	d := &Drift{
		Containers: DriftSection{Kind: "containers"},
		Volumes:    DriftSection{Kind: "volumes"},
		Images:     DriftSection{Kind: "images"},
	}

	aC, bC := map[string]model.ContainerRecord{}, map[string]model.ContainerRecord{}
	for _, r := range a.Containers {
		aC[r.Name] = r
	}
	for _, r := range b.Containers {
		bC[r.Name] = r
	}
	aV, bV := map[string]model.VolumeRecord{}, map[string]model.VolumeRecord{}
	for _, r := range a.Volumes {
		aV[r.Name] = r
	}
	for _, r := range b.Volumes {
		bV[r.Name] = r
	}

	for name, br := range bC {
		ar, ok := aC[name]
		if !ok {
			d.Containers.Added = append(d.Containers.Added, name)
			continue
		}
		switch {
		case ar.LayerHash != "" && br.LayerHash != "":
			// Layer fingerprints are stable across re-commits, so this is a
			// real content comparison, not commit-metadata churn.
			if ar.LayerHash != br.LayerHash {
				d.Containers.Changed = append(d.Containers.Changed, name+" (filesystem changed)")
			}
		case ar.ImageObject != br.ImageObject:
			// Pre-layer-hash snapshot: every commit produced a new object
			// hash, so this may be churn — say so instead of guessing.
			d.Containers.Changed = append(d.Containers.Changed,
				name+" (re-committed — comparison needs newer snapshots)")
		}
	}
	for name := range aC {
		if _, ok := bC[name]; !ok {
			d.Containers.Removed = append(d.Containers.Removed, name)
		}
	}

	for name, br := range bV {
		ar, ok := aV[name]
		if !ok {
			d.Volumes.Added = append(d.Volumes.Added, name)
			continue
		}
		if ar.Object == br.Object {
			continue // byte-identical content
		}
		reason := "content changed"
		if fc, err := DiffVolumeFiles(open, ar, br); err == nil && fc != nil {
			switch {
			case fc.Empty():
				reason = "metadata changed only (timestamps)" // tar moved, files didn't
			default:
				reason = fc.Summarize()
			}
		}
		d.Volumes.Changed = append(d.Volumes.Changed, name+" ("+reason+")")
	}
	for name := range aV {
		if _, ok := bV[name]; !ok {
			d.Volumes.Removed = append(d.Volumes.Removed, name)
		}
	}

	aI, bI := map[string]bool{}, map[string]bool{}
	for _, r := range a.Images {
		aI[r.Digest] = true
	}
	for _, r := range b.Images {
		bI[r.Digest] = true
	}
	for k := range bI {
		if !aI[k] {
			d.Images.Added = append(d.Images.Added, k)
		}
	}
	for k := range aI {
		if !bI[k] {
			d.Images.Removed = append(d.Images.Removed, k)
		}
	}

	sortDrift(d.Containers)
	sortDrift(d.Volumes)
	sortDrift(d.Images)
	return d
}
