package dockerapi

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/pkg/stdcopy"
)

// SaveImage returns a raw tar stream of `docker save <ref>`. The caller must
// Close the stream.
func (c *Client) SaveImage(ctx context.Context, ref string) (io.ReadCloser, error) {
	return c.c.ImageSave(ctx, []string{ref})
}

// CommitContainer commits the container's current filesystem (including its
// writable layer) as a new image tagged ref, returning the new image ID.
// This is the API behind `docker commit`.
func (c *Client) CommitContainer(ctx context.Context, containerID, ref string) (string, error) {
	resp, err := c.c.ContainerCommit(ctx, containerID, container.CommitOptions{Reference: ref})
	if err != nil {
		return "", err
	}
	return resp.ID, nil
}

// RemoveImage force-removes an image (used to clean up commit intermediates).
func (c *Client) RemoveImage(ctx context.Context, ref string) error {
	_, err := c.c.ImageRemove(ctx, ref, image.RemoveOptions{Force: true})
	return err
}

// helperCandidates are looked up locally, in order, before anything is pulled.
var helperCandidates = []string{"alpine:3", "alpine:latest", "busybox:latest", "busybox:stable", "debian:bookworm-slim"}

// pickHelperImage returns a locally present image containing tar.
func (c *Client) pickHelperImage(ctx context.Context) (string, error) {
	imgs, err := c.ListImages(ctx)
	if err != nil {
		return "", err
	}
	present := map[string]bool{}
	for _, img := range imgs {
		for _, tag := range img.RepoTags {
			present[tag] = true
		}
	}
	for _, cand := range helperCandidates {
		if present[cand] {
			return cand, nil
		}
	}
	// Fall back to any local image likely to ship tar.
	for _, img := range imgs {
		for _, tag := range img.RepoTags {
			l := strings.ToLower(tag)
			if strings.Contains(l, "alpine") || strings.Contains(l, "busybox") ||
				strings.Contains(l, "debian") || strings.Contains(l, "ubuntu") {
				return tag, nil
			}
		}
	}
	// Last resort: pull alpine (the one network touch dockervc ever makes).
	fmt.Fprintln(os.Stderr, "dockervc: no local helper image; pulling alpine:3 for volume archiving")
	pr, err := c.c.ImagePull(ctx, "alpine:3", image.PullOptions{})
	if err != nil {
		return "", fmt.Errorf("no helper image with tar available and alpine:3 pull failed: %w", err)
	}
	defer pr.Close()
	if _, err := io.Copy(io.Discard, pr); err != nil {
		return "", err
	}
	return "alpine:3", nil
}

// VolumeTarStream archives a volume's contents by running a short-lived,
// network-disabled helper container that mounts the volume read-only and
// writes a tar to stdout. The returned reader yields the tar; closing it
// removes the helper container.
func (c *Client) VolumeTarStream(ctx context.Context, volumeName string) (io.ReadCloser, error) {
	helper, err := c.pickHelperImage(ctx)
	if err != nil {
		return nil, err
	}

	suffix := make([]byte, 3)
	rand.Read(suffix)
	name := "dockervc-tar-" + hex.EncodeToString(suffix)

	hc, err := c.c.ContainerCreate(ctx,
		&container.Config{
			Image:           helper,
			Cmd:             []string{"tar", "cf", "-", "-C", "/src", "."},
			NetworkDisabled: true,
			Labels:          map[string]string{"dockervc": "helper"},
		},
		&container.HostConfig{
			Binds:      []string{volumeName + ":/src:ro"},
			AutoRemove: false,
		},
		nil, nil, name)
	if err != nil {
		return nil, fmt.Errorf("create tar helper for volume %s: %w", volumeName, err)
	}

	attach, err := c.c.ContainerAttach(ctx, hc.ID, container.AttachOptions{
		Stdout: true, Stderr: true, Stream: true,
	})
	if err != nil {
		c.c.ContainerRemove(ctx, hc.ID, container.RemoveOptions{Force: true})
		return nil, fmt.Errorf("attach tar helper: %w", err)
	}

	if err := c.c.ContainerStart(ctx, hc.ID, container.StartOptions{}); err != nil {
		attach.Close()
		c.c.ContainerRemove(ctx, hc.ID, container.RemoveOptions{Force: true})
		return nil, fmt.Errorf("start tar helper: %w", err)
	}
	waitOK, waitErr := c.c.ContainerWait(context.Background(), hc.ID, container.WaitConditionNotRunning)

	pr, pw := io.Pipe()
	var errBuf bytes.Buffer
	go func() {
		// Demultiplex the attach stream: stdout is the tar, stderr is diagnostics.
		_, copyErr := stdcopy.StdCopy(pw, &errBuf, attach.Reader)
		attach.Close()
		if copyErr != nil {
			pw.CloseWithError(fmt.Errorf("tar helper stream: %w (helper stderr: %s)", copyErr, strings.TrimSpace(errBuf.String())))
			return
		}
		select {
		case err := <-waitErr:
			pw.CloseWithError(fmt.Errorf("wait for tar helper: %w", err))
		case status := <-waitOK:
			if status.StatusCode != 0 {
				detail := strings.TrimSpace(errBuf.String())
				if detail == "" && status.Error != nil {
					detail = status.Error.Message
				}
				if detail == "" {
					detail = "no diagnostics"
				}
				pw.CloseWithError(fmt.Errorf("tar helper exited with status %d: %s", status.StatusCode, detail))
				return
			}
			pw.CloseWithError(nil)
		}
	}()

	return &volumeTarReader{
		Reader: &tarValidatingReader{r: pr},
		cleanup: func() {
			// Force handles the still-running case; ignores errors — best effort.
			c.c.ContainerRemove(context.Background(), hc.ID, container.RemoveOptions{Force: true})
		},
	}, nil
}

// tarValidatingReader fails fast (before the CAS commits anything) if the
// stream does not look like a tar archive — e.g. the helper's tar errored.
type tarValidatingReader struct {
	r       io.Reader
	checked bool
}

func (t *tarValidatingReader) Read(p []byte) (int, error) {
	n, err := t.r.Read(p)
	// ustar magic sits at offset 257. Only validate when the first read
	// actually covers it; short streams are caught at restore time.
	if !t.checked && n >= 262 {
		t.checked = true
		if !bytes.Equal(p[257:262], []byte("ustar")) {
			return n, fmt.Errorf("volume stream is not a tar archive")
		}
	}
	return n, err
}

// volumeTarReader removes the helper container once the tar is drained or
// the caller closes the stream.
type volumeTarReader struct {
	io.Reader
	cleanup    func()
	cleanupRun bool
}

func (v *volumeTarReader) Read(p []byte) (int, error) {
	n, err := v.Reader.Read(p)
	if err == io.EOF && !v.cleanupRun {
		v.cleanupRun = true
		go v.cleanup() // removal need not block the caller
	}
	return n, err
}

func (v *volumeTarReader) Close() error {
	if !v.cleanupRun {
		v.cleanupRun = true
		go v.cleanup()
	}
	return nil
}

// Version returns the engine version string.
func (c *Client) Version(ctx context.Context) (string, error) {
	v, err := c.c.ServerVersion(ctx)
	if err != nil {
		return "", err
	}
	return v.Version, nil
}
