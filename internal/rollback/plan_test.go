package rollback

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"dockervc/internal/dockerapi"
	"dockervc/internal/model"
)

// fixtureManifest mirrors the demo environment shape: a web container on the
// demo network, a worker mounting the demo-data volume, a stopped sidecar
// that shares the worker's (deduplicated) filesystem object.
func fixtureManifest() *model.Manifest {
	webInspect, _ := json.Marshal(map[string]any{
		"Name":   "/demo-web",
		"Image":  "sha256:webbase",
		"Config": map[string]any{"Image": "nginx:alpine", "Env": []string{"K=V"}},
		"HostConfig": map[string]any{
			"NetworkMode":  "demo-net",
			"PortBindings": map[string]any{"80/tcp": []any{map[string]any{"HostPort": "8080"}}},
		},
		"NetworkSettings": map[string]any{
			"Networks": map[string]any{"demo-net": map[string]any{"Aliases": []string{"demo-web"}}},
		},
	})
	workerInspect, _ := json.Marshal(map[string]any{
		"Name":  "/demo-worker",
		"Image": "sha256:workerbase",
		"Config": map[string]any{"Image": "busybox:latest",
			"Cmd": []any{"sh", "-c", "while true; do sleep 5; done"}},
		"HostConfig": map[string]any{"Binds": []any{"demo-data:/data"}, "NetworkMode": "demo-net"},
		"Mounts":     []any{map[string]any{"Type": "volume", "Name": "demo-data", "Destination": "/data"}},
		"NetworkSettings": map[string]any{
			"Networks": map[string]any{"demo-net": map[string]any{}},
		},
	})
	sidecarInspect, _ := json.Marshal(map[string]any{
		"Name":            "/demo-sidecar",
		"Image":           "sha256:workerbase",
		"Config":          map[string]any{"Image": "busybox:latest", "Cmd": []any{"sleep", "infinity"}},
		"HostConfig":      map[string]any{"NetworkMode": "bridge"},
		"NetworkSettings": map[string]any{"Networks": map[string]any{"bridge": map[string]any{}}},
	})
	netInspect, _ := json.Marshal(map[string]any{
		"Name": "demo-net", "Driver": "bridge",
		"IPAM": map[string]any{
			"Driver": "default",
			"Config": []any{map[string]any{"Subnet": "172.18.0.0/16", "Gateway": "172.18.0.1"}},
		},
	})

	return &model.Manifest{
		ID: "snap-test-0001",
		Containers: []model.ContainerRecord{
			{Name: "demo-web", ID: "id-web", InspectJSON: webInspect,
				ImageObject: "objweb", Running: true},
			{Name: "demo-worker", ID: "id-worker", InspectJSON: workerInspect,
				ImageObject: "objworker", Running: true},
			{Name: "demo-sidecar", ID: "id-sidecar", InspectJSON: sidecarInspect,
				ImageObject: "objworker", Running: false},
		},
		Images: []model.ImageRecord{
			{Refs: []string{"nginx:alpine"}, Digest: "sha256:aaaa", Object: "objnginx"},
			{Refs: []string{"busybox:latest"}, Digest: "sha256:bbbb", Object: "objbusybox"},
		},
		Volumes: []model.VolumeRecord{
			{Name: "demo-data", Driver: "local", Object: "objdata"},
			{Name: "demo-cache", Driver: "local", Object: "objcache"},
		},
		Networks: []model.NetworkRecord{
			{Name: "demo-net", InspectJSON: netInspect},
		},
	}
}

// liveEmpty: an engine with nothing — every step fires.
func liveEmpty() *LiveState {
	return &LiveState{
		ContainerIDs: map[string]string{}, ContainerConfigHashes: map[string]string{},
		ContainerImages: map[string]string{}, ContainerImageRefs: map[string]string{},
		VolumeNames: map[string]bool{}, VolumeUsers: map[string][]ContainerUse{},
		VolumeStates: map[string]EntityState{}, VolumeDiffs: map[string]string{},
		NetworkNames: map[string]bool{}, NetworkInspects: map[string]json.RawMessage{},
		ImageDigests: map[string]bool{}, ImageTags: map[string]bool{},
		RunningNames: map[string]bool{},
	}
}

func markContainerUnchanged(t *testing.T, ls *LiveState, rec model.ContainerRecord) {
	t.Helper()
	hash, err := dockerapi.ContainerConfigHash(rec.InspectJSON)
	if err != nil {
		t.Fatalf("hash container config: %v", err)
	}
	ls.ContainerConfigHashes[rec.Name] = hash
	var facts struct {
		Image string `json:"Image"`
	}
	if err := json.Unmarshal(rec.InspectJSON, &facts); err != nil {
		t.Fatalf("parse image identity: %v", err)
	}
	ls.ContainerImages[rec.Name] = facts.Image
}

// liveConflict: the demo containers/volumes/network/images exist — removals,
// refills and skips are exercised.
func liveConflict() *LiveState {
	ls := liveEmpty()
	ls.ContainerIDs["demo-web"] = "live-web"
	ls.ContainerIDs["demo-worker"] = "live-worker"
	ls.RunningNames["demo-web"] = true
	ls.RunningNames["demo-worker"] = true
	ls.VolumeNames["demo-data"] = true
	ls.NetworkNames["demo-net"] = true
	ls.ImageDigests["sha256:aaaa"] = true // nginx present by digest
	ls.ImageTags["nginx:alpine"] = true
	return ls
}

// kindsInOrder asserts the §2 dependency order: removals → networks → images
// → volumes → creates → starts (enum values are sequential by design).
func kindsInOrder(t *testing.T, steps []Step) bool {
	t.Helper()
	last := -1
	for _, s := range steps {
		if int(s.Kind) < last {
			return false
		}
		last = int(s.Kind)
	}
	return true
}

func findStep(steps []Step, kind StepKind, name string) *Step {
	for i := range steps {
		if steps[i].Kind == kind && steps[i].Name == name {
			return &steps[i]
		}
	}
	return nil
}

// (a) full scope on an empty engine: every kind appears, in §2 order.
func TestBuildPlanOrderingAll(t *testing.T) {
	m := fixtureManifest()
	steps, _ := BuildPlan(m, liveEmpty(), Scope{All: true})
	if !kindsInOrder(t, steps) {
		t.Fatalf("steps out of dependency order:\n%s", stepList(steps))
	}
	for _, want := range []struct {
		kind StepKind
		name string
	}{
		{StepCreateNetwork, "demo-net"},
		{StepLoadImage, "nginx:alpine"},
		{StepLoadImage, "busybox:latest"},
		{StepLoadImage, RestoreTag(m.ID, "demo-web")},
		{StepRestoreVolume, "demo-data"},
		{StepRestoreVolume, "demo-cache"},
		{StepCreateContainer, "demo-web"},
		{StepCreateContainer, "demo-worker"},
		{StepCreateContainer, "demo-sidecar"},
		{StepStartContainer, "demo-web"},
		{StepStartContainer, "demo-worker"},
	} {
		if findStep(steps, want.kind, want.name) == nil {
			t.Fatalf("missing %d/%s in plan:\n%s", want.kind, want.name, stepList(steps))
		}
	}
	// The stopped sidecar is created but never started.
	if findStep(steps, StepStartContainer, "demo-sidecar") != nil {
		t.Fatal("sidecar recorded stopped — must not get a start step")
	}
	// Shared (deduped) filesystem: one load carries both restore tags.
	shared := findStep(steps, StepLoadImage, RestoreTag(m.ID, "demo-worker"))
	if shared == nil || len(shared.Refs) != 2 {
		t.Fatalf("worker/sidecar shared filesystem not loaded once with two tags: %+v", shared)
	}
}

// (b)+(d) conflicting engine: removals first with Why "name conflict",
// existing volume refilled, present-by-digest image skipped.
func TestBuildPlanConflicts(t *testing.T) {
	m := fixtureManifest()
	steps, warns := BuildPlan(m, liveConflict(), Scope{All: true})
	if !kindsInOrder(t, steps) {
		t.Fatalf("steps out of dependency order:\n%s", stepList(steps))
	}
	if len(steps) < 2 || steps[0].Kind != StepRemoveContainer || steps[1].Kind != StepRemoveContainer {
		t.Fatalf("conflicting containers must be removed first:\n%s", stepList(steps))
	}
	web := findStep(steps, StepRemoveContainer, "demo-web")
	if web == nil || web.ContainerID != "live-web" || web.Why != "name conflict" {
		t.Fatalf("demo-web removal step wrong: %+v", web)
	}
	data := findStep(steps, StepRestoreVolume, "demo-data")
	if data == nil || data.Why != "contents will be replaced" {
		t.Fatalf("existing volume must be a replace step: %+v", data)
	}
	if !hasStr(warns, "contents are replaced") {
		t.Fatalf("volume replacement not warned: %v", warns)
	}
	cache := findStep(steps, StepRestoreVolume, "demo-cache")
	if cache == nil || cache.Why != "missing" {
		t.Fatalf("missing volume step wrong: %+v", cache)
	}
	// nginx present by digest + tag → an honest skip step, no load.
	nginx := findStep(steps, StepLoadImage, "nginx:alpine")
	if nginx == nil || !nginx.Skip {
		t.Fatalf("present image should be a skip step: %+v", nginx)
	}
}

// (b) scope filtering: volumes-only touches nothing else.
func TestBuildPlanScopeVolumesOnly(t *testing.T) {
	m := fixtureManifest()
	steps, _ := BuildPlan(m, liveConflict(), Scope{Volumes: []string{"demo-data"}})
	if len(steps) != 1 || steps[0].Kind != StepRestoreVolume || steps[0].Name != "demo-data" {
		t.Fatalf("volumes-only plan must contain exactly the volume refill:\n%s", stepList(steps))
	}
}

// (c) implicit dependencies: selecting demo-worker pulls its network, its
// mounted volume and its filesystem image — and nothing else.
func TestBuildPlanContainerDependencies(t *testing.T) {
	m := fixtureManifest()
	steps, _ := BuildPlan(m, liveEmpty(), Scope{Containers: []string{"demo-worker"}})
	if findStep(steps, StepCreateNetwork, "demo-net") == nil {
		t.Fatal("worker's network dependency (demo-net) missing")
	}
	if findStep(steps, StepRestoreVolume, "demo-data") == nil {
		t.Fatal("worker's volume dependency (demo-data) missing")
	}
	if findStep(steps, StepRestoreVolume, "demo-cache") != nil {
		t.Fatal("demo-cache is not a dependency of demo-worker — must not be restored")
	}
	if findStep(steps, StepLoadImage, "nginx:alpine") != nil {
		t.Fatal("image records are not implicit container dependencies")
	}
	if findStep(steps, StepLoadImage, RestoreTag(m.ID, "demo-worker")) == nil {
		t.Fatal("worker's committed filesystem not loaded")
	}
	if findStep(steps, StepCreateContainer, "demo-sidecar") != nil {
		t.Fatal("sidecar not selected — must not be recreated (filesystem sharing is not an edge)")
	}
}

// (e) WriteTo renders entity names and actions for --dry-run.
func TestPlanWriteTo(t *testing.T) {
	m := fixtureManifest()
	steps, warns := BuildPlan(m, liveConflict(), Scope{All: true})
	p := &Plan{SnapshotID: m.ID, Steps: steps, Warnings: warns}
	var buf bytes.Buffer
	p.WriteTo(&buf)
	out := buf.String()
	for _, want := range []string{
		m.ID,
		"stop+remove container demo-web",
		"create container demo-worker",
		"restore volume demo-data",
		"start container demo-web",
		"load image",
		"warning:",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("dry-run output missing %q:\n%s", want, out)
		}
	}
}

// (f) ParseScopeArgs rejects unknown names and lists what the snapshot has.
func TestParseScopeArgs(t *testing.T) {
	m := fixtureManifest()

	if _, err := ParseScopeArgs(m, false, []string{"nope"}, nil, nil, nil); err == nil ||
		!strings.Contains(err.Error(), `unknown container "nope"`) ||
		!strings.Contains(err.Error(), "demo-web") {
		t.Fatalf("unknown container not rejected with the available list: %v", err)
	}
	if _, err := ParseScopeArgs(m, false, nil, []string{"nope"}, nil, nil); err == nil ||
		!strings.Contains(err.Error(), "demo-data") {
		t.Fatalf("unknown volume not rejected with the available list: %v", err)
	}
	if _, err := ParseScopeArgs(m, false, nil, nil, nil, []string{"nope"}); err == nil ||
		!strings.Contains(err.Error(), "demo-net") {
		t.Fatalf("unknown network not rejected with the available list: %v", err)
	}
	if _, err := ParseScopeArgs(m, false, nil, nil, []string{"nope:tag"}, nil); err == nil ||
		!strings.Contains(err.Error(), "nginx:alpine") {
		t.Fatalf("unknown image not rejected with the available list: %v", err)
	}

	// Images resolve by ref and by digest prefix to the record's digest.
	sc, err := ParseScopeArgs(m, false, nil, nil, []string{"busybox:latest", "sha256:aaa"}, nil)
	if err != nil {
		t.Fatalf("valid image selectors rejected: %v", err)
	}
	if len(sc.Images) != 2 || sc.Images[0] != "sha256:bbbb" || sc.Images[1] != "sha256:aaaa" {
		t.Fatalf("image selectors not resolved to digests: %v", sc.Images)
	}
}

// Restore tags present in the engine → filesystem loads become skips
// (partial-failure re-runs are cheap).
func TestBuildPlanSkipsLoadedFilesystems(t *testing.T) {
	m := fixtureManifest()
	ls := liveConflict()
	ls.ImageTags[RestoreTag(m.ID, "demo-web")] = true
	ls.ImageTags[RestoreTag(m.ID, "demo-worker")] = true
	ls.ImageTags[RestoreTag(m.ID, "demo-sidecar")] = true
	steps, _ := BuildPlan(m, ls, Scope{All: true})
	if s := findStep(steps, StepLoadImage, RestoreTag(m.ID, "demo-web")); s == nil || !s.Skip {
		t.Fatalf("already-loaded filesystem should skip: %+v", s)
	}
	if s := findStep(steps, StepLoadImage, "busybox:latest"); s == nil || s.Skip {
		t.Fatalf("busybox absent from engine — must load: %+v", s)
	}
}

func TestBuildPlanKeepsUnchangedContainer(t *testing.T) {
	m := fixtureManifest()
	ls := liveEmpty()
	rec := m.Containers[0]
	ls.ContainerIDs[rec.Name] = "live-web"
	ls.RunningNames[rec.Name] = true
	ls.NetworkNames["demo-net"] = true
	markContainerUnchanged(t, ls, rec)

	steps, warns := BuildPlan(m, ls, Scope{Containers: []string{rec.Name}})
	if len(steps) != 0 {
		t.Fatalf("unchanged container should require no actions:\n%s", stepList(steps))
	}
	if len(warns) != 0 {
		t.Fatalf("unchanged container warnings: %v", warns)
	}
}

func TestBuildPlanUnchangedVolumeRequiresNoAction(t *testing.T) {
	m := fixtureManifest()
	ls := liveEmpty()
	ls.VolumeNames["demo-data"] = true
	ls.VolumeStates["demo-data"] = EntityUnchanged
	ls.VolumeDiffs["demo-data"] = "file contents match"

	steps, _ := BuildPlan(m, ls, Scope{Volumes: []string{"demo-data"}})
	if len(steps) != 0 {
		t.Fatalf("unchanged volume should require no actions:\n%s", stepList(steps))
	}
}

func TestBuildPlanChangedSharedVolumeStopsAndRestartsUsers(t *testing.T) {
	m := fixtureManifest()
	ls := liveEmpty()
	ls.VolumeNames["demo-data"] = true
	ls.VolumeStates["demo-data"] = EntityChanged
	ls.VolumeDiffs["demo-data"] = "+1 created, ~2 modified"
	ls.VolumeUsers["demo-data"] = []ContainerUse{
		{Name: "demo-worker", ID: "worker-id", Running: true},
		{Name: "observer", ID: "observer-id", Running: true, ReadOnly: true},
	}
	ls.ContainerIDs["demo-worker"] = "worker-id"
	ls.ContainerIDs["observer"] = "observer-id"
	ls.RunningNames["demo-worker"] = true
	ls.RunningNames["observer"] = true

	steps, warns := BuildPlan(m, ls, Scope{Volumes: []string{"demo-data"}})
	for _, tc := range []struct {
		kind StepKind
		name string
	}{
		{StepStopContainer, "demo-worker"},
		{StepStopContainer, "observer"},
		{StepRestoreVolume, "demo-data"},
		{StepStartContainer, "demo-worker"},
		{StepStartContainer, "observer"},
	} {
		if findStep(steps, tc.kind, tc.name) == nil {
			t.Fatalf("missing shared-volume action %v/%s:\n%s", tc.kind, tc.name, stepList(steps))
		}
	}
	if !hasStr(warns, "shared by") || !hasStr(warns, "outside the selected scope") {
		t.Fatalf("shared-volume warnings missing: %v", warns)
	}
}

func hasStr(list []string, sub string) bool {
	for _, s := range list {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

func stepList(steps []Step) string {
	var sb strings.Builder
	for i, s := range steps {
		fmt.Fprintf(&sb, "  %2d. %s\n", i+1, s)
	}
	return sb.String()
}

func TestRestoreTag(t *testing.T) {
	// The tag is what `docker images` shows for a restored container
	// filesystem — it must read as "<name>-restored-from-<snapshot>" and
	// stay a legal docker reference (lowercase, no spaces).
	got := RestoreTag("snap-20260912-160108-9246", "Demo_Web.1")
	want := "demo_web.1-restored-from-snap-20260912-160108-9246"
	if got != want {
		t.Fatalf("RestoreTag = %q, want %q", got, want)
	}
	// names docker would reject (spaces, capitals, slashes) are scrubbed
	if g := RestoreTag("snap-x", "My App/v2"); g != "my-app-v2-restored-from-snap-x" {
		t.Fatalf("scrubbing: got %q", g)
	}
	// same container + snapshot always maps to the same tag (re-run skip)
	if RestoreTag("snap-x", "demo-web") != RestoreTag("snap-x", "demo-web") {
		t.Fatal("RestoreTag must be deterministic")
	}
}
