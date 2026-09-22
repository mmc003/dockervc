// Package snapshot implements capture of Docker engine state into a
// snapshot manifest plus content-addressed objects.
package snapshot

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/docker/docker/api/types/mount"

	"dockervc/internal/dockerapi"
	"dockervc/internal/model"
	"dockervc/internal/progress"
	"dockervc/internal/store"
)

// Options controls one snapshot run.
type Options struct {
	Message           string
	Stop              bool     // stop containers first (app-consistent)
	Only              []string // subset of: containers, volumes, images, networks, bindmounts
	IncludeAnonymous  bool     // include anonymous volumes (64-hex names)
	IncludeBindMounts bool     // tar host bind-mount paths
}

// scope returns true if the given entity kind should be captured.
func (o Options) scope(kind string) bool {
	if len(o.Only) == 0 {
		return true
	}
	for _, k := range o.Only {
		if k == kind {
			return true
		}
	}
	return false
}

// Capturer runs a snapshot.
type Capturer struct {
	Cli      *dockerapi.Client
	St       *store.Store
	Opt      Options
	Progress progress.Reporter

	manifest       *model.Manifest
	warnings       []string
	meter          *progress.Meter
	baseline       *model.Manifest
	requiredImages map[string]bool
}

// Run captures the engine state and persists the snapshot. On failure the
// snapshot row is not written; any objects already stored are harmless and
// will be reclaimed by prune.
func (c *Capturer) Run(ctx context.Context) (result *model.Manifest, retErr error) {
	c.meter = progress.NewMeter(c.Progress, "snapshot")
	defer func() {
		c.meter.Finish(retErr != nil)
		c.meter = nil
	}()
	baseline, err := c.St.LatestSnapshot()
	if err != nil {
		return nil, fmt.Errorf("load snapshot size baseline: %w", err)
	}
	c.baseline = baseline

	engineID, dockerVersion, err := c.Cli.EngineInfo(ctx)
	if err != nil {
		return nil, fmt.Errorf("connect to docker engine: %w", err)
	}

	id, err := newSnapshotID()
	if err != nil {
		return nil, err
	}
	m := &model.Manifest{
		ID:            id,
		CreatedAt:     time.Now(),
		Message:       c.Opt.Message,
		DockerVersion: dockerVersion,
		EngineID:      engineID,
		Consistent:    c.Opt.Stop,
	}
	c.manifest = m

	// --stop: application-consistent capture. Record running state, stop,
	// and restart whatever we stopped when done (even on failure).
	var toRestart []string
	if c.Opt.Stop {
		toRestart, err = c.stopRunningContainers(ctx)
		if err != nil {
			return nil, err
		}
		defer func() {
			for _, id := range toRestart {
				if err := c.Cli.StartContainer(ctx, id); err != nil {
					log.Printf("warning: could not restart container %s: %v", id, err)
				}
			}
		}()
	}

	if c.Opt.scope("containers") {
		if err := c.captureContainers(ctx); err != nil {
			return nil, fmt.Errorf("capture containers: %w", err)
		}
	}
	if c.Opt.scope("volumes") {
		if err := c.captureVolumes(ctx); err != nil {
			return nil, fmt.Errorf("capture volumes: %w", err)
		}
	}
	if c.Opt.scope("bindmounts") && c.Opt.IncludeBindMounts {
		if err := c.captureBindMounts(ctx); err != nil {
			return nil, fmt.Errorf("capture bind mounts: %w", err)
		}
	}
	if c.Opt.scope("images") || len(c.requiredImages) > 0 {
		if err := c.captureImages(ctx, c.Opt.scope("images")); err != nil {
			return nil, fmt.Errorf("capture images: %w", err)
		}
	}
	if c.Opt.scope("networks") {
		if err := c.captureNetworks(ctx); err != nil {
			return nil, fmt.Errorf("capture networks: %w", err)
		}
	}
	c.meter.Phase("finalizing snapshot", "", 0, progress.TotalUnknown, 0)

	for _, rec := range c.manifest.Images {
		m.TotalSize += rec.Size
	}
	for _, rec := range c.manifest.Containers {
		m.TotalSize += rec.Size
	}
	for _, rec := range c.manifest.Volumes {
		m.TotalSize += rec.Size
	}
	for _, rec := range m.BindMounts {
		m.TotalSize += rec.Size
	}

	if err := c.St.InsertSnapshot(m); err != nil {
		return nil, fmt.Errorf("persist snapshot: %w", err)
	}
	return m, nil
}

// Warnings returns non-fatal issues encountered during capture.
func (c *Capturer) Warnings() []string { return c.warnings }

func (c *Capturer) warn(format string, args ...any) {
	c.warnings = append(c.warnings, fmt.Sprintf(format, args...))
}

func (c *Capturer) stopRunningContainers(ctx context.Context) ([]string, error) {
	list, err := c.Cli.ListContainers(ctx)
	if err != nil {
		return nil, err
	}
	var stopped []string
	for _, ct := range list {
		if !strings.Contains(ct.State, "running") {
			continue
		}
		if err := c.Cli.StopContainer(ctx, ct.ID, 30); err != nil {
			return stopped, fmt.Errorf("stop container %s: %w", first(ct.Names), err)
		}
		stopped = append(stopped, ct.ID)
	}
	return stopped, nil
}

func (c *Capturer) captureContainers(ctx context.Context) error {
	list, err := c.Cli.ListContainers(ctx)
	if err != nil {
		return err
	}
	for i, ct := range list {
		name := strings.TrimPrefix(first(ct.Names), "/")
		c.progressEntity("snapshotting container", name, c.estimateSize("container", name), i, len(list))
		inspect, err := c.Cli.InspectContainer(ctx, ct.ID)
		if err != nil {
			return fmt.Errorf("inspect %s: %w", name, err)
		}
		running := inspect.State != nil && inspect.State.Running

		raw, err := json.Marshal(inspect)
		if err != nil {
			return fmt.Errorf("marshal inspect for %s: %w", name, err)
		}
		configHash, err := dockerapi.ContainerConfigHash(raw)
		if err != nil {
			return fmt.Errorf("normalize container %s configuration: %w", name, err)
		}
		if inspect.Image == "" {
			return fmt.Errorf("container %s has no immutable image ID", name)
		}
		imageRef := ""
		if inspect.Config != nil {
			imageRef = inspect.Config.Image
		}
		if c.requiredImages == nil {
			c.requiredImages = map[string]bool{}
		}
		c.requiredImages[inspect.Image] = true

		c.manifest.Containers = append(c.manifest.Containers, model.ContainerRecord{
			Name:        name,
			ID:          ct.ID,
			InspectJSON: raw,
			ImageRef:    imageRef,
			ImageID:     inspect.Image,
			ImageKey:    inspect.Image,
			ConfigHash:  configHash,
			Running:     running,
		})
		fmt.Printf("  container %-30s recipe (image %s)\n", name, short(inspect.Image))
		c.finishProgressEntity(name, i+1, len(list), 0)
	}
	return nil
}

func (c *Capturer) captureVolumes(ctx context.Context) error {
	vols, err := c.Cli.ListVolumes(ctx)
	if err != nil {
		return err
	}
	selected := vols[:0]
	for _, v := range vols {
		if c.Opt.IncludeAnonymous || !isAnonymousVolume(v.Name) {
			selected = append(selected, v)
		}
	}
	for i, v := range selected {
		c.progressEntity("snapshotting volume", v.Name, c.estimateSize("volume", v.Name), i, len(selected))
		if v.Driver != "local" {
			c.warn("volume %s uses driver %q; captured via helper container, verify on restore", v.Name, v.Driver)
		}
		stream, err := c.Cli.VolumeTarStream(ctx, v.Name)
		if err != nil {
			return fmt.Errorf("archive volume %s: %w", v.Name, err)
		}
		// Tee the tar into a background parser: the bytes PutBlob consumes —
		// and therefore the CAS hash — are exactly what docker produced, and
		// the file index falls out of the same stream at no extra I/O.
		indexer := NewVolumeIndexer(stream)
		obj, err := c.putBlobTracked("volume", "", indexer.Reader())
		stream.Close()
		idx, idxErr := indexer.Finish() // reap the parser goroutine either way
		if err != nil {
			return fmt.Errorf("store volume %s: %w", v.Name, err)
		}
		if idxErr != nil {
			c.warn("volume %s stored but not file-indexed (tar parse failed: %v)", v.Name, idxErr)
			idx = nil
		}
		c.accountObject(obj)

		rec := model.VolumeRecord{
			Name:    v.Name,
			Driver:  v.Driver,
			Options: v.Options,
			Object:  obj.Hash,
			Size:    obj.Size,
		}
		// Persist even an empty index. An empty volume is a known state that
		// reconciliation can compare exactly; omitting its index would make it
		// indistinguishable from a legacy snapshot that predates indexing.
		if idx != nil {
			iobj, err := c.St.PutBlob("volindex", "", idx.Reader())
			if err != nil {
				return fmt.Errorf("store file index for %s: %w", v.Name, err)
			}
			c.accountObject(iobj)
			rec.IndexObject = iobj.Hash
			rec.Files = len(idx.Files)
		}
		c.manifest.Volumes = append(c.manifest.Volumes, rec)
		files := ""
		if rec.Files > 0 {
			files = fmt.Sprintf("  %d files", rec.Files)
		}
		fmt.Printf("  volume    %-30s %s%s\n", v.Name, HumanBytes(obj.Size), files)
		c.finishProgressEntity(v.Name, i+1, len(selected), obj.Size)
	}
	return nil
}

func (c *Capturer) captureBindMounts(ctx context.Context) error {
	list, err := c.Cli.ListContainers(ctx)
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, ct := range list {
		inspect, err := c.Cli.InspectContainer(ctx, ct.ID)
		if err != nil {
			return err
		}
		for _, mnt := range inspect.Mounts {
			if mnt.Type != mount.TypeBind || seen[mnt.Source] {
				continue
			}
			seen[mnt.Source] = true
		}
	}
	paths := make([]string, 0, len(seen))
	for p := range seen {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for i, p := range paths {
		c.progressEntity("snapshotting bind mount", p, c.estimateSize("bindmount", p), i, len(paths))
		stream, err := tarHostPath(p)
		if err != nil {
			c.warn("bind mount %s not captured: %v", p, err)
			continue
		}
		obj, err := c.putBlobTracked("bindmount", "", stream)
		stream.Close()
		if err != nil {
			return fmt.Errorf("store bind mount %s: %w", p, err)
		}
		c.accountObject(obj)
		c.manifest.BindMounts = append(c.manifest.BindMounts, model.BindMountRecord{
			HostPath: p, Object: obj.Hash, Size: obj.Size,
		})
		fmt.Printf("  bindmount %-30s %s\n", p, HumanBytes(obj.Size))
		c.finishProgressEntity(p, i+1, len(paths), obj.Size)
	}
	return nil
}

func (c *Capturer) captureImages(ctx context.Context, all bool) error {
	imgs, err := c.Cli.ListImages(ctx)
	if err != nil {
		return err
	}
	selected := imgs[:0]
	for _, img := range imgs {
		if all || c.requiredImages[img.ID] {
			selected = append(selected, img)
		}
	}
	for id := range c.requiredImages {
		found := false
		for _, img := range selected {
			if img.ID == id {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("required container image %s is not present", id)
		}
	}
	for i, img := range selected {
		// Dedup key: repo digest if pushed/pulled, else local image ID.
		key := img.ID
		for _, d := range img.RepoDigests {
			key = d
			break
		}
		storageKey := "image-neutral-v1:" + key
		c.progressEntity("snapshotting image", key, c.estimateSize("image", key), i, len(selected))
		if existing, ok, err := c.St.ObjectByImageKey(storageKey); err != nil {
			return err
		} else if ok {
			// Dedup: the exact image content is already stored; the ref is
			// wired up by InsertSnapshot from the manifest below.
			c.manifest.Images = append(c.manifest.Images, model.ImageRecord{
				ID: img.ID, Refs: cleanImageRefs(img.RepoTags), Digests: img.RepoDigests,
				Key: img.ID, Digest: key, Object: existing.Hash, Size: existing.Size,
			})
			c.manifest.Stats.ReusedObjects++
			c.finishProgressEntity(key+" (reused)", i+1, len(selected), 0)
			continue
		}

		// Use the immutable ID rather than a mutable public tag. New container
		// recipes preserve RepoTags only as provenance.
		ref := img.ID
		stream, err := c.Cli.SaveImage(ctx, ref)
		if err != nil {
			if c.requiredImages[img.ID] {
				return fmt.Errorf("save required container image %s: %w", ref, err)
			}
			c.warn("image %s not captured: %v", ref, err)
			continue
		}
		neutral := dockerapi.TagNeutralImageArchive(stream)
		obj, err := c.putBlobTracked("image", storageKey, neutral)
		neutral.Close()
		if err != nil {
			return fmt.Errorf("store image %s: %w", ref, err)
		}
		c.accountObject(obj)
		c.manifest.Images = append(c.manifest.Images, model.ImageRecord{
			ID: img.ID, Refs: cleanImageRefs(img.RepoTags), Digests: img.RepoDigests,
			Key: img.ID, Digest: key, Object: obj.Hash, Size: obj.Size,
		})
		fmt.Printf("  image     %-30s %s\n", ref, HumanBytes(obj.Size))
		c.finishProgressEntity(key, i+1, len(selected), obj.Size)
	}
	return nil
}

func cleanImageRefs(refs []string) []string {
	out := make([]string, 0, len(refs))
	for _, ref := range refs {
		if ref != "" && ref != "<none>:<none>" {
			out = append(out, ref)
		}
	}
	return out
}

func (c *Capturer) captureNetworks(ctx context.Context) error {
	nets, err := c.Cli.ListNetworks(ctx)
	if err != nil {
		return err
	}
	selected := nets[:0]
	for _, n := range nets {
		if !isBuiltinNetwork(n.Name) {
			selected = append(selected, n)
		}
	}
	for i, n := range selected {
		c.progressEntity("capturing network", n.Name, 0, i, len(selected))
		inspect, err := c.Cli.InspectNetwork(ctx, n.Name)
		if err != nil {
			return fmt.Errorf("inspect network %s: %w", n.Name, err)
		}
		raw, err := json.Marshal(inspect)
		if err != nil {
			return err
		}
		c.manifest.Networks = append(c.manifest.Networks, model.NetworkRecord{
			Name: n.Name, InspectJSON: raw,
		})
		fmt.Printf("  network   %-30s (config)\n", n.Name)
		c.finishProgressEntity(n.Name, i+1, len(selected), 0)
	}
	return nil
}

// accountObject updates dedup stats from a PutBlob result.
func (c *Capturer) accountObject(obj store.ObjectInfoResult) {
	if obj.New {
		c.manifest.Stats.NewObjects++
		c.manifest.Stats.NewBytes += obj.Size
	} else {
		c.manifest.Stats.ReusedObjects++
	}
}

func (c *Capturer) putBlobTracked(kind, imageKey string, r io.Reader) (store.ObjectInfoResult, error) {
	var storedAdvance func(int64)
	if c.meter != nil {
		storedAdvance = c.meter.AddBytes
	}
	return c.St.PutBlobTracked(kind, imageKey, r, nil, storedAdvance)
}

func (c *Capturer) progressEntity(phase, name string, estimate int64, done, total int) {
	if c.meter == nil {
		return
	}
	kind := progress.TotalUnknown
	if estimate > 0 {
		kind = progress.TotalEstimated
	}
	c.meter.Phase(phase, name, estimate, kind, total)
	c.meter.Item(name, done, total)
}

func (c *Capturer) finishProgressEntity(name string, done, total int, actual int64) {
	if c.meter == nil {
		return
	}
	if actual > 0 {
		c.meter.SetBytes(actual, actual, progress.TotalExact)
	}
	c.meter.Item(name, done, total)
}

func (c *Capturer) estimateSize(kind, name string) int64 {
	if c.baseline == nil {
		return 0
	}
	switch kind {
	case "container":
		for _, rec := range c.baseline.Containers {
			if rec.Name == name {
				return rec.Size
			}
		}
	case "volume":
		for _, rec := range c.baseline.Volumes {
			if rec.Name == name {
				return rec.Size
			}
		}
	case "image":
		for _, rec := range c.baseline.Images {
			if rec.Digest == name {
				return rec.Size
			}
		}
	case "bindmount":
		for _, rec := range c.baseline.BindMounts {
			if rec.HostPath == name {
				return rec.Size
			}
		}
	}
	return 0
}

// newSnapshotID yields sortable, human-friendly IDs:
// snap-YYYYMMDD-HHMMSS-xxxx (4 random hex chars).
func newSnapshotID() (string, error) {
	b := make([]byte, 2)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return fmt.Sprintf("snap-%s-%s",
		time.Now().Format("20060102-150405"), hex.EncodeToString(b)), nil
}

func isBuiltinNetwork(name string) bool {
	switch name {
	case "bridge", "host", "none", "docker":
		return true
	}
	return false
}

func isAnonymousVolume(name string) bool {
	if len(name) != 64 {
		return false
	}
	_, err := hex.DecodeString(name)
	return err == nil
}

func sanitize(name string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			return r
		default:
			return '-'
		}
	}, strings.ToLower(name))
}

func first(names []string) string {
	if len(names) == 0 {
		return ""
	}
	return names[0]
}

func short(id string) string {
	return strings.TrimPrefix(filepath.Base(id), "sha256:")[:12]
}
