# Implementation Plan — Basic Rollback (`dockervc rollback`, Step 3)

Status: APPROVED for implementation. The three open decisions are LOCKED at the
end of this file. Read HANDOFF.md (§0 ground rules, §2 data model, §4 rollback
spec) before coding; this plan was validated against the code, the Docker SDK
v28.5.2 sources, and the live store.

## 1. Basic-tier scope

Keep the stub's promised flag surface exactly (HANDOFF §4 is binding):

```
dockervc rollback <snap> [--all] [--containers a,b] [--volumes x,y]
                     [--images i,j] [--networks n] [--dry-run]
                     [--yes] [--keep-current]
```

Name-level targeting (not snapshot-style `--only`): restore is destructive, so
granularity is per entity, and the dry-run plan / confirmation can quote exactly
what gets touched. No scope flags → interactive per-kind prompts (see §5).
`--dry-run` prints the plan, changes nothing, exits 0.

In scope:
- Networks: create from pruned inspect JSON; reuse-if-exists (never delete).
- Volumes: create-if-missing + exact content restore from the stored tar
  (clear-then-copy — LOCKED decision, see below).
- Images: load only what is needed — selected `ImageRecord`s whose digest is
  absent from the engine, plus every selected container's `ImageObject`.
  Present-by-digest images only get missing repo tags re-applied (`ImageTag`).
- Containers: recreate from `InspectJSON` per §3 field table; start only those
  recorded `Running: true`.
- Pre-rollback checkpoint ON by default (reuses `snapshot.Capturer` verbatim,
  message `pre-rollback checkpoint before <snapID>`); `--keep-current` skips;
  checkpoint failure aborts the rollback.
- Refuse broken snapshots: `missingObjects(st, m)` (internal/cli/doctor.go)
  non-empty, or `GetSnapshot` error → hard error before any planning.

Out of scope (documented as deferred): bind-mount content restore (plan-time
warning only when the host source is missing), per-container static IPs
(`IPAMConfig` dropped; engine reassigns), hostname preservation, transactional
rollback-of-the-rollback.

## 2. Restore ordering (dependency-driven; flat ordered step list, applied strictly)

1. **Stop+remove conflicting containers** — only containers whose names are
   about to be recreated (unique names block create; running conflicts hold
   volumes/networks).
2. **Create networks** — containers reference networks by name at create time.
3. **Load images** — before volumes: if the engine is empty, VolumeRestore's
   helper container needs an image, and a just-loaded snapshot image satisfies
   `pickHelperImage` (stream.go), avoiding the one network pull dockervc allows.
4. **Restore volumes** — before containers start, so startup writes see
   restored content.
5. **Create containers** — in manifest order, from loaded committed-image IDs.
6. **Start recorded-running containers** — last, after all peers/deps exist.

## 3. Per-resource mechanics

New thin wrappers in `internal/dockerapi/restore.go` (SDK
`github.com/docker/docker/client` v28.5.2; same style as stream.go):

```go
func (c *Client) ImageLoad(ctx context.Context, r io.Reader) error            // Quiet:true; drains+closes resp.Body or the load never completes
func (c *Client) ImageTag(ctx context.Context, source, target string) error
func networkCreateOptions(raw []byte) (name string, opts network.CreateOptions, err error) // pure
func (c *Client) NetworkCreateFromInspect(ctx context.Context, raw json.RawMessage) error
func (c *Client) VolumeCreate(ctx context.Context, name, driver string, opts map[string]string) error
func (c *Client) VolumeRestore(ctx context.Context, volumeName string, tarStream io.Reader) error
func containerCreateConfig(raw []byte, imageName string) (*container.Config, *container.HostConfig, *network.NetworkingConfig, string, error) // pure
func (c *Client) ContainerCreateFromInspect(ctx context.Context, inspectJSON json.RawMessage, imageName string) (id string, warnings []string, err error)
func (c *Client) ContainerRemove(ctx context.Context, id string, force bool) error
```

### Networks
`networkCreateOptions` unmarshals the stored `network.Inspect` JSON (live-store
shape verified: `Name, Id, Created, Scope, Driver, EnableIPv4/6, IPAM{Driver,
Options, Config[{Subnet,IPRange,Gateway,AuxAddress}]}, Internal, Attachable,
Options, Labels, Containers, …`) → `network.CreateOptions{Driver, EnableIPv4*,
EnableIPv6*, Internal, Attachable, Options, Labels, IPAM}` (pointers only when
true; IPAM.Config copied field-by-field).
**Never sent:** `Id, Created, Scope, Containers, Peers, Services,
ConfigFrom/ConfigOnly` (warn+skip the record if `ConfigOnly` was true).
`client.NetworkCreate(ctx, name, opts)`; name from the record, not the JSON.

### Volumes
Ensure: `client.VolumeCreate(ctx, volume.CreateOptions{Name, Driver,
DriverOpts: rec.Options})` — idempotent for an existing identical volume.
Content (`VolumeRestore`), mirroring `VolumeTarStream` in stream.go:
1. `helper := pickHelperImage(ctx)` (reuse).
2. Create helper: `container.Config{Image: helper, Cmd: ["sleep","300"],
   NetworkDisabled: true, Labels: {"dockervc":"helper"}}`,
   `HostConfig{Binds: [name + ":/dst"]}` (**rw**, unlike capture's `:ro`),
   name `dockervc-restore-<hex3>`; start it.
3. Clear for exact restore: exec `sh -c "find /dst -mindepth 1 -delete"`
   (Detach true), poll `ContainerExecInspect` until not Running, fail on
   non-zero ExitCode. (Alternative if exec feels heavy: a throwaway container
   running the delete as its Cmd — acceptable.)
4. `client.CopyToContainer(ctx, id, "/dst", tarStream, ...)` with
   `tarStream = st.OpenObject(hash)` (already decompressed, entries rooted at
   `./`). Streaming only — never materialize to disk.
5. `ContainerRemove(Force: true)` deferred/best-effort, same as
   `volumeTarReader.cleanup`.

### Images
- Per needed object: `st.OpenObject(hash)` → `ImageLoad` (drain + close Body!).
- Skip-if-present: `ImageRecord`s compare `rec.Digest` against live
  `image.Summary.ID` + `RepoDigests` (capture's dedup key, capture.go:379).
- Container `ImageObject`s (committed images have no repo digest): after each
  load, tag deterministically `<sanitized-name>-restored-from-<snapID>`;
  skip the load when the tag already exists in ImageList. Makes
  partial-failure re-runs cheap.
- After loading an ImageRecord object, re-apply recorded tags via `ImageTag`.
- Keep `map[hash]imageID` so container steps reference loaded images without
  re-loading.

### Containers
`containerCreateConfig(raw, imageName)` from stored `types.ContainerJSON`:

| Source | Destination | Notes |
|---|---|---|
| `Config.{Env,Cmd,Entrypoint,WorkingDir,User,Labels,StopSignal,Tty,OpenStdin}` + `Healthcheck, StopTimeout, ExposedPorts, NetworkDisabled, Domainname` | new `container.Config` | verbatim; `ExposedPorts` keeps `docker ps` port display honest |
| — | `Config.Image = imageName` | loaded committed-image ID; **drop** recorded `Config.Image` |
| — | drop `Config.Hostname` | engine assigns; recorded value is the old container ID |
| `HostConfig.{Binds,PortBindings,RestartPolicy,Privileged,ReadonlyRootfs,ExtraHosts,LogConfig,CapAdd,CapDrop}` + embedded `Resources` wholesale (`Memory, NanoCPUs, CpuShares, CpusetCpus/Mems, MemoryReservation/Swap, PidsLimit, Ulimits, …`) + `AutoRemove, SecurityOpt, Sysctls, Tmpfs, DNS*, Links, PidMode, IpcMode, ShmSize` | new `container.HostConfig` | copying the embedded `Resources` struct in one assignment covers all memory/CPU fields |
| `NetworkSettings.Networks` | `network.NetworkingConfig{EndpointsConfig}` with `Aliases, Links, DriverOpts` | **`IPAMConfig = nil`**; drop operational fields (`NetworkID, EndpointID, IPAddress, Gateway, MacAddress, DNSNames, …`) — engine reassigns |
| `HostConfig.NetworkMode` | `NetworkMode` | container's primary (first) network name |
| `ContainerRecord.Name` | create-time name | authoritative; compose labels ride in `Config.Labels` |

Verify at implementation (Docker 28 daemon): whether `HostConfig.Mounts` must
be copied alongside `Binds` — a stale/empty `Mounts` next to live `Binds` is
the field to watch on the demo env.
`client.ContainerCreate(ctx, cfg, hc, nc, nil /* same engine */, name)`; surface
`CreateResponse.Warnings` as plan warnings; if `rec.Running` →
`ContainerStart`.

## 4. Conflict + partial-failure policy

**One gate: confirmation IS the force flag** (no `--force`). The prompt lists
what will be removed (conflicting containers) and replaced (volume contents).
Per type after the gate:
- Container name exists → stop (`StopContainer(ctx, id, 30)`) + remove + recreate.
- Volume exists → reuse, clear + refill (exact restore); warn "contents replaced".
- Network exists → reuse, never delete; warn if driver/IPAM subnet differs.
- Image digest present → skip load, re-apply missing tags only.
- Running containers not being replaced → never touched.

**Partial failure: fail-fast, honest summary, no rollback-of-the-rollback.**
First hard failure stops: "N of M steps completed, failed at: <step>: <err>",
exit non-zero; applied steps stay. Re-running the same command is the recovery
(steps idempotent: image loads skip via restore tag, clear+copy is idempotent,
remove-then-create re-succeeds). Non-fatal issues (missing bind source,
network drift, create warnings, config-only network) collect in a `warn()`
list exactly like `Capturer.warn` (capture.go:145), printed at the end.

## 5. New / changed files, in dependency order

1. `internal/dockerapi/restore.go` (new) — wrappers above; the inverse of
   stream.go, one file per concern.
2. `internal/dockerapi/restore_test.go` (new) — the two pure mappers only.
3. `internal/rollback/plan.go` (new) — pure planning, no Docker import:

```go
type StepKind int // StepRemoveContainer, StepCreateNetwork, StepLoadImage,
                  // StepRestoreVolume, StepCreateContainer, StepStartContainer
type Step struct {
    Kind StepKind
    Name string   // entity name / image ref
    Why  string   // "name conflict", "missing", "recorded running"…
    ContainerID string          // remove step: live container id
    InspectJSON json.RawMessage // network/container steps
    ObjectHash  string          // image/volume steps (CAS hash)
    Refs        []string        // image re-tags
    Vol         model.VolumeRecord
    Running     bool            // container step → follow-up start
}
func (s Step) String() string   // dry-run line, e.g. "stop+remove container demo-web (name conflict)"

type LiveState struct {
    ContainerIDs, VolumeNames, NetworkNames, ImageDigests, ImageTags map[string]bool
    RunningNames map[string]bool
}
func FetchLiveState(ctx context.Context, cli *dockerapi.Client) (*LiveState, error)

type Scope struct { All bool; Containers, Volumes, Images, Networks []string }
func ParseScopeArgs(m *model.Manifest, all bool, containers, volumes, images, networks []string) (Scope, error) // unknown name → error listing what the snapshot has

type Plan struct { SnapshotID string; Steps []Step; Warnings []string }
func BuildPlan(m *model.Manifest, live *LiveState, scope Scope) (steps []Step, warnings []string)
func (p *Plan) WriteTo(w io.Writer) // --dry-run / pre-confirm rendering
```

Scope semantics: `--all` selects everything; each flag's names select those
entities **plus implicit dependencies** — selecting containers pulls in their
`ImageObject`s, mounted named volumes, and networks.

4. `internal/rollback/apply.go` (new):

```go
type Executor struct { Cli *dockerapi.Client; St *store.Store; Out io.Writer }
func (e *Executor) Apply(ctx context.Context, p *Plan) error
// sequential; prints each step as it completes (TUI captures stdout);
// maintains map[objectHash]imageID and <name>-restored-from-<snap> tags;
// fail-fast; summary error lists completed vs failed counts.
```

5. `internal/rollback/plan_test.go` (new) — §6 fixtures.
6. `internal/cli/rollback.go` (new); `internal/cli/stubs.go` (modified) —
   delete the rollbackCmd stub + its init line (export/import stubs stay).
   `rollbackOpts` struct: `all, containers, volumes, images, networks
   []string; dryRun, yes, keepCurrent bool`. RunE: `dockerapi.New()` + `Ping`
   → `GetSnapshot` (prefix-resolve like `show`) → refuse broken via
   `missingObjects` → resolve scope (flags; none → per-kind confirm()-built
   prompts) → `FetchLiveState` → `BuildPlan` → `--dry-run` print + exit 0 →
   unless `--keep-current`, run `snapshot.Capturer{Cli, St, Opt{Message:
   "pre-rollback checkpoint before " + m.ID}}` (abort on failure) → plan
   summary + `confirm(...)` unless `--yes` (shared forceYes var) →
   `Executor.Apply` → print warnings, propagate non-zero on failure.
7. `internal/cli/interactive.go` — rollback menu row desc → "restore engine
   state from a snapshot"; `guidedRollback` (pickSnapshot → per-kind
   promptBool → argv; no --yes, the command's own confirm runs, like
   guidedDelete); `resetCommandFlags` zeroes `rollbackOpts`.
8. `internal/cli/tui.go` — `confirmDestructive`: add `name == "rollback"`
   (every rollback is destructive). `runMenuAction`: `case "rollback"`:
   askPick → askBool dry-run preview → run `rollback <id> --dry-run` → askBool
   apply → `t.execTUI("rollback", "--yes", id)`. `snapArgSlots` already has
   `"rollback": {0}` — no change. `printMan` self-updates from the tree —
   verified, no change.
9. Docs + version: README (build-out row → rollback working; quick-start gains
   a `--dry-run` example), DESIGN.md §6 Step 3 → done + note deviations,
   HANDOFF.md §4 → done + deviations. Makefile VERSION → **0.5.0**.

**Store changes: none.** `GetSnapshot`, `OpenObject`, existing inventory calls
cover everything; `AdoptObject` is Step 4 only.

## 6. Test plan

Unit (no engine, `go test ./...`):
- `restore_test.go`: `networkCreateOptions` on a fixture matching the
  live-store shape (`"IPAM":{"Driver":"default","Config":[{"Subnet":"172.18.0.0/16",...}]}`) —
  asserts Id/Created/Scope/Containers dropped, Driver/Options/Labels/IPAM
  carried; `containerCreateConfig` on a demo-web-shaped fixture — Hostname
  dropped, Image replaced, Env/Cmd/Labels/Binds/PortBindings/RestartPolicy
  carried, EndpointsConfig built with Aliases and IPAMConfig == nil.
- `plan_test.go` over fixture manifest + hand-built LiveState: (a) step-kind
  sequence remove→networks→images→volumes→creates→starts; (b) scope filtering;
  (c) implicit dependencies; (d) conflict listing (live demo-web → remove step
  Why "name conflict"); (e) WriteTo contains entity names/actions; (f)
  ParseScopeArgs rejects unknown names listing available ones.

Live demo engine (manual; report real output):
1. Take a FRESH snapshot first (`rollback baseline`) — the user's existing 8
   snapshots are all deliberately broken for doctor testing; rollback must
   also be verified to HARD-REFUSE on one of them.
2. Mutate: `docker run --rm -v demo-data:/data busybox sh -c 'echo mutated > /data/notes.txt'`;
   `docker rm demo-web`.
3. `rollback <fresh-snap> --all --dry-run` — plan lists volume refill, image
   loads/skips, container creates, starts only for recorded-running.
4. `rollback <fresh-snap> --all` — confirm; verify `notes.txt` restored,
   demo-web recreated (running iff recorded), new pre-rollback checkpoint in
   `log`; re-run same rollback → no-op/skips (idempotence).
5. Granular `--volumes demo-data --yes` — containers/networks untouched.
6. `--keep-current` — no new snapshot.
7. TUI: menu rollback flow; `:`-mode `rollback <Tab>` completion + y/n
   destructive dialog; no raw-mode hang. Line-menu guided flow via piped stdin.

## 7. Locked decisions (were open questions)

1. **Volumes: clear-then-copy** — after rollback the volume matches the
   snapshot exactly (post-snapshot files deleted). Accepted risk: a failed
   copy after the clear leaves the volume empty until re-run.
2. **Flag surface: keep the stub/HANDOFF surface** (`--all` + per-entity), no `--only`.
3. **Pre-rollback checkpoint: default ON**; `--keep-current` opts out.
