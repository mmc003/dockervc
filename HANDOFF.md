# HANDOFF — implementing rollback (Step 3) and export/import (Step 4)

**Audience:** a coding agent (or human) with **no prior context in this repo**.
DESIGN.md explains the *intent*; this file is **ground truth for what exists**.
Where the two disagree, this file and the code win.

You are implementing two of the five build-out steps. Everything else —
snapshot capture, content-addressed storage, history/diff/status, a full-screen
TUI, packaging — already works and must keep working.

---

## 0. Ground rules

- Go module `dockervc`, Go ≥ 1.22. **No CGO** (SQLite is `modernc.org/sqlite`;
  the binary must stay static). Docker access is via the official Go SDK
  (`github.com/docker/docker/client`), not shelling out — except `docker
  save`/`load`-style streams, which the SDK handles via API endpoints in
  `internal/dockerapi`.
- Build & verify before you claim done:

  ```console
  go vet ./... && go test ./...
  go build -o /tmp/dockervc .        # quick check (version shows "dev")
  make dist                          # all 6 platforms; requires sudo-free local build
  ```

- `make dist` stamps the version from `Makefile` (`VERSION ?=`) into the
  binary via ldflags. **Bump `VERSION` for every change that ships** (patch
  for fixes, minor for features). Package names embed it:
  `dist/dockervc-<version>-<os>-<arch>.tar.gz`.
- Tests: unit tests live next to the code (`*_test.go`). Anything you add,
  add a test for. The existing suites must stay green.
- A demo Docker environment exists for live verification (§6). Use it instead
  of creating throwaway containers when possible.
- No new network calls, no telemetry, no cloud anything. Ever.

## 1. Repo map

```
main.go                      entrypoint → cli.Execute()
internal/cli/                cobra command tree + full-screen TUI
  root.go                    rootCmd, --store flag, store open/close lifecycle, confirm()
  init.go, snapshot.go       init / snapshot / log / show / delete commands
  status.go                  status + diff commands (--files detail lives here)
  maintenance.go             prune / config + command registration for status/diff;
                             `verify` lives here too as a deprecated alias of doctor --deep
  doctor.go                  doctor: structural + --deep health check, --repair flow,
                             BrokenSuffix() powering the ✗ BROKEN markers in `log`
  stubs.go                   rollback / export / import — STUBS YOU WILL REPLACE
  interactive.go             `dockervc cli`/`man`: menu table, guided flows, runArgs(),
                             man page generator, resetCommandFlags()
  tui.go                     full-screen alternate-screen TUI (raw mode, key decode,
                             dialogs, Tab completion, output panes)
internal/dockerapi/          thin client over the Docker SDK (single *client.Client)
  stream.go                  SaveImage / CommitContainer / VolumeTarStream (helper container)
internal/model/manifest.go   Manifest + per-entity records; ObjectHashes() = GC ref set
internal/snapshot/           capture (capture.go), drift/diff (status.go),
                             volume file indexes (volindex.go), HumanBytes
internal/store/              cas.go (PutBlob/OpenObject), db.go (SQLite), store.go (lock, GC)
packaging/install.sh         copies binary + docs into /usr/local (run via sudo)
Makefile                     cross-compile, dist packaging (VERSION is bumped here)
```

## 2. Ground truth: data model, schema, CAS

### Manifest (internal/model/manifest.go) — one JSON blob per snapshot

Key fields: `ID` (`snap-YYYYMMDD-HHMMSS-xxxx`), `CreatedAt`, `Message`,
`DockerVersion`, `EngineID`, `Consistent` (true when taken with `--stop`),
`Containers []ContainerRecord`, `Images []ImageRecord`, `Volumes
[]VolumeRecord`, `Networks []NetworkRecord`, `BindMounts []BindMountRecord`
(opt-in), `TotalSize`, `Stats`.

| Record | Fields you will need for restore |
|---|---|
| `ContainerRecord` | `Name`, `ID`, `InspectJSON` (full `docker inspect` at capture time), `ImageObject` (CAS hash of the committed-filesystem image tar), `LayerHash` (stable fingerprint of the layer *content* — commit churn-proof; what `diff` compares and what dedups container images across snapshots via the `containerfs:<hash>` image_key), `Running`, `Size` |
| `ImageRecord` | `Refs` (repo tags), `Digest` (repo digest or image ID), `Object`, `Size` |
| `VolumeRecord` | `Name`, `Driver`, `Options`, `Object` (CAS hash of volume tar), `IndexObject` (file index hash), `Files`, `Size` |
| `NetworkRecord` | `Name`, `InspectJSON` |

`Manifest.ObjectHashes()` returns every referenced object hash (images,
container ImageObjects, volume Objects **and IndexObjects**, bindmounts).
`refs` is written from this in `InsertSnapshot` — any object type you add to
the manifest must be added there too, or `prune` will delete it.

### SQLite schema (internal/store/db.go) — actual, not sketched

```sql
snapshots(id TEXT PK, created_at INT, message TEXT,
          manifest TEXT NOT NULL, manifest_hash TEXT NOT NULL)
objects(hash TEXT PK, kind TEXT, size INT, created_at, image_key TEXT)
refs(snapshot_id, hash, PRIMARY KEY(snapshot_id, hash))   -- FKs cascade
config(key TEXT PK, value TEXT)
```

`kind` ∈ `image | volume | volindex | bindmount`. `image_key` holds the docker
digest for image dedup lookups (`ObjectByImageKey`), NULL otherwise.

### CAS semantics (internal/store/cas.go) — read this before touching blobs

- `PutBlob(kind, imageKey, r)` **zstd-compresses** the stream, hashes the
  **compressed** bytes (SHA-256), writes atomically (`tmp/` → rename), dedups
  by existing hash, inserts the `objects` row. Returns hash + size + `New`.
- `OpenObject(hash)` returns a **decompressing** reader — callers see the
  original tar bytes.
- `ObjectPath(hash)` = `objects/<hash[:2]>/<hash>`.
- **Consequence for import:** a stored object's hash equals the SHA-256 of its
  on-disk (compressed) bytes. Import must place bytes into the store
  *verbatim* and register the same hash — do **not** round-trip through
  `PutBlob` (it would re-compress and change the hash). You will add a new
  store method for this (§5).

### Volume file indexes (internal/snapshot/volindex.go)

Each volume snapshot also stores a tiny JSON file index (path → type/size/
mode/mtime/content-hash). `LoadFileIndex(open, rec)` + `DiffIndexes(...)` are
the read APIs; `BlobOpener = func(hash) (io.ReadCloser, error)` is how the
snapshot package reads blobs without importing store. Indexes are optional
metadata — restore must not depend on them.

## 3. Existing API surface (and the gaps you will fill)

`dockerapi.Client` has: `Ping`, `EngineInfo`, `Version`, `ListContainers`,
`InspectContainer`, `StopContainer`/`StartContainer`/`IsRunning`,
`CommitContainer`, `SaveImage`, `RemoveImage`, `ListVolumes`, `VolumeTarStream`,
`ListNetworks`, `InspectNetwork`, `ListImages`, `pickHelperImage` (prefers an
already-present small image like busybox/alpine).

**Missing for Step 3** (add to `internal/dockerapi`, same thin-wrapper style):

- `ImageLoad(ctx, r io.Reader) error` — `client.ImageLoad` with `Quiet: true`;
  stream from `store.OpenObject`.
- `NetworkCreateFromInspect(ctx, raw json.RawMessage) error` — parse stored
  inspect, create with Name/Driver/Options/Labels/IPAM (§4).
- `VolumeCreate(ctx, name, driver string, opts map[string]string) error` —
  `client.VolumeCreate`; treat "already exists" as success.
- `VolumeRestore(ctx, volumeName string, tarStream io.Reader) error` — the
  reverse of `VolumeTarStream`: helper container mounting the volume **rw** at
  `/dst` (idle command, e.g. `sleep 30`), then `client.CopyToContainer(ctx,
  id, "/dst", tarStream, opts)` streaming the decompressed object, then remove
  the helper. Note the stored tar's entries are rooted at the volume root.
- `ContainerCreateFromInspect(ctx, inspectJSON json.RawMessage, imageName
  string) (id string, err error)` + `ContainerRemove(ctx, id, force bool)` —
  see the field mapping in §4.

`store.Store` has: `PutBlob`, `OpenObject`, `ObjectPath`, `InsertSnapshot`,
`GetSnapshot(idOrPrefix)`, `ListSnapshots`, `LatestSnapshot`,
`DeleteSnapshot`, `ObjectByImageKey`, `UnreferencedObjects`, `DeleteObject`,
`AllObjects`, `QuickCheck`, `ConfigGet/Set/All`, `CleanEmptyShards`, `Close`.
**Missing for Step 4:** `AdoptObject` (§5).

`snapshot` package: `Capturer` (what `dockervc snapshot` runs),
`ComputeDrift`, `DiffManifests`, `HumanBytes`, the volindex API.

## 4. Step 3 — rollback (`internal/rollback`, replace the stub in stubs.go)

The stub command already promises this surface — keep it exactly:

```
dockervc rollback <snap> [--all] [--containers a,b] [--volumes x,y]
                         [--images i1,i2] [--networks n1] [--dry-run]
                         [--yes] [--keep-current]
```

No scope flags → interactive scope prompt (which entities?). `--dry-run`
prints the plan and exits 0 without touching Docker or the store.

### Execution model

1. **Resolve** the snapshot (prefix ok, same as `show`), load manifest,
   select entities by scope flags; entities not selected are never touched.
2. **Pre-rollback checkpoint**: unless `--keep-current`, run a normal
   `snapshot.Capturer` run (`-m "pre-rollback checkpoint"`). This makes every
   rollback itself reversible.
3. **Build a typed plan** (a slice of steps with a String() rendering):
   stop/remove conflicting containers → create networks → load images →
   create/fill volumes → create containers → start the ones recorded running.
4. **Confirm** with the shared `confirm()` in root.go unless `--yes` (accepts
   `\r` — it runs under the TUI's raw mode).
5. **Apply in order**, printing each step as it completes (TUI captures
   stdout). On failure: stop, print what succeeded and what didn't, exit
   non-zero. Steps are idempotent-ish (image load dedups, volume copy
   overwrites, container create after remove), so a re-run after partial
   failure is the documented recovery.

### Per-entity mechanics

- **Networks** (first — containers attach to them): parse stored
  `InspectJSON`. Load-bearing fields: `Name`, `Driver`, `Options`, `Labels`,
  `IPAM` (`Driver`, `Config[].Subnet`, `Config[].Gateway`). **Drop**
  engine-generated ones (`Id`, `Created`, `Containers`, `Scope`, `Internal`
  only if true, and per-container IPAM assignments). If a network with the
  name already exists: reuse it, don't recreate.
- **Images**: for every selected `ImageRecord` AND every selected container's
  `ImageObject`: `OpenObject` → `ImageLoad` (streaming, don't materialize).
  After load, re-tag by `Refs` when the snapshot recorded tags (`ImageTag`).
  Keep a hash→loaded-image-ID map; containers reference their committed image
  through it.
- **Volumes**: `VolumeCreate` if missing (name, driver, options). Then
  `OpenObject` (decompressed tar) → `VolumeRestore` (§3). Only volumes
  selected — or selected containers' mounted volumes — are restored.
- **Containers** (last): for each `ContainerRecord`, derive create config
  from `InspectJSON`:

  | Use (from inspect) | Notes |
  |---|---|
  | `Config.{Env, Cmd, Entrypoint, WorkingDir, User, Labels, StopSignal, Tty, OpenStdin}` | copy verbatim; **drop** `Hostname`, `Image` (set below) |
  | `HostConfig.{Binds, PortBindings, RestartPolicy, Privileged, ReadonlyRootfs, ExtraHosts, LogConfig, CapAdd/CapDrop, Memory/Cpu*}` | volume binds keep working — volumes are restored under the **same names**; bind-mount sources that don't exist on this host → plan warning, keep the bind |
  | `NetworkMode` + endpoints from `NetworkSettings.Networks` → `NetworkingConfig.EndpointsConfig` with `Aliases`; drop recorded `IPAMConfig` IPs (engine reassigns) |
  | `Name` | from the record; compose labels (`com.docker.compose.*`) come along in `Config.Labels` |

  Conflict policy: a live container with the same name is stopped and removed
  (this is in the plan and shown by `--dry-run`). Create with the loaded
  committed-image ID as the image. If `Running`, `StartContainer`.
- **Bind mounts** are only restored where the host path exists and is
  writable; otherwise warn and skip (they're opt-in capture, host-specific).
- **Run-state**: only containers recorded `Running: true` are started.

### Tests

- Unit: plan builder over a fixture manifest — ordering, scope filtering,
  conflict listing, dry-run output. No Docker needed.
- Live (demo env, §6): mutate a volume file + delete a container, rollback
  the pre-change snapshot, assert the file's content is back and the
  container exists and runs.

## 5. Step 4 — export/import (`internal/portable`, replace stubs)

`dockervc export <snap> -o state.dvca [--latest]` / `dockervc import
state.dvca [--apply]`. `--apply` = import, then run the rollback machinery
against the imported snapshot (with its own confirmation). `--split` is
explicitly **not** implemented — reject the flag with a "deferred" message.

### Archive layout (`.dvca`) — outer tar is UNCOMPRESSED

Blobs inside are already zstd; compressing the outer layer wastes CPU for ~0%
gain. (This deviates from the original DESIGN sketch deliberately.)

```
manifest.json          exact model.Manifest JSON (same bytes as snapshots.manifest)
checksums.sha256       one "sha256 <hash>  <path>" line per entry:
                       manifest.json itself + every objects/<hash>
objects/<hash>         CAS blob bytes VERBATIM (compressed), filename = hash
```

Write with `archive/tar` streaming from `ObjectPath` files; set tar sizes
from the objects table (or `os.Stat`). Progress print every N objects.

### Import algorithm (order matters)

1. Stream-read the outer tar. Reject unexpected entries.
2. For each `objects/<hash>`: **hash the bytes while streaming to a tmp file**
   (sha256 of the file bytes as-is); mismatch with the filename → abort with
   the offending entry named. Also verify against `checksums.sha256`.
3. New store method `AdoptObject(hash, kind string, size int64, src io.Reader)
   error`: write bytes verbatim under `ObjectPath(hash)` via `tmp/` + rename
   (no compression, no re-hash), `INSERT OR IGNORE` the `objects` row (size =
   stored bytes). **Never** `PutBlob` (§2).
4. Decode `manifest.json` into `model.Manifest`; verify `manifest_hash` by
   re-marshaling canonically (same as `db.go` does) — catches truncation.
5. **ID collision**: if `snapshots.id` already exists with the same
   `manifest_hash` → print "already imported", success, no-op. Different hash
   → hard error (suggest deleting the local one or using `--store` elsewhere).
6. `InsertSnapshot(m)` → refs registered → `log`/`show`/`rollback` work.

`kind` for adopted rows: images + container ImageObjects → `image`; volume
Objects → `volume`; IndexObjects → `volindex`; bindmounts → `bindmount`.

### Tests

- Round-trip: build a tiny store (two snapshots, one index) → export →
  import into a fresh `--store` tmpdir → identical `show` output, identical
  object hashes.
- Corruption: flip one byte in an objects entry → import fails naming the
  entry, store left clean (no partial snapshot row).
- `--apply` on the demo env = import then rollback (integration).

## 6. CLI & TUI integration (both steps — the checklist that avoids regressions)

The TUI wraps the real command tree; new commands must be wired or they will
be invisible there (or worse, deadlock raw mode):

- [ ] `stubs.go`: delete the replaced stubs; real commands keep
      `Annotations: map[string]string{needsStore: "true"}`.
- [ ] `interactive.go` `menuActions`: update rollback/export/import rows
      (desc no longer "planned"); add guided flows if useful (rollback: pick
      snapshot + scope prompt).
- [ ] `interactive.go` `snapArgSlots`: `"rollback"` and `"export"` are
      already registered — keep them working (Tab-completes snapshot args).
- [ ] `interactive.go` `resetCommandFlags`: reset any new package-level flag
      vars, or flags leak between TUI executions (bit us before).
- [ ] `tui.go` `confirmDestructive`: route rollback (and `import --apply`)
      through the TUI y/n dialog + implicit `--yes` — their console
      `confirm()` would be invisible in raw mode and hang the TUI.
- [ ] `man` updates itself from `LocalFlags()` — just verify.
- [ ] Destructive console path: always honor `--yes`, default to
      `confirm()`.
- [ ] Long operations: print progress lines to stdout (TUI captures them);
      never read stdin outside `confirm()`/guided flows.

## 7. Demo environment (live verification)

Running on this machine (recreate cheaply if wiped): containers `demo-web`
(nginx:alpine, serves `/usr/share/nginx/html/demo.txt` on demo-net),
`demo-worker` (busybox, appends ticks to volume `demo-data:/data`),
`demo-sidecar` (busybox sleep); volumes `demo-data` (has `log.txt`,
`notes.txt`, …), `demo-extra-data`, `demo-cache`, `demo-status-demo`; network
`demo-net`; images nginx:alpine, busybox, alpine, hello-world.

- Mutate a volume without starting anything:
  `docker run --rm -v demo-data:/data busybox sh -c 'echo x > /data/f'`
- Verify restored content:
  `docker run --rm -v demo-data:/data busybox cat /data/notes.txt`
- Store: `~/.dockervc`. Reinstall after `make dist`:
  `cd ~/docker-version-control && sudo ./dist/darwin-arm64/install.sh`
- Report results with real command output, not summaries.

## 8. Definition of done

- [ ] `go vet ./...` and `go test ./...` green, including new tests
- [ ] `make dist` succeeds; `VERSION` bumped; user reinstall verified
- [ ] Rollback: `--dry-run` plan correct on demo env; full rollback restores
      a deleted container + a mutated volume; granular scope touches nothing
      else; `--keep-current` skips the checkpoint
- [ ] Export/import: round-trip into a fresh store byte-identical; corrupted
      archive rejected cleanly; `--apply` chains into rollback
- [ ] TUI: all new commands usable via menu/`:`/Tab-completion; destructive
      ops confirmed via dialog; no hangs under raw mode
- [ ] README.md + DESIGN.md status rows updated; HANDOFF.md marked done
