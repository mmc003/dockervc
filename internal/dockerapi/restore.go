// The inverse of stream.go: thin wrappers that put state BACK into the
// engine — image load/tag, network/volume create, volume content restore,
// container remove/create. Pure JSON→SDK mappers live here too (next to the
// data they map) so the rollback planner can stay engine-free.
package dockerapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"
)

// ImageLoad streams a docker-save tar into the engine.
func (c *Client) ImageLoad(ctx context.Context, r io.Reader) error {
	_, err := c.ImageLoadID(ctx, r)
	return err
}

// ImageLoadID loads like ImageLoad and returns the ID (or ref) the daemon
// reports for the loaded image. The load response is the only daemon-native
// source for this: the save tar's config digest is NOT the image ID on
// containerd-image-store engines (Docker Desktop's default), so the tar
// cannot be trusted for the ID. Quiet stays off on purpose — quiet=1
// suppresses the very "Loaded image ID:" lines parsed here.
func (c *Client) ImageLoadID(ctx context.Context, r io.Reader) (string, error) {
	resp, err := c.c.ImageLoad(ctx, r)
	if err != nil {
		return "", fmt.Errorf("load image: %w", err)
	}
	defer resp.Body.Close()
	// The response doubles as the completion barrier: the daemon finalizes
	// the load only once the body is drained.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return "", fmt.Errorf("read load response: %w", err)
	}
	id := loadedImageRef(string(body))
	if id == "" {
		return "", fmt.Errorf("load response named no image — cannot determine what was loaded")
	}
	return id, nil
}

// ImageTag applies a repo tag to an existing image (source may be an ID or a
// ref). Used to re-apply a snapshot's recorded tags and to tag restored
// container filesystems deterministically.
func (c *Client) ImageTag(ctx context.Context, source, target string) error {
	return c.c.ImageTag(ctx, source, target)
}

// loadedImageRef extracts what the daemon loaded from a load-response body:
// "Loaded image ID: sha256:…" for tag-less saves (committed container
// filesystems), "Loaded image: <ref>" when the tar carried repo tags.
func loadedImageRef(body string) string {
	for _, line := range strings.Split(body, "\n") {
		var ev struct {
			Stream string `json:"stream"`
		}
		if json.Unmarshal([]byte(strings.TrimSpace(line)), &ev) != nil {
			continue
		}
		s := strings.TrimSpace(ev.Stream)
		if id, ok := strings.CutPrefix(s, "Loaded image ID: "); ok {
			return strings.TrimSpace(id)
		}
		if ref, ok := strings.CutPrefix(s, "Loaded image: "); ok {
			return strings.TrimSpace(ref)
		}
	}
	return ""
}

// networkCreateOptions maps a stored network.Inspect JSON onto create
// options. Everything the engine owns is dropped: Id, Created, Scope,
// Containers/Peers/Services and the ConfigFrom/ConfigOnly pair (config-only
// networks are skipped by the planner, never created). EnableIPv4/6 pointers
// are set only when true so a stored "false" doesn't fight the daemon's
// default. IPAM is copied field-by-field, per config entry.
func networkCreateOptions(raw []byte) (string, network.CreateOptions, error) {
	var n network.Inspect
	if err := json.Unmarshal(raw, &n); err != nil {
		return "", network.CreateOptions{}, fmt.Errorf("parse stored network inspect: %w", err)
	}
	opts := network.CreateOptions{
		Driver:     n.Driver,
		Internal:   n.Internal,
		Attachable: n.Attachable,
		Options:    n.Options,
		Labels:     n.Labels,
	}
	if n.EnableIPv4 {
		t := true
		opts.EnableIPv4 = &t
	}
	if n.EnableIPv6 {
		t := true
		opts.EnableIPv6 = &t
	}
	if n.IPAM.Driver != "" || len(n.IPAM.Config) > 0 || len(n.IPAM.Options) > 0 {
		ipam := network.IPAM{Driver: n.IPAM.Driver, Options: n.IPAM.Options}
		for _, cfg := range n.IPAM.Config {
			ipam.Config = append(ipam.Config, network.IPAMConfig{
				Subnet:     cfg.Subnet,
				IPRange:    cfg.IPRange,
				Gateway:    cfg.Gateway,
				AuxAddress: cfg.AuxAddress,
			})
		}
		opts.IPAM = &ipam
	}
	return n.Name, opts, nil
}

// NetworkCreateFromInspect re-creates a network from its stored inspect JSON.
// The planner only emits this for networks that don't exist, so a name
// collision here is a real error, not a reuse case.
func (c *Client) NetworkCreateFromInspect(ctx context.Context, raw json.RawMessage) error {
	name, opts, err := networkCreateOptions(raw)
	if err != nil {
		return err
	}
	if _, err := c.c.NetworkCreate(ctx, name, opts); err != nil {
		return fmt.Errorf("create network %s: %w", name, err)
	}
	return nil
}

// VolumeCreate ensures a volume exists with the recorded driver and options.
// Creating over an existing volume of the same name is an idempotent no-op
// when the parameters match.
func (c *Client) VolumeCreate(ctx context.Context, name, driver string, opts map[string]string) error {
	if _, err := c.c.VolumeCreate(ctx, volume.CreateOptions{
		Name: name, Driver: driver, DriverOpts: opts,
	}); err != nil {
		return fmt.Errorf("create volume %s: %w", name, err)
	}
	return nil
}

// VolumeRestore restores a volume's exact content — the mirror of
// VolumeTarStream. A short-lived helper mounts the volume rw at /dst, /dst is
// cleared (a rollback must be exact: files created after the snapshot don't
// survive), then the stored tar streams in via CopyToContainer. The tar's
// entries are rooted at the volume root ("./"), matching /dst.
func (c *Client) VolumeRestore(ctx context.Context, volumeName string, tarStream io.Reader) error {
	helper, err := c.pickHelperImage(ctx)
	if err != nil {
		return err
	}

	suffix := make([]byte, 3)
	rand.Read(suffix)
	name := "dockervc-restore-" + hex.EncodeToString(suffix)

	hc, err := c.c.ContainerCreate(ctx,
		&container.Config{
			Image:           helper,
			Cmd:             []string{"sleep", "300"},
			NetworkDisabled: true,
			Labels:          map[string]string{"dockervc": "helper"},
		},
		&container.HostConfig{
			Binds: []string{volumeName + ":/dst"}, // rw, unlike capture's :ro
		},
		nil, nil, name)
	if err != nil {
		return fmt.Errorf("create restore helper for volume %s: %w", volumeName, err)
	}
	// Best-effort cleanup like volumeTarReader.cleanup; Force covers the
	// still-running case.
	defer c.c.ContainerRemove(context.Background(), hc.ID, container.RemoveOptions{Force: true})

	if err := c.c.ContainerStart(ctx, hc.ID, container.StartOptions{}); err != nil {
		return fmt.Errorf("start restore helper: %w", err)
	}
	if err := c.clearDir(ctx, hc.ID, "/dst"); err != nil {
		return err
	}
	if err := c.c.CopyToContainer(ctx, hc.ID, "/dst", tarStream, container.CopyToContainerOptions{}); err != nil {
		return fmt.Errorf("copy content into volume %s: %w", volumeName, err)
	}
	return nil
}

// clearDir empties a directory inside a running helper container. find
// -mindepth 1 -delete (not rm -rf) so the mount point itself is never a
// candidate; busybox/alpine find both support it. Exec runs detached and is
// polled to completion — its exit code decides success.
func (c *Client) clearDir(ctx context.Context, containerID, dir string) error {
	exec, err := c.c.ContainerExecCreate(ctx, containerID, container.ExecOptions{
		Cmd: []string{"find", dir, "-mindepth", "1", "-delete"},
	})
	if err != nil {
		return fmt.Errorf("create clear exec: %w", err)
	}
	if err := c.c.ContainerExecStart(ctx, exec.ID, container.ExecStartOptions{Detach: true}); err != nil {
		return fmt.Errorf("start clear exec: %w", err)
	}
	for {
		insp, err := c.c.ContainerExecInspect(ctx, exec.ID)
		if err != nil {
			return fmt.Errorf("inspect clear exec: %w", err)
		}
		if !insp.Running {
			if insp.ExitCode != 0 {
				return fmt.Errorf("clear %s failed with exit code %d", dir, insp.ExitCode)
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// ContainerRemove removes a container; force kills a running one first.
func (c *Client) ContainerRemove(ctx context.Context, id string, force bool) error {
	return c.c.ContainerRemove(ctx, id, container.RemoveOptions{Force: force})
}

// containerCreateConfig derives a create triple from a stored container
// inspect. Recorded operational state is dropped — Hostname (the old
// container's ID), Config.Image (superseded by the restored image), endpoint
// IDs/IPs/MACs (engine reassigns; static IPAMConfig is deliberately not
// restored). The embedded Resources struct is copied in one assignment so
// every memory/CPU/device limit rides along without enumerating fields.
func containerCreateConfig(raw []byte, imageName string) (*container.Config, *container.HostConfig, *network.NetworkingConfig, string, error) {
	var cj container.InspectResponse
	if err := json.Unmarshal(raw, &cj); err != nil {
		return nil, nil, nil, "", fmt.Errorf("parse stored container inspect: %w", err)
	}
	if cj.Config == nil || cj.HostConfig == nil {
		return nil, nil, nil, "", fmt.Errorf("stored inspect lacks Config/HostConfig")
	}
	old := cj.Config

	cfg := &container.Config{
		Domainname:      old.Domainname,
		User:            old.User,
		ExposedPorts:    old.ExposedPorts, // keeps `docker ps` port display honest
		Tty:             old.Tty,
		OpenStdin:       old.OpenStdin,
		Env:             old.Env,
		Cmd:             old.Cmd,
		Healthcheck:     old.Healthcheck,
		WorkingDir:      old.WorkingDir,
		Entrypoint:      old.Entrypoint,
		NetworkDisabled: old.NetworkDisabled,
		Labels:          old.Labels,
		StopSignal:      old.StopSignal,
		StopTimeout:     old.StopTimeout,
		Image:           imageName,
	}

	oh := cj.HostConfig
	hc := &container.HostConfig{
		Binds:         oh.Binds,
		Mounts:        oh.Mounts, // --mount-created containers carry them here, not in Binds
		LogConfig:     oh.LogConfig,
		NetworkMode:   oh.NetworkMode, // the container's primary network name
		PortBindings:  oh.PortBindings,
		RestartPolicy: oh.RestartPolicy,
		AutoRemove:    oh.AutoRemove,
		CapAdd:        oh.CapAdd,
		CapDrop:       oh.CapDrop,
		DNS:           oh.DNS,
		DNSOptions:    oh.DNSOptions,
		DNSSearch:     oh.DNSSearch,
		ExtraHosts:    oh.ExtraHosts,
		IpcMode:       oh.IpcMode,
		Links:         oh.Links,
		PidMode:       oh.PidMode,
		Privileged:    oh.Privileged,
		ReadonlyRootfs: oh.ReadonlyRootfs,
		SecurityOpt:   oh.SecurityOpt,
		Sysctls:       oh.Sysctls,
		Tmpfs:         oh.Tmpfs,
		ShmSize:       oh.ShmSize,
	}
	hc.Resources = oh.Resources // wholesale: Memory, NanoCPUs, Cpuset*, Ulimits, Devices, …

	nc := &network.NetworkingConfig{EndpointsConfig: map[string]*network.EndpointSettings{}}
	if cj.NetworkSettings != nil {
		for name, ep := range cj.NetworkSettings.Networks {
			if ep == nil {
				continue
			}
			nc.EndpointsConfig[name] = &network.EndpointSettings{
				Aliases:    ep.Aliases,
				Links:      ep.Links,
				DriverOpts: ep.DriverOpts,
				// IPAMConfig nil: per-container static IPs are dropped, the
				// engine reassigns. Operational fields (NetworkID, IPAddress,
				// MacAddress, DNSNames, …) are simply never copied.
			}
		}
	}

	return cfg, hc, nc, strings.TrimPrefix(cj.Name, "/"), nil
}

// ContainerCreateFromInspect re-creates a container from its stored inspect
// JSON, running under imageName (the restored committed filesystem). Create
// warnings are returned, not printed — the caller surfaces them as plan
// warnings.
func (c *Client) ContainerCreateFromInspect(ctx context.Context, inspectJSON json.RawMessage, imageName string) (id string, warnings []string, err error) {
	cfg, hc, nc, name, err := containerCreateConfig(inspectJSON, imageName)
	if err != nil {
		return "", nil, err
	}
	resp, err := c.c.ContainerCreate(ctx, cfg, hc, nc, nil /* same engine */, name)
	if err != nil {
		return "", nil, fmt.Errorf("create container %s: %w", name, err)
	}
	return resp.ID, resp.Warnings, nil
}
