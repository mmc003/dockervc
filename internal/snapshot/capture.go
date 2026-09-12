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
	"dockervc/internal/store"
)

// Options controls one snapshot run.
type Options struct {
	Message            string
	Stop               bool     // stop containers first (app-consistent)
	Only               []string // subset of: containers, volumes, images, networks, bindmounts
	IncludeAnonymous   bool     // include anonymous volumes (64-hex names)
	IncludeBindMounts  bool     // tar host bind-mount paths
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
	Cli *dockerapi.Client
	St  *store.Store
	Opt Options

	manifest *model.Manifest
	warnings []string
}

// Run captures the engine state and persists the snapshot. On failure the
// snapshot row is not written; any objects already stored are harmless and
// will be reclaimed by prune.
func (c *Capturer) Run(ctx context.Context) (*model.Manifest, error) {
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
	if c.Opt.scope("images") {
		if err := c.captureImages(ctx); err != nil {
			return nil, fmt.Errorf("capture images: %w", err)
		}
	}
	if c.Opt.scope("networks") {
		if err := c.captureNetworks(ctx); err != nil {
			return nil, fmt.Errorf("capture networks: %w", err)
		}
	}

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
	for _, ct := range list {
		name := strings.TrimPrefix(first(ct.Names), "/")
		inspect, err := c.Cli.InspectContainer(ctx, ct.ID)
		if err != nil {
			return fmt.Errorf("inspect %s: %w", name, err)
		}
		running := inspect.State != nil && inspect.State.Running

		raw, err := json.Marshal(inspect)
		if err != nil {
			return fmt.Errorf("marshal inspect for %s: %w", name, err)
		}

		// Commit the container filesystem (its writable layer) to an image,
		// save that image, then drop the intermediate image.
		commitRef := fmt.Sprintf("dockervc/snap/%s/%s", c.manifest.ID, sanitize(name))
		commitID, err := c.Cli.CommitContainer(ctx, ct.ID, commitRef)
		if err != nil {
			return fmt.Errorf("commit container %s: %w", name, err)
		}
		stream, err := c.Cli.SaveImage(ctx, commitID)
		if err != nil {
			c.Cli.RemoveImage(ctx, commitID)
			return fmt.Errorf("save committed image for %s: %w", name, err)
		}
		// Spill the save stream so the layer content can be hashed after the
		// blob is stored; the bytes PutBlob sees are unchanged.
		var layerHash string
		indexer, idxErr := NewContainerFSIndexer(stream)
		if idxErr != nil {
			c.warn("container %s: layer hashing unavailable (%v)", name, idxErr)
			indexer = nil
		}
		src := io.Reader(stream)
		if indexer != nil {
			src = indexer.Reader()
		}
		obj, err := c.St.PutBlob("image", "", src)
		stream.Close()
		if indexer != nil {
			if h, ferr := indexer.Finish(); ferr != nil {
				c.warn("container %s: layer hash failed (%v)", name, ferr)
			} else {
				layerHash = h
			}
		}
		rmErr := c.Cli.RemoveImage(ctx, commitID)
		if rmErr != nil {
			c.warn("intermediate image %s left behind (remove failed: %v)", short(commitID), rmErr)
		}
		if err != nil {
			return fmt.Errorf("store committed image for %s: %w", name, err)
		}

		// Every commit re-tars the filesystem (new config timestamps), so the
		// object hash differs even for identical content. Keying by layer
		// hash lets an unchanged container reuse an earlier snapshot's
		// object instead of storing a duplicate. Safe to share: the image is
		// only the filesystem source — container config lives in
		// InspectJSON, restored independently.
		reused := false
		if layerHash != "" {
			key := "containerfs:" + layerHash
			if prev, ok, kerr := c.St.ObjectByImageKey(key); kerr != nil {
				c.warn("container %s: dedup lookup failed (%v)", name, kerr)
			} else if ok && prev.Hash != obj.Hash {
				if derr := c.St.DeleteObject(obj.Hash); derr != nil {
					c.warn("container %s: duplicate object not reclaimed (%v)", name, derr)
				} else {
					obj = store.ObjectInfoResult{Hash: prev.Hash, Size: prev.Size, New: false}
					reused = true
				}
			} else if !ok {
				if kerr := c.St.SetImageKey(obj.Hash, key); kerr != nil {
					c.warn("container %s: dedup key not recorded (%v)", name, kerr)
				}
			}
		}
		c.accountObject(obj)

		c.manifest.Containers = append(c.manifest.Containers, model.ContainerRecord{
			Name:        name,
			ID:          ct.ID,
			InspectJSON: raw,
			ImageObject: obj.Hash,
			LayerHash:   layerHash,
			Running:     running,
			Size:        obj.Size,
		})
		note := ""
		if reused {
			note = "  (filesystem unchanged, reused)"
		}
		fmt.Printf("  container %-30s %s%s\n", name, HumanBytes(obj.Size), note)
	}
	return nil
}

func (c *Capturer) captureVolumes(ctx context.Context) error {
	vols, err := c.Cli.ListVolumes(ctx)
	if err != nil {
		return err
	}
	for _, v := range vols {
		if !c.Opt.IncludeAnonymous && isAnonymousVolume(v.Name) {
			continue
		}
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
		obj, err := c.St.PutBlob("volume", "", indexer.Reader())
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
		if idx != nil && len(idx.Files) > 0 {
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
	for _, p := range paths {
		stream, err := tarHostPath(p)
		if err != nil {
			c.warn("bind mount %s not captured: %v", p, err)
			continue
		}
		obj, err := c.St.PutBlob("bindmount", "", stream)
		stream.Close()
		if err != nil {
			return fmt.Errorf("store bind mount %s: %w", p, err)
		}
		c.accountObject(obj)
		c.manifest.BindMounts = append(c.manifest.BindMounts, model.BindMountRecord{
			HostPath: p, Object: obj.Hash, Size: obj.Size,
		})
		fmt.Printf("  bindmount %-30s %s\n", p, HumanBytes(obj.Size))
	}
	return nil
}

func (c *Capturer) captureImages(ctx context.Context) error {
	imgs, err := c.Cli.ListImages(ctx)
	if err != nil {
		return err
	}
	for _, img := range imgs {
		// Dedup key: repo digest if pushed/pulled, else local image ID.
		key := img.ID
		for _, d := range img.RepoDigests {
			key = d
			break
		}
		if existing, ok, err := c.St.ObjectByImageKey(key); err != nil {
			return err
		} else if ok {
			// Dedup: the exact image content is already stored; the ref is
			// wired up by InsertSnapshot from the manifest below.
			c.manifest.Images = append(c.manifest.Images, model.ImageRecord{
				Refs: img.RepoTags, Digest: key, Object: existing.Hash, Size: existing.Size,
			})
			c.manifest.Stats.ReusedObjects++
			continue
		}

		ref := img.ID
		if len(img.RepoTags) > 0 {
			ref = img.RepoTags[0]
		}
		stream, err := c.Cli.SaveImage(ctx, ref)
		if err != nil {
			c.warn("image %s not captured: %v", ref, err)
			continue
		}
		obj, err := c.St.PutBlob("image", key, stream)
		stream.Close()
		if err != nil {
			return fmt.Errorf("store image %s: %w", ref, err)
		}
		c.accountObject(obj)
		c.manifest.Images = append(c.manifest.Images, model.ImageRecord{
			Refs: img.RepoTags, Digest: key, Object: obj.Hash, Size: obj.Size,
		})
		fmt.Printf("  image     %-30s %s\n", ref, HumanBytes(obj.Size))
	}
	return nil
}

func (c *Capturer) captureNetworks(ctx context.Context) error {
	nets, err := c.Cli.ListNetworks(ctx)
	if err != nil {
		return err
	}
	for _, n := range nets {
		if isBuiltinNetwork(n.Name) {
			continue
		}
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
