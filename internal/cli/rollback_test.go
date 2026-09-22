package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"dockervc/internal/model"
	"dockervc/internal/rollback"
)

func TestPrepareImageNameReplacementOffersDependencyModesOnlyWhenAvailable(t *testing.T) {
	m := &model.Manifest{
		ID: "snap-test",
		Containers: []model.ContainerRecord{{
			Name: "app", ImageID: "sha256:snapshot", ImageKey: "sha256:snapshot",
			ImageRef: "example/app:latest", Running: true,
			InspectJSON: []byte(`{
				"Config":{"Image":"example/app:latest"},
				"HostConfig":{"Binds":["app-data:/data"]},
				"NetworkSettings":{"Networks":{"app-net":{}}}
			}`),
		}},
		Images: []model.ImageRecord{{
			ID: "sha256:snapshot", Key: "sha256:snapshot", Digest: "sha256:snapshot",
			Refs: []string{"example/app:latest"}, Object: "object",
		}},
	}
	live := &rollback.LiveState{
		ContainerIDs: map[string]string{},
		ImageTags:    map[string]bool{"example/app:latest": true},
		ImageTagIDs:  map[string]string{"example/app:latest": "sha256:current"},
		ImageDigests: map[string]bool{"sha256:current": true},
		VolumeNames:  map[string]bool{},
		NetworkNames: map[string]bool{},
	}
	scope := rollback.Scope{Containers: []string{"app"}}

	run := func() (rollback.Scope, string) {
		cmd := &cobra.Command{}
		var stderr bytes.Buffer
		cmd.SetErr(&stderr)
		got, proceed := prepareImageNameReplacement(cmd, m, live, scope, true)
		if !proceed || !got.ReplaceConflictingImageNames {
			t.Fatalf("dry-run conflict preflight = %+v/proceed=%v", got, proceed)
		}
		return got, stderr.String()
	}

	_, output := run()
	if !strings.Contains(output, "image name conflict") || strings.Contains(output, "--reuse-existing-by-name") {
		t.Fatalf("missing dependencies should call out conflict without reuse alternatives:\n%s", output)
	}

	live.VolumeNames["app-data"] = true
	live.NetworkNames["app-net"] = true
	_, output = run()
	for _, want := range []string{"--reuse-existing-by-name", "--recreate-with-current-dependencies"} {
		if !strings.Contains(output, want) {
			t.Fatalf("available named dependencies should offer %s:\n%s", want, output)
		}
	}
}
