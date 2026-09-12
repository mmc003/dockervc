// Package dockerapi wraps the official Docker Engine API client with the
// small surface dockervc needs: inventory listing, inspection, and the
// streaming operations (image save, container commit, volume tar).
package dockerapi

import (
	"context"
	"fmt"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"
)

// Client is a thin wrapper over the official API client.
type Client struct {
	c *client.Client
}

// New connects to the local engine (DOCKER_HOST honored) and negotiates the
// API version.
func New() (*Client, error) {
	c, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("docker client: %w", err)
	}
	return &Client{c: c}, nil
}

// Ping verifies engine connectivity.
func (c *Client) Ping(ctx context.Context) error {
	_, err := c.c.Ping(ctx)
	return err
}

// EngineInfo returns the engine ID and version string.
func (c *Client) EngineInfo(ctx context.Context) (id, version string, err error) {
	info, err := c.c.Info(ctx)
	if err != nil {
		return "", "", err
	}
	return info.ID, info.ServerVersion, nil
}

// ListContainers returns every container (running and stopped).
func (c *Client) ListContainers(ctx context.Context) ([]container.Summary, error) {
	return c.c.ContainerList(ctx, container.ListOptions{All: true})
}

// InspectContainer returns the full inspect JSON for one container.
func (c *Client) InspectContainer(ctx context.Context, id string) (types.ContainerJSON, error) {
	return c.c.ContainerInspect(ctx, id)
}

// ListVolumes returns every volume known to the engine.
func (c *Client) ListVolumes(ctx context.Context) ([]*volume.Volume, error) {
	resp, err := c.c.VolumeList(ctx, volume.ListOptions{})
	if err != nil {
		return nil, err
	}
	return resp.Volumes, nil
}

// ListNetworks returns every network.
func (c *Client) ListNetworks(ctx context.Context) ([]network.Summary, error) {
	return c.c.NetworkList(ctx, network.ListOptions{})
}

// InspectNetwork returns the full inspect JSON for one network.
func (c *Client) InspectNetwork(ctx context.Context, id string) (network.Inspect, error) {
	return c.c.NetworkInspect(ctx, id, network.InspectOptions{})
}

// ListImages returns every image on the host.
func (c *Client) ListImages(ctx context.Context) ([]image.Summary, error) {
	return c.c.ImageList(ctx, image.ListOptions{All: true})
}

// StopContainer stops a container gracefully.
func (c *Client) StopContainer(ctx context.Context, id string, timeoutSecs int) error {
	t := timeoutSecs
	return c.c.ContainerStop(ctx, id, container.StopOptions{Timeout: &t})
}

// StartContainer starts an existing container.
func (c *Client) StartContainer(ctx context.Context, id string) error {
	return c.c.ContainerStart(ctx, id, container.StartOptions{})
}

// IsRunning reports whether the container is currently running.
func (c *Client) IsRunning(ctx context.Context, id string) (bool, error) {
	json, err := c.c.ContainerInspect(ctx, id)
	if err != nil {
		return false, err
	}
	return json.State != nil && json.State.Running, nil
}
