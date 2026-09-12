// Package rollback plans and applies the restoration of a snapshot onto a
// live engine. Planning is pure (manifest + live-state in, ordered steps
// out) so it is unit-testable and dry-runnable; applying lives in apply.go.
package rollback

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"dockervc/internal/dockerapi"
	"dockervc/internal/model"
)

// StepKind identifies what a Step does to the engine.
type StepKind int

const (
	StepRemoveContainer StepKind = iota
	StepCreateNetwork
	StepLoadImage
	StepRestoreVolume
	StepCreateContainer
	StepStartContainer
)

// Step is one atomic action against the engine. Steps are value types: the
// plan is built up front (dry-runnable) and applied strictly in order.
type Step struct {
	Kind        StepKind
	Name        string   // entity name / image ref
	Why         string   // "name conflict", "missing", "recorded running"…
	Skip        bool     // plan-time no-op, shown for honesty in dry-run output
	ContainerID string   // remove step: live container id
	InspectJSON json.RawMessage // network/container steps
	ObjectHash  string   // image/volume steps (CAS hash)
	Refs        []string // image re-tags to apply after loading
	Vol         model.VolumeRecord
	Running     bool // container step → follow-up start
}

// String renders the dry-run line for the step.
func (s Step) String() string {
	switch s.Kind {
	case StepRemoveContainer:
		return fmt.Sprintf("stop+remove container %s (%s)", s.Name, s.Why)
	case StepCreateNetwork:
		return fmt.Sprintf("create network %s (%s)", s.Name, s.Why)
	case StepLoadImage:
		if s.Skip {
			return fmt.Sprintf("skip image %s (%s)", s.Name, s.Why)
		}
		if s.ObjectHash == "" { // tag-only: Name is the present source ref
			return fmt.Sprintf("tag image %s as %s (%s)", shortRef(s.Name), strings.Join(s.Refs, ", "), s.Why)
		}
		return fmt.Sprintf("load image %s (%s)", s.Name, s.Why)
	case StepRestoreVolume:
		return fmt.Sprintf("restore volume %s (%s)", s.Name, s.Why)
	case StepCreateContainer:
		return fmt.Sprintf("create container %s (%s)", s.Name, s.Why)
	case StepStartContainer:
		return fmt.Sprintf("start container %s (%s)", s.Name, s.Why)
	}
	return fmt.Sprintf("?? step %d %s", s.Kind, s.Name)
}

// LiveState is the engine inventory a plan is built against — everything the
// conflict/skip decisions need, fetched once.
type LiveState struct {
	ContainerIDs    map[string]string // container name → live container ID
	VolumeNames     map[string]bool
	NetworkNames    map[string]bool
	NetworkInspects map[string]json.RawMessage // live network inspect by name (drift check)
	ImageDigests    map[string]bool            // image IDs + repo digests (capture's dedup key)
	ImageTags       map[string]bool            // repo tags (docker's "<none>:<none>" excluded)
	RunningNames    map[string]bool
}

// FetchLiveState inventories the engine. Network inspects ride along because
// reusing an existing network is only honest when it still matches what the
// snapshot recorded.
func FetchLiveState(ctx context.Context, cli *dockerapi.Client) (*LiveState, error) {
	ls := &LiveState{
		ContainerIDs:    map[string]string{},
		VolumeNames:     map[string]bool{},
		NetworkNames:    map[string]bool{},
		NetworkInspects: map[string]json.RawMessage{},
		ImageDigests:    map[string]bool{},
		ImageTags:       map[string]bool{},
		RunningNames:    map[string]bool{},
	}

	cts, err := cli.ListContainers(ctx)
	if err != nil {
		return nil, err
	}
	for _, ct := range cts {
		if len(ct.Names) == 0 {
			continue
		}
		name := strings.TrimPrefix(ct.Names[0], "/")
		ls.ContainerIDs[name] = ct.ID
		if strings.Contains(string(ct.State), "running") {
			ls.RunningNames[name] = true
		}
	}

	vols, err := cli.ListVolumes(ctx)
	if err != nil {
		return nil, err
	}
	for _, v := range vols {
		ls.VolumeNames[v.Name] = true
	}

	// network.Summary is the full Inspect shape, so no per-network call is
	// needed for the drift check.
	nets, err := cli.ListNetworks(ctx)
	if err != nil {
		return nil, err
	}
	for _, n := range nets {
		ls.NetworkNames[n.Name] = true
		if raw, err := json.Marshal(n); err == nil {
			ls.NetworkInspects[n.Name] = raw
		}
	}

	imgs, err := cli.ListImages(ctx)
	if err != nil {
		return nil, err
	}
	for _, img := range imgs {
		ls.ImageDigests[img.ID] = true
		for _, d := range img.RepoDigests {
			ls.ImageDigests[d] = true
		}
		for _, t := range img.RepoTags {
			if t != "<none>:<none>" { // dangling images carry a pseudo-tag
				ls.ImageTags[t] = true
			}
		}
	}
	return ls, nil
}

// Scope names what a rollback touches. All selects everything; otherwise each
// list names entities, and BuildPlan adds the implicit dependencies (a
// selected container pulls in its filesystem image, mounted named volumes
// and networks).
type Scope struct {
	All        bool
	Containers []string
	Volumes    []string
	Images     []string // image refs or digests, resolved to digests here
	Networks   []string
}

// ParseScopeArgs validates scope names against the manifest. Unknown names
// are errors listing what the snapshot actually has — a rollback is the one
// command where guessing must never happen.
func ParseScopeArgs(m *model.Manifest, all bool, containers, volumes, images, networks []string) (Scope, error) {
	sc := Scope{All: all, Containers: containers, Volumes: volumes, Networks: networks}

	if err := checkNames("container", containers, containerNames(m)); err != nil {
		return sc, err
	}
	if err := checkNames("volume", volumes, volumeNames(m)); err != nil {
		return sc, err
	}
	if err := checkNames("network", networks, networkNames(m)); err != nil {
		return sc, err
	}

	for _, sel := range images {
		digest, err := resolveImageSelector(m, sel)
		if err != nil {
			return sc, err
		}
		sc.Images = append(sc.Images, digest)
	}
	return sc, nil
}

func checkNames(kind string, want, have []string) error {
	known := map[string]bool{}
	for _, n := range have {
		known[n] = true
	}
	for _, n := range want {
		if !known[n] {
			return fmt.Errorf("unknown %s %q (snapshot has: %s)", kind, n, strings.Join(have, ", "))
		}
	}
	return nil
}

// resolveImageSelector matches an image by exact ref, exact digest, or a
// digest/ref prefix — mirroring snapshot-id prefix resolution.
func resolveImageSelector(m *model.Manifest, sel string) (string, error) {
	var prefixHits []string
	for i := range m.Images {
		rec := &m.Images[i]
		for _, ref := range rec.Refs {
			if ref == sel {
				return rec.Digest, nil
			}
		}
		if rec.Digest == sel {
			return rec.Digest, nil
		}
		if strings.HasPrefix(rec.Digest, sel) {
			prefixHits = append(prefixHits, rec.Digest)
		}
	}
	if len(prefixHits) == 1 {
		return prefixHits[0], nil
	}
	if len(prefixHits) > 1 {
		return "", fmt.Errorf("ambiguous image %q matches %d images — use a full digest", sel, len(prefixHits))
	}
	return "", fmt.Errorf("unknown image %q (snapshot has: %s)", sel, imageNames(m))
}

func containerNames(m *model.Manifest) []string {
	out := make([]string, 0, len(m.Containers))
	for i := range m.Containers {
		out = append(out, m.Containers[i].Name)
	}
	return out
}

func volumeNames(m *model.Manifest) []string {
	out := make([]string, 0, len(m.Volumes))
	for i := range m.Volumes {
		out = append(out, m.Volumes[i].Name)
	}
	return out
}

func networkNames(m *model.Manifest) []string {
	out := make([]string, 0, len(m.Networks))
	for i := range m.Networks {
		out = append(out, m.Networks[i].Name)
	}
	return out
}

func imageNames(m *model.Manifest) []string {
	var out []string
	for i := range m.Images {
		if len(m.Images[i].Refs) > 0 {
			out = append(out, m.Images[i].Refs[0])
		} else {
			out = append(out, shortRef(m.Images[i].Digest))
		}
	}
	return out
}

// Plan is a snapshot restoration, ready to print or apply.
type Plan struct {
	SnapshotID string
	Steps      []Step
	Warnings   []string
}

// WriteTo renders the plan: header, numbered steps, warnings. Used by both
// --dry-run and the pre-confirmation summary. It implements io.WriterTo.
func (p *Plan) WriteTo(w io.Writer) (int64, error) {
	var sb strings.Builder
	fmt.Fprintf(&sb, "rollback plan for %s — %d step(s):\n", p.SnapshotID, len(p.Steps))
	for i, s := range p.Steps {
		fmt.Fprintf(&sb, "  %2d. %s\n", i+1, s)
	}
	for _, warn := range p.Warnings {
		fmt.Fprintf(&sb, "  warning: %s\n", warn)
	}
	n, err := io.WriteString(w, sb.String())
	return int64(n), err
}

// HasTag reports whether the engine has a repo tag. Docker normalizes bare
// repos to "<repo>:latest" in RepoTags, so a tag applied as "a/b/c" reads
// back as "a/b/c:latest" — both spellings count as present.
func (ls *LiveState) HasTag(ref string) bool {
	return ls.ImageTags[ref] || ls.ImageTags[ref+":latest"]
}

// RestoreTag is the deterministic tag under which a container's restored
// filesystem is loaded — mirrors capture's dockervc/snap/<id>/<name> tagging
// and makes partial-failure re-runs skip already-loaded images.
func RestoreTag(snapshotID, containerName string) string {
	return "dockervc/restore/" + snapshotID + "/" + sanitize(containerName)
}

// sanitize matches capture's container-name scrubbing so snap and restore
// tags of the same container line up.
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

func shortRef(ref string) string {
	if len(ref) > 19 {
		return ref[:19]
	}
	return ref
}

// ── plan building ─────────────────────────────────────────────────────────────

// BuildPlan turns a manifest, the live engine state and a scope into ordered
// steps (removals → networks → images → volumes → creates → starts) plus
// non-fatal warnings. It touches neither the engine nor the store.
func BuildPlan(m *model.Manifest, live *LiveState, scope Scope) (steps []Step, warnings []string) {
	warn := func(format string, args ...any) {
		warnings = append(warnings, fmt.Sprintf(format, args...))
	}

	selC := selectContainers(m, scope)
	selI := selectImages(m, scope)

	// Implicit dependencies ride in with selected containers: their mounted
	// volumes and attached networks, all resolved against what this snapshot
	// actually captured.
	needVolumes := map[string]bool{}
	needNetworks := map[string]bool{}
	if scope.All {
		for i := range m.Volumes {
			needVolumes[m.Volumes[i].Name] = true
		}
		for i := range m.Networks {
			needNetworks[m.Networks[i].Name] = true
		}
	} else {
		for _, n := range scope.Volumes {
			needVolumes[n] = true
		}
		for _, n := range scope.Networks {
			needNetworks[n] = true
		}
	}
	for i := range selC {
		crec := &selC[i]
		deps, err := containerDeps(crec.InspectJSON)
		if err != nil {
			warn("container %s: stored inspect unparseable (%v); volume/network dependencies may be missed", crec.Name, err)
			continue
		}
		for _, volName := range deps.VolumeNames {
			if hasVolume(m, volName) {
				needVolumes[volName] = true
			} else if isAnonymousVolume(volName) {
				warn("container %s mounts anonymous volume %s, which the snapshot did not capture; it is recreated empty", crec.Name, volName)
			} else if !isHostPathBind(volName) {
				warn("container %s mounts volume %s, which this snapshot did not capture; the engine recreates it empty", crec.Name, volName)
			}
		}
		for _, netName := range deps.NetworkNames {
			if hasNetwork(m, netName) {
				needNetworks[netName] = true
			} else if !isBuiltinNetwork(netName) {
				warn("container %s attaches to network %s, which this snapshot did not capture; creation fails unless it exists", crec.Name, netName)
			}
		}
		for _, src := range deps.BindSources {
			if _, err := os.Stat(src); err != nil {
				warn("bind source %s (container %s) is missing on this host; the bind is kept and creation may fail", src, crec.Name)
			}
		}
	}

	// 1. Stop+remove conflicting containers — unique names block create, and
	// running holders pin the volumes/networks later steps replace.
	for i := range selC {
		crec := &selC[i]
		if id, ok := live.ContainerIDs[crec.Name]; ok {
			steps = append(steps, Step{
				Kind: StepRemoveContainer, Name: crec.Name,
				ContainerID: id, Why: "name conflict",
			})
		}
	}

	// 2. Create missing networks (reuse existing ones, never delete).
	for i := range m.Networks {
		nrec := &m.Networks[i]
		if !needNetworks[nrec.Name] {
			continue
		}
		var probe struct {
			ConfigOnly bool `json:"ConfigOnly"`
		}
		if json.Unmarshal(nrec.InspectJSON, &probe) == nil && probe.ConfigOnly {
			warn("network %s is config-only in the snapshot; skipped", nrec.Name)
			continue
		}
		if live.NetworkNames[nrec.Name] {
			if drift := networkDrift(nrec.Name, nrec.InspectJSON, live.NetworkInspects[nrec.Name]); drift != "" {
				warn("network %s exists and %s — reusing the live network", nrec.Name, drift)
			}
			continue
		}
		steps = append(steps, Step{
			Kind: StepCreateNetwork, Name: nrec.Name,
			InspectJSON: nrec.InspectJSON, Why: "missing",
		})
	}

	// 3. Load images: selected image records, then the committed filesystems
	// of selected containers (grouped by object so shared/deduped filesystems
	// load once and carry one restore tag per container).
	for i := range m.Images {
		irec := &m.Images[i]
		if !selI[irec.Digest] {
			continue
		}
		if irec.Digest != "" && live.ImageDigests[irec.Digest] {
			var missing []string
			for _, ref := range irec.Refs {
				if !live.ImageTags[ref] {
					missing = append(missing, ref)
				}
			}
			if len(missing) == 0 {
				steps = append(steps, Step{
					Kind: StepLoadImage, Skip: true,
					Name: imageDisplayName(irec), Why: "already present",
				})
				continue
			}
			steps = append(steps, Step{
				Kind: StepLoadImage, Name: irec.Digest, Refs: missing,
				Why: "already present; re-applying missing tags",
			})
			continue
		}
		steps = append(steps, Step{
			Kind: StepLoadImage, Name: imageDisplayName(irec),
			ObjectHash: irec.Object, Refs: irec.Refs, Why: "missing",
		})
	}

	// Committed container filesystems have no repo digest, so presence is
	// keyed by the deterministic restore tag. Groups are emitted in manifest
	// (first-seen) order — map iteration would shuffle the plan.
	fsOwners := map[string][]string{} // object hash → container names, first-seen
	for i := range selC {
		crec := &selC[i]
		if crec.ImageObject != "" {
			fsOwners[crec.ImageObject] = append(fsOwners[crec.ImageObject], crec.Name)
		}
	}
	seenHash := map[string]bool{}
	for i := range selC {
		crec := &selC[i]
		if crec.ImageObject == "" || seenHash[crec.ImageObject] {
			continue
		}
		seenHash[crec.ImageObject] = true
		owners := fsOwners[crec.ImageObject]
		var missing []string
		for _, name := range owners {
			if !live.HasTag(RestoreTag(m.ID, name)) {
				missing = append(missing, RestoreTag(m.ID, name))
			}
		}
		first := RestoreTag(m.ID, owners[0])
		if len(missing) == 0 {
			steps = append(steps, Step{
				Kind: StepLoadImage, Skip: true, Name: first,
				Why: "filesystem already loaded",
			})
			continue
		}
		steps = append(steps, Step{
			Kind: StepLoadImage, Name: first, ObjectHash: crec.ImageObject, Refs: missing,
			Why: fmt.Sprintf("filesystem for %s", strings.Join(owners, ", ")),
		})
	}

	// 4. Restore volumes — before containers start, so startup writes see
	// restored content. Clear-then-copy: post-snapshot files do not survive.
	for i := range m.Volumes {
		vrec := &m.Volumes[i]
		if !needVolumes[vrec.Name] {
			continue
		}
		why := "missing"
		if live.VolumeNames[vrec.Name] {
			why = "contents will be replaced"
			warn("volume %s exists — its current contents are replaced by the snapshot's", vrec.Name)
		}
		steps = append(steps, Step{
			Kind: StepRestoreVolume, Name: vrec.Name,
			ObjectHash: vrec.Object, Vol: *vrec, Why: why,
		})
	}

	// 5. Create containers, in manifest order.
	for i := range selC {
		crec := &selC[i]
		if crec.ImageObject == "" {
			warn("container %s has no committed filesystem in the snapshot; not recreated", crec.Name)
			continue
		}
		steps = append(steps, Step{
			Kind: StepCreateContainer, Name: crec.Name,
			InspectJSON: crec.InspectJSON,
			Why:         "recreate from snapshot",
		})
	}

	// 6. Start recorded-running containers — last, after peers/deps exist.
	for i := range selC {
		crec := &selC[i]
		if crec.ImageObject == "" || !crec.Running {
			continue
		}
		steps = append(steps, Step{
			Kind: StepStartContainer, Name: crec.Name,
			Why: "recorded running",
		})
	}

	return steps, warnings
}

func selectContainers(m *model.Manifest, scope Scope) []model.ContainerRecord {
	if scope.All {
		return m.Containers
	}
	if len(scope.Containers) == 0 {
		return nil
	}
	want := map[string]bool{}
	for _, n := range scope.Containers {
		want[n] = true
	}
	var out []model.ContainerRecord
	for i := range m.Containers {
		if want[m.Containers[i].Name] {
			out = append(out, m.Containers[i])
		}
	}
	return out
}

func selectImages(m *model.Manifest, scope Scope) map[string]bool {
	out := map[string]bool{}
	for i := range m.Images {
		if scope.All {
			out[m.Images[i].Digest] = true
		}
	}
	if !scope.All {
		for _, d := range scope.Images {
			out[d] = true
		}
	}
	return out
}

func hasVolume(m *model.Manifest, name string) bool {
	for i := range m.Volumes {
		if m.Volumes[i].Name == name {
			return true
		}
	}
	return false
}

func hasNetwork(m *model.Manifest, name string) bool {
	for i := range m.Networks {
		if m.Networks[i].Name == name {
			return true
		}
	}
	return false
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

// isHostPathBind: Binds entries that are not host paths name volumes
// ("vol:/dst"), so anything starting with "/" (or containing ":\") is a path.
func isHostPathBind(src string) bool {
	return strings.HasPrefix(src, "/") || strings.Contains(src, ":\\")
}

func imageDisplayName(rec *model.ImageRecord) string {
	if len(rec.Refs) > 0 {
		return rec.Refs[0]
	}
	return shortRef(rec.Digest)
}

// containerDeps pulls the dependency names out of a stored container inspect
// with small anonymous shapes — the full SDK unmarshal belongs to
// dockerapi (creation); planning only needs names.
type containerDepsResult struct {
	VolumeNames  []string // named volumes, from Mounts and -v binds alike
	NetworkNames []string
	BindSources  []string // host paths from Binds
}

func containerDeps(raw []byte) (*containerDepsResult, error) {
	var probe struct {
		Mounts []struct {
			Type string `json:"Type"`
			Name string `json:"Name"`
		} `json:"Mounts"`
		HostConfig struct {
			Binds []string `json:"Binds"`
		} `json:"HostConfig"`
		NetworkSettings struct {
			Networks map[string]json.RawMessage `json:"Networks"`
		} `json:"NetworkSettings"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, err
	}
	out := &containerDepsResult{}
	for _, mnt := range probe.Mounts {
		if mnt.Type == "volume" && mnt.Name != "" {
			out.VolumeNames = append(out.VolumeNames, mnt.Name)
		}
	}
	for _, bind := range probe.HostConfig.Binds {
		src, _, _ := strings.Cut(bind, ":")
		switch {
		case src == "":
		case isHostPathBind(src):
			out.BindSources = append(out.BindSources, src)
		default:
			out.VolumeNames = append(out.VolumeNames, src) // named volume in a -v bind
		}
	}
	for name := range probe.NetworkSettings.Networks {
		out.NetworkNames = append(out.NetworkNames, name)
	}
	return out, nil
}

// networkDrift compares what the snapshot recorded against the live network
// of the same name; "" means they match closely enough to reuse silently.
func networkDrift(name string, stored, liveJSON json.RawMessage) string {
	type netProbe struct {
		Driver string `json:"Driver"`
		IPAM   struct {
			Config []struct {
				Subnet string `json:"Subnet"`
			} `json:"Config"`
		} `json:"IPAM"`
	}
	var s, l netProbe
	if err := json.Unmarshal(stored, &s); err != nil {
		return ""
	}
	if len(liveJSON) == 0 || json.Unmarshal(liveJSON, &l) != nil {
		return ""
	}
	if s.Driver != l.Driver {
		return fmt.Sprintf("uses driver %q where the snapshot recorded %q", l.Driver, s.Driver)
	}
	subnets := func(p netProbe) []string {
		var out []string
		for _, c := range p.IPAM.Config {
			if c.Subnet != "" {
				out = append(out, c.Subnet)
			}
		}
		return out
	}
	ss, ls := subnets(s), subnets(l)
	if strings.Join(ss, ",") != strings.Join(ls, ",") {
		return fmt.Sprintf("uses subnets [%s] where the snapshot recorded [%s]",
			strings.Join(ls, ", "), strings.Join(ss, ", "))
	}
	return ""
}
