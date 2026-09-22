# Implementation Plan and Recovery Checkpoint — Progress/ETA and TUI

Last updated: 2026-09-22

This file records the completed v0.7.0 deep-status/progress work and the
remaining follow-ups. It is intentionally detailed enough to resume after an
interrupted session.

## Working-tree context

- Branch: `windows-port`
- Latest pushed implementation commit: `4d449a9`
  (`feat: inspect exported archives`).
- Commit `324a4dd` contains stages 1–5, their tests and documentation, and the
  stage 7 TUI implementation plan. It explicitly records that interactive
  CLI/TUI ETA is not operational yet.
- The preceding snapshot-storage-accounting commit is `1181aab`
  (`feat: show snapshot storage sharing`).
- Preserve all existing snapshot-storage-accounting changes.

## Completed: snapshot storage accounting

- `show <snapshot>` reports capture counts plus referenced, exclusive, and
  shared object-store bytes/counts.
- Store query deduplicates repeated manifest references and calculates current
  cross-snapshot sharing.
- Version was raised from 0.6.0 to 0.7.0 and documented.
- Tests passed and commit `1181aab` was pushed.

## Completed and pushed: deep live-volume status

Implemented in commit `324a4dd`:

- `status --deep`
- `status --deep --volumes name,...`
- `diff <snapshot> --files <volume>` against the live engine
- Read-only volume streaming with no new snapshot or CAS object
- File created/modified/deleted summaries
- `?` unavailable results for legacy indexes and failed scans
- Continue scanning later volumes after one failure, then return nonzero
- Simple `scanning volume ...` messages
- Helper-container exit-code checking
- Documentation and v0.7.0 changelog updates
- Focused tests in `internal/cli/status_test.go` and
  `internal/snapshot/livevolume_test.go`

The full suite passed after these changes:

```text
go vet ./...
go test ./...
```

## Completed and pushed: reusable progress and ETA system

### Design rules

- Core packages emit operation-neutral progress events.
- CLI code owns formatting and terminal behavior.
- Progress is written to stderr; final command results remain on stdout.
- Exact totals display `ETA`.
- Estimated totals display `~ETA`.
- Unknown totals show bytes/rate/elapsed without an ETA.
- Initial unstable samples display `ETA calculating…`.
- Progress-rendering failures must never fail the underlying operation.
- No goroutine or timer is required for ordinary byte updates; stream reads
  drive events and the renderer throttles output.
- Interactive terminals update one line at most four times per second.
- Redirected output prints phase changes, completion, and periodic updates
  without ANSI sequences.
- The full-screen TUI currently keeps its existing `running: ...` display.
  The approved follow-up plan for live TUI progress is recorded in stage 7.

### Committed code checkpoint

The following core files were added or modified in commit `324a4dd`:

- `internal/progress/progress.go`
  - `Event`, `Reporter`, `ReporterFunc`, `Nop`
  - thread-safe `Meter`
  - `CountingReader`
- `internal/cli/progress.go`
  - CLI renderer
  - EWMA throughput
  - elapsed/ETA formatting
  - TTY and redirected-output modes
- `internal/cli/root.go`
  - new persistent `--no-progress` flag
- `internal/cli/interactive.go`
  - resets `noProgress` between in-process TUI commands

The core, renderer, and deterministic tests exist. Export, restore, snapshot,
and deep status are connected to the reporter. Before commit `324a4dd` was
pushed, final `go vet ./...` and `go test ./...` both passed. A version-stamped
Windows build reports `dockervc 0.7.0 (windows/amd64)` and exposes the new
global `--no-progress` flag plus the deep-status flags.

## Implementation record and remaining work

### 1. Progress core tests — complete

Add deterministic tests for:

- Meter start/phase/update/finish events
- Exact, estimated, and unknown totals
- Duration formatting
- EWMA rate behavior
- `ETA` versus `~ETA`
- `ETA calculating…` before two seconds
- No ANSI output for redirected writers
- TTY line termination on success and failure
- Nil/no-op reporter behavior
- `CountingReader` byte counts

### 2. Export — exact ETA — complete

Files:

- `internal/portable/write.go`
- `internal/cli/export.go`
- export tests

Implementation:

- Keep deterministic archive bytes unchanged.
- Export already stats all unique referenced objects; sum their exact sizes.
- Add a `progress.Reporter` field to `portable.Exporter` while temporarily
  preserving the existing per-object callback for compatibility/tests.
- Count source object bytes as they are copied into the outer tar.
- Emit phases: validating, writing objects, finalizing.
- Use exact totals during object writing.
- CLI supplies `commandProgress(cmd.ErrOrStderr())`.
- Replace the current occasional stdout progress messages.
- Final `Exported ...` summary remains on stdout.

### 3. Restore/rollback — exact stored-byte ETA — complete

Files:

- `internal/store/cas.go`
- `internal/rollback/apply.go`
- `internal/cli/rollback.go`
- `internal/cli/import.go` for `--apply`
- rollback/store tests

Implementation:

- Add an object-open path that counts compressed bytes below the zstd
  decoder: file -> counting reader -> zstd decoder -> Docker.
- Preserve existing `OpenObject` behavior for callers that do not need
  progress.
- Sum exact stored sizes for image/volume restore steps.
- Add `Progress progress.Reporter` to `rollback.Executor`.
- Emit current plan step and exact aggregate byte progress.
- Non-byte Docker steps show item count and elapsed time without byte ETA.
- Show a finalizing/applying phase after bytes are supplied when Docker is
  still processing.
- A normal rollback has parent phases: safety snapshot, then restore.
- `import --apply` uses the same reporter path.

### 4. Snapshot — estimated ETA — complete for prior-entity baselines

Files:

- `internal/store/cas.go`
- `internal/snapshot/capture.go`
- `internal/cli/snapshot.go`
- snapshot/store tests

Implementation:

- Add progress reporting to `PutBlob` without breaking existing callers
  (prefer a new options/progress-aware method with `PutBlob` delegating).
- Count raw input and compressed output; ETA uses compressed bytes when
  comparing against stored snapshot sizes.
- `snapshot.Capturer` accepts a reporter.
- The current implementation estimates from the same entity in the latest
  snapshot. New entities intentionally use an unknown total. Docker-size and
  median fallbacks remain optional future refinements.
- Never pre-scan volumes merely to obtain totals.
- Mark all snapshot ETAs estimated (`~ETA`).
- Reused/deduplicated entities reduce remaining estimated work immediately.
- Rollback safety snapshots reuse this integration.

### 5. Deep status integration — complete

- Replace the temporary `scanning volume ...` line with progress events.
- Count bytes consumed by `IndexLiveVolume`.
- Estimate total from the stored file index's logical sizes plus tar overhead.
- Mark the ETA estimated because the live volume may differ substantially.
- Keep `--no-progress` behavior consistent.

### 6. Additional exact-total integrations — deferred

- `import`: archive/object bytes are known.
- `doctor --deep`: all stored object sizes are known.
- `import --apply`: named import, checkpoint, and restore phases.
- These are valuable follow-ups but should not block the requested snapshot,
  restore, and export progress.

### 7. TUI live progress — planned, not yet implemented

Goal:

- Show the same live phase, item, byte progress, throughput, elapsed time,
  and exact/estimated ETA in the full-screen `dockervc cli` interface that
  direct commands already expose.
- Cover snapshot, rollback/restore, export, deep status, and the restore
  portion of `import --apply` through their existing `progress.Reporter`
  integrations.
- Preserve the complete command output and existing result pane after the
  command finishes.

Current constraint:

- `runTUI` blocks in `readKey`, and `doExec` then runs the Cobra command
  synchronously on that same goroutine.
- `captureOutput` drains stdout/stderr into a pipe but returns the captured
  text only after the command exits.
- The current `running: ...` flash is therefore the only frame the TUI can
  draw during a long command.

#### 7.1 Extract reusable progress presentation state

Files:

- `internal/cli/progress.go`
- `internal/cli/progress_test.go`

Implementation:

- Separate throughput smoothing and event-to-display formatting from the
  terminal writer. Keep one stateful tracker responsible for:
  - current operation, phase, and item;
  - item and byte totals;
  - EWMA transfer rate;
  - elapsed time;
  - exact `ETA`, estimated `~ETA`, `ETA calculating…`, or no ETA when the
    total is unknown.
- Keep the existing console renderer as one consumer of that tracker so TUI
  support does not create a second ETA algorithm.
- Give the TUI a structured display model as well as a compact text form;
  avoid parsing the console progress line or ANSI output.
- Allow a clock to be supplied in tests so elapsed/ETA behavior remains
  deterministic.

#### 7.2 Allow commands to receive a TUI progress reporter

Files:

- `internal/cli/progress.go`
- `internal/cli/interactive.go`
- CLI call sites in `snapshot.go`, `rollback.go`, `export.go`, `import.go`,
  and `status.go`

Implementation:

- Add a command-context progress reporter override.
- Replace raw `commandProgress(cmd.ErrOrStderr())` selection with a helper
  that first checks the command context for an injected reporter and falls
  back to the existing stderr renderer for ordinary direct commands.
- Continue honoring `--no-progress`; it must suppress both console and TUI
  progress views.
- Add a context-aware `runArgs` variant for the TUI worker while preserving
  the existing helper for tests and the line-based menu.
- Do not route TUI progress through stdout/stderr. Those streams remain
  dedicated to the final captured command log.

#### 7.3 Convert the TUI to a single-owner event loop

Files:

- `internal/cli/tui.go`
- focused TUI tests

Implementation:

- Keep all `tui` state mutation and all calls to `render` on the main TUI
  goroutine.
- Move blocking keyboard reads into a key-reader goroutine that sends typed
  key events to the main loop.
- Run one command at a time in a worker goroutine. The worker sends:
  - progress events during execution;
  - one completion event containing captured stdout/stderr and success or
    failure state.
- Use a bounded/latest-value progress channel so slow terminal rendering can
  coalesce intermediate byte updates and can never stall Docker I/O or CAS
  writes.
- While a job is active, prevent menu actions or `:` commands from starting a
  second Cobra execution. Cobra flags, package globals, and the shared store
  are not safe for concurrent commands.
- Add a one-second UI tick while a job is active so elapsed time continues to
  advance even when Docker emits no new byte event. Byte events may trigger
  faster redraws, capped at the existing four-updates-per-second target.
- On completion, stop the tick, clear active progress state, populate the
  existing output pane, refresh snapshots/store totals, and render once.

Cancellation boundary:

- Do not let Ctrl-C exit the process while a background command continues.
- Give the worker a cancellable command context. During an active job,
  Ctrl-C requests cancellation and the TUI waits for the completion event
  before returning to the menu or exiting.
- Esc should not silently cancel a snapshot/restore; show an explicit hint
  explaining the available cancellation key.
- Preserve cleanup already provided by deferred meter completion, store
  closure, helper-container cleanup, and temporary-file removal.

#### 7.4 Add an active-operation view

Files:

- `internal/cli/tui.go`
- `internal/cli/tui_test.go` or a focused `tui_progress_test.go`

Display, adjusted to terminal width:

```text
 snapshot in progress
 snapshotting volume  openclaw_dev_home
 [██████████████──────────────] 48%  1.02 GiB / ~2.12 GiB
 84.3 MiB/s  elapsed 00:12  ~ETA 00:13
```

Rules:

- Exact totals use a normal percentage/bar and `ETA`.
- Estimated totals visibly use `~` on the total/ETA.
- Unknown totals use an indeterminate marker plus bytes/rate/elapsed and omit
  ETA.
- `ETA calculating…` remains visible until the rate is stable enough.
- Non-byte phases show item counts and elapsed time.
- Nested operations naturally replace the active phase: for example,
  rollback's safety snapshot is followed by restore progress.
- The footer explains that the job is active and that Ctrl-C requests
  cancellation. No normal menu/input cursor is shown as actionable.
- A failed/cancelled job still opens the output pane with its diagnostics.

#### 7.5 Testing and verification

Unit tests:

- Progress tracker produces identical exact, estimated, calculating, and
  unknown-total semantics for console and TUI consumers.
- TUI reporter delivery is non-blocking and coalesces safely when the event
  channel is full.
- Active-operation rendering works at normal and narrow terminal widths.
- A UI tick advances elapsed time without new byte events.
- Completion transitions from active view to output pane and preserves all
  captured command output.
- A second command cannot start while one is active.
- Ctrl-C cancels through context and waits for worker completion.
- `--no-progress` leaves the simple running state without live metrics.

Integration checks:

- Run snapshot, export, rollback, `status --deep`, and `import --apply` from
  both guided menu actions and `:` raw-command mode.
- Confirm the TUI remains responsive and redraws without corrupting the
  alternate screen.
- Confirm direct command progress output is unchanged.
- Confirm final output panes contain no ANSI progress-control sequences.
- Run `go test -race ./internal/cli` where supported, followed by the normal
  `go vet ./...` and `go test ./...` checks.

Documentation/version:

- Update README and DESIGN to state that both direct commands and the TUI
  show live progress.
- Keep version `0.7.0` while this remains part of the same unreleased feature
  batch; do not bump again solely for the TUI completion.

## Implemented locally: exported archive inspection

- `archive list [directory]` quickly lists `.dvca` files by reading only each
  leading manifest; it does not stream multi-gigabyte object payloads.
- `archive show <file.dvca>` displays snapshot metadata, archive/captured
  sizes, resource counts, volumes, images, and file-index availability without
  importing or mutating the store.
- `archive verify <file.dvca>` checks structure, required objects, manifest and
  object checksums, with exact progress for both structural and hashing passes.
- `archive files <file.dvca> <volume> [prefix]` authenticates and decodes the
  selected volume-index object and lists indexed paths without importing.
- The line menu and full-screen TUI expose a guided Archive flow; raw `:` mode
  completes archive paths.
- Focused portable and CLI tests cover fast inspection, full verification,
  tamper detection, object authentication, path filtering, and unsafe-prefix
  rejection. Final `go vet ./...` and `go test ./...` pass.
- Archive inspection was committed and pushed in `4d449a9`.

## Archive extraction roadmap

This roadmap follows the archive-inspection commands (`archive list`,
`archive show`, `archive verify`, and `archive files`) without changing the
existing progress and TUI stage numbering above. The labels start at E2
because archive inspection is the first archive feature. E2 and E3 are now
implemented locally through the existing `export` command; E4 and E5 remain
future work.

### E2. Extract whole volumes and images from local snapshots — implemented locally

Implementation checkpoint (2026-09-21):

- The approved UX was integrated into `export` rather than adding a separate
  `extract` command:

  ```text
  dockervc export <snapshot|latest> --volume <volume> [-o <volume.tar>]
  dockervc export <snapshot|latest> --image <ref-or-digest> [-o <image.tar>]
  ```
- `internal/artifact` provides reusable manifest selectors plus a streaming,
  checksum-verifying local-CAS exporter. Whole volume and image output is the
  original uncompressed tar stream; Docker is not required.
- Outputs are written to a sibling temporary file, synced, and renamed only
  after the complete compressed CAS object matches its SHA-256 name.
- Existing outputs require the command's standard `--yes` flag. Non-regular,
  symlink, and CAS-alias output paths are rejected.
- Exact stored-byte progress, elapsed time, and ETA use the existing reporter.
- With no `-o`, artifacts are organized below
  `<export.folder>/<snapshot-id>/volumes|images/` and atomically registered in
  `export-index.json`. Explicit `-o` outputs are not registered.
- `archive list` includes registered partial artifacts alongside `.dvca`
  archives. The line menu and full-screen TUI expose selective export flows.
- Stdout (`-o -`) was intentionally deferred: safe stdout output requires a
  verification pass before emitting bytes because corrupt output cannot be
  retracted.

Goal:

- Recover one complete volume or image from a snapshot in the local store
  without restoring the full snapshot and without requiring a running Docker
  engine.

Earlier standalone-command sketch (superseded by the integrated CLI above):

```text
dockervc extract volume <snapshot> <volume> -o <volume.tar>
dockervc extract image <snapshot> <image-ref-or-id> -o <image.tar>
```

Examples:

```powershell
dockervc extract volume latest openclaw_dev_home -o openclaw-home.tar
dockervc extract image snap-123 postgres:17 -o postgres-17.tar
docker load -i postgres-17.tar
```

Semantics and safety:

- Resolve snapshot IDs, unambiguous prefixes, and `latest` through the same
  resolver used by existing snapshot commands.
- Resolve resource names from the selected snapshot manifest. Ambiguous
  image references must fail and list the matching IDs/references.
- Stream the selected CAS object through decompression to the output; do not
  materialize the full object in memory or a temporary file.
- A whole-volume extraction emits the captured standard tar stream. An image
  extraction emits the original Docker-save tar stream, suitable for
  `docker load`.
- Refuse to overwrite an existing output unless the standard `--yes` flag
  is supplied. Write to a temporary sibling file and atomically rename it on
  success so cancellation or corruption cannot leave a plausible partial
  result at the requested path.
- Future: support `-o -` only with a verify-first/two-pass design; diagnostics
  and progress must remain on stderr.
- Verify the selected CAS object's digest while streaming. On mismatch,
  delete the temporary output and return a corruption error.
- Reject directory output paths, special files, and output paths that alias
  the underlying object-store file.
- Reuse the existing progress reporter with exact stored-byte totals and
  elapsed time/ETA. Honor global `--no-progress`.

Shared implementation:

- Introduce a read-only resource selector that accepts a snapshot manifest
  and returns a typed volume/image object reference plus user-facing identity.
- Introduce an object-source interface that can open an object stream and
  report its stored size and expected digest. Initially implement it for the
  local CAS; E4 will add a `.dvca` implementation.
- Keep extraction logic below Cobra so CLI, TUI, and tests can call the same
  service.

Tests:

- Whole-volume output is byte-for-byte the original uncompressed tar.
- Image output is a valid Docker-save tar and preserves its manifest entries.
- `latest`, full IDs, prefixes, names, and image references resolve correctly.
- Missing and ambiguous resources return actionable errors.
- Existing outputs are protected unless `--yes` is set.
- Digest failure and cancellation remove temporary outputs.
- Stdout extraction contains only payload bytes; progress remains on stderr.
- Large objects are streamed with bounded memory and emit exact progress.

### E3. Extract individual files and directories from volumes — implemented locally

Implementation checkpoint (2026-09-21):

- Integrated CLI:

  ```text
  dockervc export <snapshot|latest> --volume <volume> --path <path> [-o <selection.tar>]
  dockervc export <snapshot|latest> --volume <volume> --path <file> --raw [-o <file>]
  dockervc files <snapshot|latest> <volume> [prefix]
  ```
- The tar filter scans the stored volume stream once, uses component-aware
  matching, preserves selected tar headers/metadata, and never unpacks onto
  the host. It continues through the complete object so checksum verification
  covers all stored bytes.
- Paths are normalized as slash-separated, volume-relative names. Absolute,
  drive-qualified, NUL-containing, and `..` paths are rejected.
- `--raw` accepts exactly one regular-file entry and rejects directories,
  links, duplicates, and missing paths.
- Default filtered/raw outputs are organized below
  `selections/<volume>/` and `raw/<volume>/`, respectively, and recorded in
  the same export index.
- `dockervc files` browses the already captured local volume index without
  Docker. The guided line menu and TUI expose it. The full-screen selective
  export flow also reuses that index as a filterable hierarchical browser:
  Enter opens folders, “export this folder” selects the current subtree, and
  manual entry remains available for legacy snapshots and empty directories
  (directory entries are not stored in the index).
- Focused tests cover whole-object equality, subtree selection, raw files,
  unsafe/missing paths, CAS corruption, CLI flag validation, default layout,
  index registration, and partial-export discovery.

Goal:

- Extract a selected regular file or directory tree from a captured volume
  without restoring the volume or scanning unrelated snapshot resources.

Earlier standalone-command sketch (superseded by the integrated CLI above):

```text
dockervc extract volume <snapshot> <volume> --path <path> -o <selection.tar>
dockervc extract volume <snapshot> <volume> --path <file> --raw -o <file>
```

Recommended behavior:

- Tar is the default output for both files and directories so permissions,
  timestamps, ownership, links, and directory structure can be preserved.
- `--raw` is valid only when `--path` resolves to exactly one regular file;
  it writes that file's content without a tar wrapper.
- Interpret `--path` as a volume-root-relative, slash-separated path. Accept
  a leading `./` for convenience, normalize it, and reject absolute paths,
  drive-qualified paths, NULs, and any `..` traversal.
- A directory selection includes the directory entry and all descendants.
  Matching must occur on normalized path components, not string prefixes
  (`foo` must not also select `foobar`).
- Preserve tar metadata for selected entries. Preserve symlink and hardlink
  entries without following them outside the archive.
- Reject duplicate/conflicting normalized names and unsafe link targets in
  raw or filesystem-oriented output modes. Merely emitting a filtered tar
  may retain link metadata but must never dereference it on the host.
- Use the stored volume file index to validate that the requested path exists
  and identify its type before opening the object. Legacy snapshots without
  an index may fall back to a single streaming tar scan, clearly reporting
  that lookup is slower.
- Continue streaming the tar. Do not unpack into a temporary directory.
- Progress measures source object bytes consumed. The total is exact for the
  stored object, while the selected output size can remain informational
  because tar traversal may require reading past unrelated entries.

Shared implementation:

- Add a reusable normalized archive-path type and component-aware subtree
  matcher.
- Add a streaming tar filter that copies selected headers and bodies to a tar
  writer, with an alternate raw-file sink.
- Keep selection independent of the local CAS by consuming the E2
  object-source interface; this makes E4 a source substitution rather than a
  second extraction implementation.

Tests:

- Extract a file, empty directory, nested directory, symlink, hardlink, and
  zero-byte file.
- Confirm component-boundary matching and deterministic filtered-tar output.
- Reject absolute, drive-qualified, traversal, and malformed paths.
- Reject `--raw` for directories, links, multiple matches, and missing files.
- Confirm metadata preservation and no host-side link dereferencing.
- Cover indexed lookup and the legacy-index fallback.
- Verify partial reads, malformed tar headers, digest mismatches, and
  cancellation leave no final output.

### E4. Extract resources directly from `.dvca` archives

Goal:

- Provide the E2 and E3 extraction behavior directly from a portable archive
  without first importing it into the local store.

Recommended CLI:

```text
dockervc archive extract <file.dvca> volume <volume> -o <volume.tar>
dockervc archive extract <file.dvca> volume <volume> --path <path> -o <selection.tar>
dockervc archive extract <file.dvca> volume <volume> --path <file> --raw -o <file>
dockervc archive extract <file.dvca> image <image-ref-or-id> -o <image.tar>
```

Semantics and format considerations:

- Read and validate `manifest.json` before resolving a resource. Do not write
  snapshot metadata or objects into the local store.
- Locate the referenced `objects/<digest>` member through the portable
  archive index. Reject duplicate object paths, unexpected entry types,
  truncated data, digest/path disagreement, and references to absent objects.
- Apply the same resource resolution, path normalization, filtered-tar, raw
  extraction, overwrite, temporary-file, cancellation, and progress rules as
  E2/E3.
- Extraction must work on seekable archive files. For stdin, either spool to
  a bounded/declared temporary file or reject it initially with a clear
  message; do not silently buffer an unbounded archive in memory.
- Structural validation may be limited to the manifest and selected object
  for fast extraction. Offer or document `archive verify` when the user wants
  a full-archive checksum pass.
- Preserve compatibility with the current `.dvca` format. Do not modify the
  manifest or treat a partial selection as a new snapshot.

Shared implementation:

- Implement the E2 object-source interface for indexed `.dvca` members.
- Route both local-snapshot and archive extraction through the same selector
  and extraction service. Only manifest loading and object opening should
  differ by source.
- Reuse archive-inspection indexing code so large archives are not scanned
  once for `show` and again merely to locate the selected object in the same
  process.

Tests:

- Run the same whole-resource and path-selection contract tests against local
  CAS and `.dvca` object sources.
- Extract from archives with reordered members and unrelated extra objects.
- Detect missing, duplicated, truncated, corrupt, and mismatched objects.
- Confirm extraction does not mutate the local database or CAS.
- Verify large-object streaming, cancellation cleanup, stdout purity, and
  redirected progress output.

### E5. Evaluate importable selective resource bundles

Goal:

- Decide whether users need a portable package that can restore one volume or
  load one image without importing a complete snapshot. Do not overload or
  silently weaken the existing `.dvca` snapshot contract.

Decision gate:

- First ship and observe E2–E4. Plain volume/image tar extraction may satisfy
  recovery and transfer needs (`docker load` already accepts image output).
- Proceed only if there is a concrete need for retained dockervc metadata,
  direct named-volume restore, multi-resource selection, or a one-command
  import/apply workflow.

Proposed CLI if needed:

```text
dockervc bundle volume <snapshot> <volume> -o <volume.dvcb>
dockervc bundle image <snapshot> <image-ref-or-id> -o <image.dvcb>
dockervc bundle show <file.dvcb>
dockervc bundle verify <file.dvcb>
dockervc bundle import <file.dvcb>
```

Format and safety design requirements:

- Use a distinct bundle extension and format/version marker (for example,
  `.dvcb`), not a `.dvca` containing an edited full-snapshot manifest.
- Give every bundle its own immutable bundle ID derived from canonical
  metadata and referenced object digests. Never reuse the source snapshot ID
  for a partial manifest, avoiding collisions with later full-snapshot
  imports.
- Record source snapshot ID as provenance only, plus resource type, original
  identity, capture metadata, object digest, logical size, stored size,
  compression, and required format version.
- Define explicit conflict behavior at import/apply time: fail by default if
  a target volume or image identity exists, with separately reviewed rename
  or replace options. Any destructive replacement requires an explicit flag
  and, for volumes, a safety snapshot where feasible.
- Keep verification possible without Docker. Applying a volume or loading an
  image may require Docker and should reuse rollback/restore primitives and
  progress reporting.
- Specify whether one bundle may contain multiple selected resources before
  freezing v1. Prefer a manifest capable of a resource list even if the first
  CLI creates one-resource bundles.
- Document forward-compatibility rules, canonical manifest encoding,
  checksum/signature boundaries, and limits for entry count, path length,
  and declared sizes.

Shared implementation:

- Reuse resource selectors, object sources, safe archive writing, digest
  verification, and progress reporting from E2–E4.
- Keep bundle import separate from snapshot import in storage and CLI layers;
  a bundle is a resource transfer artifact, not snapshot history.

Tests required before stabilizing the format:

- Deterministic bundle bytes and IDs for identical inputs.
- Round-trip volume restore and Docker image load.
- Provenance survives without creating or shadowing a snapshot record.
- Conflict, rename, explicit replace, safety-checkpoint, and cancellation
  behavior.
- Unknown versions/features fail safely; optional metadata is ignored only
  when the format declares it safe.
- Corrupt manifests, object digests, duplicate entries, zip/tar path
  traversal, oversized declarations, and truncated bundles are rejected.
- Cross-platform path and metadata fixtures, plus compatibility fixtures that
  remain readable after future format changes.

## Planned feature: dependency-aware container reconciliation

Status: implementation underway. Reconciliation Stage 1 is committed in
`9223a5e`; the core reference-only recipe work described below is committed in
`5da9882`. The current-dependency container redeployment strategy was
implemented locally on 2026-09-22 and is undergoing final verification.

### Goal

A snapshot should store a container as a recreation recipe whose fields refer
to the image, volumes/mounts, networks, configuration, and desired running
state needed to reproduce it. Restoring a container should reconcile that
recipe with the live Docker engine instead of always deleting and recreating
everything:

- leave present and unchanged entities untouched;
- prompt before restoring present but changed entities;
- create or load entities that are completely missing;
- stop/recreate the container only when its creation-time configuration or
  image actually requires it;
- restore a changed volume in place under the same volume name;
- show and safely coordinate every container affected by shared storage.

Names remain the user-facing identity, but names alone are not sufficient to
detect equality. Each reference also needs an immutable digest, normalized
configuration hash, or captured content/index hash, plus the snapshot object
required to restore it.

### Implementation checkpoint: reconciliation stage 1

Implemented on 2026-09-21 and committed as `9223a5e`:

- Live rollback inventory now inspects every container once and records its
  normalized creation-configuration hash, immutable image ID, configured
  image reference, running state, and named-volume attachments.
- Container configuration hashing reuses the same normalized create mapping
  as restoration, excluding daemon-generated runtime state and comparing the
  image separately.
- Before a rollback plan is built, required existing volumes with snapshot
  file indexes are deep-scanned and classified as unchanged, changed,
  missing, or unverifiable.
- Unchanged volumes are omitted from the mutation plan. Changed volumes show
  the file-diff summary and are restored; missing volumes are created;
  legacy/unscannable volumes are marked unverifiable and restored
  conservatively with a warning.
- New snapshots persist file indexes for empty volumes too, allowing an empty
  live volume to compare as unchanged instead of looking like a legacy volume
  with no index support.
- Existing containers whose normalized configuration and image identity match
  the snapshot are retained instead of always being removed/recreated.
- Running-state-only drift produces start/stop actions without recreation.
- Changed shared volumes discover every live attached container. Running
  users, including users outside the selected scope and read-only users, are
  stopped before the volume restore and restarted afterward if they were
  previously running. The plan names the shared and out-of-scope impact.
- Cancellation during deep scanning aborts planning before any mutation.
- Focused reconciliation, shared-volume, configuration-hash, rollback,
  Docker mapping, and CLI tests pass; `go vet ./...` and `go test ./...` pass.

Stage 1 intentionally kept the existing snapshot compatibility path in
which each container may reference a committed filesystem image object.
Remaining work begins with the new reference-only container recipe: make a
container depend on the captured original image entity instead of requiring a
per-container committed filesystem, then make image loading/tag conflicts
safe before changing the snapshot format.

### Implementation checkpoint: reconciliation Stage 2 core

Implemented on 2026-09-22 and committed as `5da9882`:

- `ContainerRecord` now has backward-compatible `ImageRef`, `ImageID`,
  `ImageKey`, and `ConfigHash` recipe fields. `ImageObject` and `LayerHash`
  remain available for legacy manifests.
- `ImageRecord` now distinguishes immutable local ID, registry digests,
  mutable provenance refs, canonical dependency key, and stored object.
- New snapshots no longer run `docker commit` per container. They inventory
  container recipes and capture each required base image once at snapshot
  scope. `snapshot --only containers` automatically includes those images.
- Image capture saves by immutable ID and streams the docker-save archive
  through tag neutralization. `RepoTags` and legacy `repositories` metadata
  are removed so `docker load` cannot move a public tag. A versioned dedup key
  prevents reuse of older tag-bearing image objects.
- Restore planning now supports both formats: new recipes resolve an exact
  existing image ID or load one shared image object under a dockervc-owned
  internal tag; legacy recipes continue using their committed filesystem.
- Multiple containers referencing the same image produce one load step and
  share the resolved image reference.
- Added `rollback --reuse-existing-by-name`. This opt-in mode performs no deep
  dependency comparisons and never restores, loads, or replaces dependencies.
  It verifies that every required image tag/ID, volume name, and network name
  exists, then creates only missing selected containers. Any missing named
  dependency aborts planning.
- Added manifest compatibility, tag-neutral archive, shared-image planning,
  exact-image reuse, and reuse-by-name tests.
- Added `rollback --recreate-with-current-dependencies` as a distinct
  redeployment/update strategy. It preflights current same-name image,
  volume, and network dependencies, removes every selected live container,
  and recreates it from snapshot configuration without loading, restoring,
  replacing, retagging, or comparing those dependencies. Missing selected
  containers are created; recorded running state is reapplied.
- Both guided interfaces offer ordinary snapshot reconciliation,
  recreate-with-current-dependencies, and missing-only strategies. The
  full-screen TUI always previews the two name-trusting strategies before
  offering to apply them.
- Ordinary container and standalone-image restoration now prefers the
  recorded original image reference. Free names are restored automatically;
  a name owned by a different or unverifiable image is reported before the
  safety checkpoint and requires explicit replacement confirmation, default
  no. When all container dependencies exist, the conflict output also offers
  the missing-only and recreate-with-current-dependencies alternatives.
- This feature batch is versioned as `0.8.0`; it adds public rollback modes
  and image-name replacement policy rather than patching existing behavior.

Still pending before Stage 2 is complete: richer per-entity dry-run/status
output and storage accounting, broader portable/doctor/prune compatibility
fixtures, Docker integration tests, manual OpenClaw validation, and release
documentation.

### Next implementation stage: reference-only container recipes

#### Current behavior to replace for new snapshots

`captureContainers` currently performs this for every container:

```text
container -> docker commit -> docker save -> ContainerRecord.ImageObject
```

Restoration loads that per-container committed image under a deterministic
`<container>-restored-from-<snapshot>` tag. This remains the compatibility
path for existing snapshots, but new snapshots should instead store one
recreation recipe per container and reference image/volume/network entities
captured once at snapshot scope.

The new default deliberately excludes files written only to the disposable
container writable layer. Persistent state must live in the captured image,
a named/anonymous volume, or an explicitly captured bind mount. A future
opt-in `--include-container-filesystem` mode may retain the old commit-based
behavior when users need it.

#### Stage 2.1: manifest identity fields

Extend `ContainerRecord` with explicit, optional reference fields while
retaining `ImageObject` and `LayerHash` for legacy decoding/restoration:

```go
ImageRef  string // original Config.Image, e.g. openclaw-dev-base:1.0
ImageID   string // immutable local image ID used by the container
ImageKey  string // stable link to the snapshot ImageRecord
ConfigHash string // normalized creation-time configuration
```

Clarify `ImageRecord` identity instead of overloading its current `Digest`
field, which may contain either a registry digest or a local image ID:

```go
ID      string   // immutable local image ID
Refs    []string // mutable repo tags, provenance only during container restore
Digests []string // immutable registry digests when present
Key     string   // canonical manifest/dedup lookup key
Object  string   // tag-neutral docker-save object
```

New fields must use backward-compatible JSON encoding. Old manifests with
only `ImageObject` continue to decode without migration. Update object
enumeration, portable archive validation, doctor, show/diff, export/import,
and selected-artifact code so referenced image objects remain reachable.

#### Stage 2.2: dependency-aware capture

Refactor capture into an inventory/dependency flow:

1. Inventory containers and images once.
2. Build an image-ID-to-`ImageRecord` map.
3. For each selected container, record its normalized recipe, image ID/ref,
   mount/network references, configuration hash, and desired running state.
4. Resolve every selected container's required image to one image record.
5. Capture each required image once, even when many containers share it.
6. Do not run `docker commit` for new reference-only records.

Container selection implicitly includes required dependencies. In particular,
`snapshot --only containers` must still capture the images needed to recreate
those containers. Document `--only` as selecting primary resources while
allowing mandatory dependencies to ride along. A volume or network should
likewise be referenced by the container recipe, while existing scope policy
decides whether its restorable data/config is included.

Expected storage relationship:

```text
openclaw_secretary ──┐
                     ├── one openclaw-dev-base image object
openclaw_worker ─────┘

openclaw_secretary ──┐
                     ├── one openclaw_dev_home volume object
openclaw_worker ─────┘
```

#### Stage 2.3: tag-neutral image archives

Prevent `docker load` from moving public tags before reconciliation can apply
policy:

1. Save images by immutable ID, not by the first public tag.
2. Inspect test archives and prove whether Docker omits `RepoTags` when saving
   by ID on supported engines.
3. If public tags remain, rewrite docker-save `manifest.json` (and any legacy
   repositories metadata) into a tag-neutral archive while preserving config
   and layer bytes.
4. Keep original tags only as `ImageRecord.Refs` provenance.
5. On restore, load the tag-neutral object, verify the resulting identity,
   and reapply its recorded original reference when that name is free. Use a
   dockervc-owned internal tag such as
   `dockervc/restore:<snapshot>-<digest-prefix>` only as the safe fallback for
   an unconfirmed conflict or a record without an original reference.

Never silently retag `openclaw-dev-base:1.0` or another occupied public
reference. Container and standalone-image restoration share one confirmed
replacement policy: identify the conflict before the safety checkpoint,
default to no, and leave the prior image object intact if its tag is moved.

#### Stage 2.4: dual-path restore planning

Resolve a selected container's image dependency before deciding whether the
container needs recreation:

- exact image ID/digest present: reuse it without loading or tagging;
- required image absent and original reference free: load its snapshot image
  object once and restore that original reference;
- original public tag now points elsewhere: stop for explicit confirmation;
  on approval, move the tag to the exact snapshot image, otherwise abort the
  CLI operation (the pure planner retains an internal-reference fallback);
- several selected containers share the image: emit one load step and give
  all create steps the same resolved reference.

Add an explicit resolved image reference to `StepCreateContainer` instead of
having the executor always derive `RestoreTag(snapshotID, containerName)`:

```go
ImageRef string // exact existing ID or dockervc-owned loaded-image tag
```

The executor then calls `ContainerCreateFromInspect` with `Step.ImageRef`.
Planning remains pure; loading/tagging stays in executor steps.

Choose restore format per container:

```text
new record:    ImageKey/ImageID present -> referenced ImageRecord path
legacy record: ImageObject present       -> committed-filesystem path
invalid:       neither path complete     -> unverifiable/not recreatable
```

Do not remove legacy handling until the supported snapshot-retention window
explicitly permits a format break.

#### Stage 2.5: status, diff, and user-visible accounting

For reference-only records, container equality is:

```text
normalized configuration hash
+ immutable image identity
+ desired running state (reported/actioned separately)
```

Volume and bind-mount content remain independent comparisons. `LayerHash`
continues to describe only legacy committed-filesystem snapshots or a future
opt-in writable-layer mode. Update snapshot size/accounting output so one
shared image is not presented as per-container storage.

Dry-run output must make reuse visible without turning no-ops into executable
steps, for example:

```text
container openclaw_secretary
  configuration  unchanged
  image          reuse sha256:abc...
  volume         reuse openclaw_dev_home
  action         create container only
```

#### Stage 2.6: required tests

- New manifest JSON round trip, plus old manifest compatibility fixtures.
- Two containers sharing one image produce one captured image object and one
  image-load step.
- `--only containers` captures required images automatically.
- Locally built, dangling, tagged, registry-digested, and multi-tag images map
  to the correct stable image record.
- Tag-neutral archive tests prove restore cannot move a conflicting public
  tag as a side effect of `docker load`.
- Missing container with unchanged dependencies creates only the container.
- Missing image loads once, receives an internal reference, and then creates
  all dependent containers.
- Existing exact image is reused without load/tag mutations.
- Public tag pointing to a different digest is preserved.
- Configuration-only drift recreates the container while reusing unchanged
  image, volume, and network entities.
- Running-state-only drift still uses start/stop without recreation.
- Legacy `ImageObject` snapshots retain their existing restore behavior.
- Object reachability, doctor, prune, export/import, and corruption tests
  cover both manifest generations.

#### Stage 2 completion criteria

- New snapshots create no per-container committed image objects by default.
- Every recreatable new container resolves to exactly one captured image
  entity and a complete normalized runtime recipe.
- Container restore cannot silently move public image tags.
- Unchanged images/volumes/networks are not rewritten.
- Missing dependencies are created once and shared correctly.
- Old snapshots remain restorable and portable.
- `go vet ./...`, `go test ./...`, Docker integration fixtures, and a manual
  OpenClaw dry-run/recreate test all pass before release documentation or a
  version bump is finalized.

### Snapshot container recipe

Conceptually record:

```text
container: openclaw_secretary
├── normalized container configuration and hash
├── image reference
│   ├── original tag/name
│   ├── expected immutable digest
│   └── snapshot image object
├── mounts
│   ├── volume name, driver/options, destination and access mode
│   ├── expected volume content/index hash
│   └── snapshot volume object
├── bind-mount host path, destination, access mode and optional content object
├── network names and normalized configuration hashes
└── desired state: running or stopped
```

The manifest references globally content-addressed and deduplicated objects;
it does not duplicate an image or volume object for every container that uses
it. Preserve the complete creation-time runtime specification, including
entrypoint, command, environment, user, working directory, hostname, ports,
restart policy, labels, health check, resource/security settings, networks,
and aliases. Exclude runtime-generated IDs, addresses, and sandbox state from
configuration equality.

### Reconciliation states

For the selected container and its dependency graph, run a deep comparison
and classify every entity before mutating anything:

| State | Meaning | Default action |
|---|---|---|
| Unchanged | Live entity matches the snapshot identity/content | Reuse it untouched |
| Changed | Same user-facing entity exists but differs | Show the diff and prompt to restore |
| Missing | Required entity does not exist | Create/load it from the snapshot |
| Unverifiable | Equality cannot be established safely | Warn and require an explicit decision |
| Shared conflict | Changing it affects other containers | Show all affected containers and require a group decision |

The dry-run and confirmation UI should show why an entity received its state,
not merely say that the whole container will be replaced.

### Minimal-action rules

- If container configuration, image, storage, network dependencies, and
  desired state are all unchanged, do nothing.
- If only the running state differs, start or stop the existing container;
  do not recreate it.
- If only a volume differs, stop affected containers, checkpoint and restore
  the volume, then restart them; do not recreate an otherwise unchanged
  container.
- If creation-time configuration differs, stop/remove the existing container,
  reuse every unchanged dependency, restore/create only changed or missing
  dependencies, then recreate the container.
- If the required image digest differs, load the snapshot image under a
  dockervc-owned internal tag and recreate the container against that exact
  image after confirmation.
- Never delete a named volume merely because its container is recreated.

Example dry-run:

```text
Restore plan for openclaw_secretary

  container configuration   unchanged     keep
  image                     unchanged     keep
  volume openclaw_dev_home  changed       restore in place
  network bridge            unchanged     keep
  running state             running       restore after volume

Actions:
  stop affected containers
  create safety checkpoint
  restore openclaw_dev_home in place
  restart previously running containers
```

### Deep comparison rules

- Container: compare a normalized creation specification, excluding live
  status, IDs, assigned IP/MAC addresses, timestamps, and other generated
  fields.
- Image: compare immutable image digest/content identity, never the mutable
  tag alone.
- Volume: scan the live volume and compare its file index/content hashes with
  the captured snapshot index. Report created, modified, and deleted files.
- Bind mount: deep-scan only when its contents were captured and the path can
  be accessed safely on the Docker host; otherwise mark it external or
  unverifiable.
- Network: compare normalized reproducible configuration, excluding runtime
  endpoint membership and generated IDs.
- Desired state: compare running/stopped state separately so it never forces a
  container recreation by itself.

Deep scans should be limited to the selected container's dependency graph,
emit progress/ETA, support cancellation, and finish before presenting the
destructive confirmation.

### Image handling

Treat image tags as mutable aliases and digests as identity:

- reuse an existing exact digest without prompting;
- load a missing digest from its snapshot object;
- use a deterministic internal restore reference such as
  `dockervc/restore:<snapshot>-<digest-prefix>`;
- do not silently move or replace a public tag that now points elsewhere;
- retain the original tag as recipe/provenance metadata.

The image archive/loading path must be designed so `docker load` cannot
silently replace conflicting public tags before the conflict policy runs.
Initially prefer fully archived images; registry-reference and hybrid storage
modes can be evaluated later.

### Volumes and shared-volume groups

A shared volume is one Docker volume mounted by more than one container. It is
not a copy per container: every attached container sees the same files. For
example:

```text
openclaw_secretary  ──┐
                      ├── openclaw_dev_home mounted at /home/dev
openclaw_secretary2 ──┘
```

Restoring `openclaw_dev_home` for either container changes the data visible to
both. Before volume restoration, build a dependency map across running and
stopped containers:

```text
volume name -> every attached container and its read/write mode
```

Rules:

- Preserve an existing volume's name and restore its contents in place.
- Create a missing volume using its recorded name, driver, options, and labels
  before extracting content.
- Compare the volume only once even when several selected containers share it.
- Before clearing a changed volume, take a safety checkpoint and stop every
  attached container, not only the selected one. This prevents writers and
  readers from seeing a partially restored state.
- If an attached container is outside the selected restore scope, show it and
  require an explicit group decision; default to abort.
- After restoration, restart only containers that were running beforehand,
  unless their own snapshot recipe explicitly changes desired state.
- Treat anonymous volumes as explicit identities internally by recording and
  reusing Docker's generated source name. Otherwise Docker would create a new
  empty anonymous volume during container recreation.

Suggested shared-volume prompt:

```text
Volume openclaw_dev_home has changed.

It is shared by:
  - openclaw_secretary
  - openclaw_secretary2

Restoring it changes /home/dev for both containers.

[S] Stop both, checkpoint, restore the volume, and restart them
[K] Keep the current volume
[A] Abort
```

Volume restoration is not atomic: clear-and-extract may fail partway through.
The safety checkpoint is the recovery mechanism, rerunning must be idempotent,
and failure output must identify completed, partial, and untouched entities.

### Possible feature: changed-files-only volume restoration

Status: design candidate only; do not change the safe whole-volume restore
default until the semantics, recovery path, and Docker integration tests are
complete.

The existing deep comparison already identifies regular-file, link, type,
mode, creation, modification, and deletion differences by path. A future
selective mode could apply that exact delta instead of clearing and extracting
the entire volume:

```powershell
dockervc rollback <snapshot> --volumes <volume> --changed-files-only
```

Exact delta semantics:

- A path created after the snapshot is removed.
- A path modified after the snapshot is replaced with the snapshot version.
- A path deleted after the snapshot is recreated from the snapshot.
- A path whose type changed is removed safely before restoring the recorded
  type (for example, file to directory or symlink to regular file).
- Unchanged paths are never rewritten.
- The Docker volume object and name remain unchanged.
- This is still an exact restore to the selected snapshot, not a merge that
  keeps arbitrary newer files.

Default and fallback policy:

- Whole-volume clear-and-extract remains the default because it is simpler to
  reason about and is safer for application datasets whose files form one
  consistency unit.
- Selective restore must be explicit and must print a warning for databases
  and other multi-file state. Restoring only part of a database directory can
  combine files from incompatible points in time even when every selected
  file is individually valid.
- Automatically fall back to whole-volume restoration, with an explanation,
  when the snapshot lacks a usable file index, the diff is unverifiable,
  archive paths are unsafe, hard-link dependency closure cannot be resolved,
  unsupported special files are involved, or the delta is large enough that
  selective application offers no meaningful benefit.
- Never silently downgrade from exact content comparison to metadata-only
  comparison to enable this feature.

Required index-format work:

- The current file index omits directory entries because full extraction can
  reconstruct ordinary parents. Selective exact restore needs directory
  entries, including empty directories, permissions, timestamps where
  preserved, and relevant archive metadata.
- Add a backward-compatible index generation/version field so restore can
  distinguish indexes capable of exact selective application from older
  indexes.
- Represent hard-link targets and enough dependency information to include a
  link's required target in the restore closure.
- Decide and document support for device nodes, FIFOs, sockets, xattrs, ACLs,
  ownership, Windows/Docker Desktop translation, and PAX metadata. Unsupported
  entries must trigger a safe fallback rather than partial guessing.
- Tar entry offsets are not directly seekable inside current zstd-compressed
  CAS objects. The first implementation may stream the full stored archive and
  filter entries while writing only selected paths. A future seekable index or
  per-file object layout can be evaluated separately.

Proposed planning flow:

1. Run the existing exact deep comparison and obtain sorted created,
   modified, deleted, and type-changed path sets.
2. Expand the mutation set to include required parent directories, hard-link
   targets, and metadata dependencies.
3. Produce a dry-run section listing every path action and whether selective
   restore is supported or will fall back to whole-volume restore.
4. Ask for explicit confirmation and create a safety checkpoint before any
   path mutation. The first safe implementation may retain a full-volume
   checkpoint even though the forward restore is selective.
5. Stop every attached running container, including shared-volume users
   outside the selected container scope, using the same group-safety rules as
   whole-volume restore.
6. Revalidate and normalize every target path below the mounted volume root;
   reject absolute paths, traversal, symlink escapes, and root deletion.
7. Remove live-only paths and type conflicts in depth-first order so children
   are handled before parents.
8. Stream the snapshot volume tar, extracting only the restore closure into a
   staging area inside the same volume when possible.
9. Move staged entries into place, then apply directory metadata after child
   extraction. Never expose an archive-controlled path outside the volume.
10. Verify restored paths against snapshot hashes/metadata and verify selected
    deletions are absent.
11. Restart only containers that were running before the operation, unless a
    selected container recipe requires a different desired state.
12. Report restored, deleted, unchanged, fallen-back, partial, and failed path
    counts plus the checkpoint needed for recovery.

Atomicity and recovery:

- Selective restoration is not transactionally atomic across many paths.
  Staging plus same-filesystem rename can make individual regular-file
  replacement atomic, but the whole mutation set can still fail midway.
- Keep the volume safety checkpoint mandatory by default. `--keep-current`
  must carry the same explicit recovery warning as whole-volume restore.
- A failure report must distinguish paths not started, staged, replaced,
  deleted, verified, and failed.
- Rerunning the same selective restore must be idempotent.
- Cancellation is accepted only at safe boundaries; cleanup must remove
  staging paths without deleting restored or unrelated live data.

Performance expectations:

- The live volume still needs a complete read/hash pass to prove which files
  changed under exact comparison rules.
- Without a seekable archive index, the stored snapshot archive may also need
  a complete sequential read to locate selected entries.
- The primary savings are less deletion, extraction, and write amplification,
  especially when a small number of files changed in a large volume.
- This feature complements but does not replace the ranked scan/checkpoint
  optimizations. Staged scan reuse and scoped safety checkpoints can reduce
  the remaining full-volume reads.

Required tests:

- Created paths are removed; modified and deleted paths are restored.
- Unchanged files retain content, metadata, inode where reasonably portable,
  and modification time because they are not rewritten.
- Empty directories, nested deletion ordering, file/directory type swaps,
  symlinks, hard links, permissions, and long/PAX paths behave correctly.
- Absolute paths, `..`, symlink escapes, root targets, duplicate tar entries,
  and malicious link targets are rejected before mutation.
- A shared volume stops/restarts the complete safe container group.
- Cancellation and injected failures at every stage leave a recoverable,
  accurately reported state and no unsafe staging debris.
- Legacy/no-index snapshots and unsupported entry types fall back safely.
- A large-delta policy selects whole-volume restore deterministically.
- Selective restore followed by a fresh exact deep scan reports no drift.
- Database-style fixture documentation demonstrates why the mode remains
  explicit rather than automatic.

### Bind mounts

- If contents were not captured, reuse/remount the host path without changing
  it and report that its state is external to the snapshot.
- If contents were captured, compare first and require explicit confirmation
  before clearing or overwriting changed content.
- Reject filesystem roots, home roots, symlink escapes, archive traversal, and
  other unsafe targets.
- Bind paths belong to the Docker host. For Docker Desktop or a remote daemon,
  capture and restoration should use a helper container that mounts the path;
  the dockervc client's local filesystem may not represent the daemon host.

### Proposed restore flow

1. Validate the snapshot manifest and every referenced object/checksum.
2. Resolve the selected container's full dependency graph, including all live
   containers sharing its volumes.
3. Deep-compare the normalized container configuration and all dependencies.
4. Produce a dry-run reconciliation plan with entity states and exact actions.
5. If nothing differs, report no changes and exit successfully.
6. Prompt once with explicit changed/shared/destructive actions, while still
   allowing policies to be expressed non-interactively by future flags.
7. Take a safety checkpoint covering all entities that may be changed.
8. Stop the minimum safe set of containers.
9. Create/load missing entities and restore only confirmed changed entities.
10. Recreate the selected container only if its image or creation-time
    configuration requires it.
11. Restore desired running states and wait for health checks where available.
12. Report reused, created, restored, recreated, partial, and failed entities.

### Rollback performance optimization roadmap

Ranking balances expected wall-clock improvement, safety/correctness impact,
how commonly the work is encountered, and implementation effort. Rank 1 is
the first item to implement. "Effort" includes design, compatibility tests,
failure cleanup, and Docker integration verification—not only code size.

The current exact rollback path can read a changed volume three times:

```text
deep comparison -> pre-rollback safety snapshot -> snapshot restore
```

It also creates a full-engine safety snapshot even when the plan changes only
one container or volume. Docker Desktop adds daemon and filesystem-boundary
latency, particularly for volumes containing many small files. Exact equality
currently requires a complete tar stream and SHA-256 hash of every regular
file, and required volumes are scanned sequentially.

| Rank | Optimization | Importance | Effort | Reason for rank |
|---:|---|---|---|---|
| 1 | Scope the safety checkpoint to the mutation graph | Critical | Medium | Removes unrelated image/volume/container capture from nearly every applied partial rollback while preserving recovery for everything the plan can change. |
| 2 | Reuse the deep-scan stream as staged checkpoint data | Critical | High | Eliminates the second complete read of every changed volume; this is the largest remaining I/O reduction after checkpoint scoping. |
| 3 | Move final confirmation before checkpoint capture | High | Low | Prevents an expensive checkpoint when the user declines the plan and fixes the current surprising confirmation order. Low-risk, quick improvement. |
| 4 | Add bounded parallel volume comparison, starting with `status --deep` | High | Medium | Independent volumes can scan concurrently; the read-only status path is the safest proving ground. Start with two helpers before considering rollback parallelism. |
| 5 | Show live TUI phase, volume, bytes, rate, and ETA | High | Medium | Does not reduce I/O, but makes long correct scans distinguishable from hangs and exposes which phase needs optimization. Reuse the existing progress reporter/tracker rather than creating a second calculation path. |
| 6 | Optimize clear-and-extract restoration | Medium-High | Medium-High | `find /dst -mindepth 1 -delete` is costly for many small files. Benchmark safe alternatives and the extraction transport before changing destructive code. |
| 7 | Add Docker responsiveness preflight and timing diagnostics | Medium | Low | Separates dockervc cost from slow Docker API/desktop behavior and records per-phase timings for evidence-driven work. |
| 8 | Offer an explicit metadata-first fast comparison mode | Medium | High | Can avoid hashing unchanged-looking files, but size/mtime/mode are not proof of content equality. It must be opt-in, clearly weaker, and never replace the exact default. |

#### Rank 1: mutation-scoped safety checkpoint

- Derive checkpoint scope from the final plan, not directly from the user's
  original selection.
- Include every entity that can be mutated: removed/recreated containers,
  restored volumes, changed images/networks when supported, and all live
  containers affected by shared-volume coordination.
- Do not capture unrelated engine images, volumes, networks, or containers.
- Preserve enough dependency metadata to reverse a partial failure.
- Dry-run must state exactly what the safety checkpoint would contain.
- Tests must cover shared volumes and out-of-scope attached containers so
  scoping cannot omit data that rollback will change.

#### Rank 2: staged scan/checkpoint stream reuse

- During exact live-volume comparison, tee the same tar stream into a staged
  CAS object while building its file index.
- If the volume is unchanged, discard/unreference the staged result.
- If the volume will be restored, promote the staged object and index into
  the safety checkpoint instead of reading the live volume again.
- Do not publish a snapshot record until the complete mutation graph is
  staged and validated.
- Cancellation or failure must leave only pruneable unreferenced CAS objects,
  never a plausible partial safety snapshot.
- Preserve current content hashes and deterministic indexes so this does not
  create a second comparison format.

#### Rank 3: confirm before expensive checkpoint work

The intended order is:

```text
compare -> print exact plan/warnings -> confirm -> safety checkpoint -> apply
```

The checkpoint remains mandatory by default after confirmation. If checkpoint
creation fails, abort before the first mutation. This change improves aborted
rollback time but does not weaken recovery guarantees for an applied rollback.

#### Rank 4: bounded concurrent comparison (`status --deep` first)

Current behavior:

- `status --deep` sorts the selected volume names, then awaits one complete
  `DiffLiveVolumeTracked` call before starting the next.
- Only one actively scanning `dockervc-tar-*` helper normally exists. A prior
  helper may briefly overlap during asynchronous removal, but that is cleanup,
  not useful parallel work.
- Large volumes and many-small-file volumes therefore add their scan times
  together even when the Docker host has spare I/O and CPU capacity.

Phase A — read-only `status --deep` implementation:

- Replace the sequential scan loop with a bounded worker pool.
- Begin with a hard default concurrency of two. Do not expose a public tuning
  flag until measurements show that users benefit from changing it.
- Each worker owns one independent `VolumeTarStream` and therefore one unique
  temporary `dockervc-tar-*` helper container.
- Never schedule the same volume more than once, even when selectors contain
  duplicates or several higher-level entities reference it.
- Feed workers from the already sorted volume-name list, store results by
  input position/name, and render final drift output in deterministic sorted
  order regardless of completion order.
- Preserve current best-effort behavior: a normal scan failure marks that
  volume unavailable and does not prevent unrelated volumes from finishing.
- Treat context cancellation as global: stop scheduling, cancel every active
  helper stream, wait for all workers and helper cleanup, then return the
  cancellation error.
- Bound channels and progress delivery so a slow terminal cannot block tar
  streaming or hashing.
- Keep the operation strictly read-only. No volume contents, container state,
  snapshot records, or CAS objects may be changed by `status --deep`.

Concurrent progress design:

- Do not let independent per-volume console renderers overwrite or interleave
  terminal lines.
- Give each worker a lightweight per-volume byte counter and send structured
  updates to one coordinator.
- The coordinator owns the reporter and displays aggregate bytes/rate/ETA plus
  the names of active volumes. Estimated totals are the sum of each selected
  snapshot index's estimated tar bytes.
- Redirected output should emit starts, completions, failures, and periodic
  aggregate updates without ANSI control sequences.
- The future TUI view should show both overall progress and up to the active
  worker limit, for example:

  ```text
  deep status  2 / 5 volumes  1.8 GiB / ~4.2 GiB  96 MiB/s  ~ETA 00:25
    scanning openclaw_dev_home
    scanning postgres_data
  ```

Concurrency and resource rules:

- Two helpers is the initial ceiling, not an assumption that more is always
  faster. Multiple volumes may reside on the same Docker Desktop virtual disk,
  where excess concurrency increases contention and total elapsed time.
- Each worker hashes file content locally while Docker generates a tar stream;
  benchmark CPU, disk, daemon, pipe, and memory pressure before raising the
  limit.
- Do not start a helper for missing volumes or snapshots without a comparable
  file index.
- Unique helper names already prevent naming collisions; tests must still
  prove all helpers are removed on success, error, and cancellation.

Phase B — possible rollback reuse:

- After the `status --deep` worker pool passes race, cancellation, cleanup,
  and performance tests, extract the generic read-only scan coordinator for
  rollback reconciliation.
- Rollback may reuse parallel comparison only; volume clearing, extraction,
  container stop/start, and every other mutation remain ordered and
  sequential.
- Do not enable rollback parallelism if it complicates staged checkpoint
  reuse or makes the Rank 1/2 optimizations less safe.

Required tests:

- A blocking fake tar opener proves that two scans overlap and a third waits.
- Maximum observed concurrency never exceeds the configured worker limit.
- Duplicate selectors produce one scan/helper.
- Results remain sorted when workers finish out of order.
- One scan failure does not cancel successful peers; all errors are joined.
- Context cancellation releases blocked readers, waits for workers, and leaves
  no helper containers.
- Progress aggregation remains race-free and non-blocking under a slow
  reporter (`go test -race ./internal/cli ./internal/snapshot`).
- Concurrency one produces results identical to the current sequential path.

Benchmark before selecting the final default:

- Run concurrency 1, 2, and 4 against two and four independent volumes.
- Include large-file, many-small-file, mixed-size, and same-physical-disk
  fixtures on Docker Desktop and native Linux.
- Record total elapsed time, time to first byte, per-volume and aggregate
  throughput, Docker daemon latency, CPU, memory, and helper cleanup time.
- Keep two as the default only if it provides a repeatable improvement without
  unacceptable tail latency or daemon instability; otherwise default to one
  while retaining the tested coordinator for hosts that can benefit later.

#### Rank 5: visible progress and phase timings

Integrate this work with the existing TUI live-progress stage. At minimum show:

```text
comparing volume openclaw_dev_home  1.2 GiB / ~2.0 GiB  82 MiB/s  ~ETA 00:10
creating scoped safety checkpoint   volume 1 / 1
restoring openclaw_dev_home         640 MiB / 2.0 GiB
```

Record elapsed time for inventory, comparison per volume, checkpoint capture,
volume clearing, extraction, image load, and container recreation. These
timings should be available in redirected/non-TUI output without ANSI codes.

#### Rank 6: restore-path benchmarking and optimization

- Benchmark deletion and extraction separately with large files and many
  small files on Docker Desktop and native Linux.
- Compare the existing `find -delete` path with carefully bounded alternatives
  that cannot delete the mount point or escape it.
- Compare Docker `CopyToContainer` with a helper that receives and extracts a
  tar stream directly.
- Preserve exact clear-then-extract semantics and current archive traversal
  protections.
- Do not trade same-name volume preservation for delete/recreate speed.
- Failure output must continue identifying a potentially partial volume and
  the safety checkpoint needed to recover it.

#### Rank 7: Docker latency diagnostics

- Time Docker inventory, helper creation/start, archive first-byte, archive
  completion, clear completion, and extraction completion.
- Warn when Docker API setup or first-byte latency dominates actual transfer.
- Report the active helper name and target volume so a user can inspect it.
- Add a read-only diagnostic command or verbose mode; do not make ordinary
  rollback depend on expensive calls such as full `docker system df -v`.

#### Rank 8: optional fast comparison

- Add only as an explicit policy such as `--compare=metadata`; retain exact
  content hashing as the default.
- Compare normalized path, type, size, mtime, mode, and link target first.
- Clearly report that metadata equality is probabilistic, not proof that file
  bytes match; preserved timestamps can hide content changes.
- Never use metadata-only comparison automatically for databases, legacy
  snapshots, unverifiable indexes, or safety-checkpoint decisions.
- A possible later hybrid may hash only metadata-changed files, but it needs a
  persistent trusted live baseline or change journal before it can claim exact
  equality.

#### Existing user-controlled shortcuts

- `--reuse-existing-by-name` is the fastest path when the user wants only to
  create missing containers and explicitly trusts existing named images,
  volumes, and networks. It skips deep comparison and never replaces those
  dependencies.
- `--keep-current` skips the safety checkpoint and can substantially reduce
  applied rollback time, but removes dockervc's automatic recovery point. Keep
  it an explicit expert option and warn users to use it only when an adequate
  current-state backup already exists.
- Neither shortcut changes the safe default exact-restore behavior.

#### Performance verification matrix

Measure before and after every optimization using the same fixtures:

- one unchanged large-file volume;
- one changed large-file volume;
- one unchanged many-small-files volume;
- one changed many-small-files volume;
- two independent volumes for concurrency tests;
- one shared volume with selected and out-of-scope attached containers;
- Docker Desktop and native Linux where available.

Record inventory time, time to first archive byte, scan throughput, checkpoint
bytes/time, clear time, extraction throughput, total rollback time, helper
cleanup, peak memory, and whether the plan/result is byte-for-byte equivalent.
No performance change is complete until cancellation, failure recovery,
legacy snapshots, and shared-volume safety tests still pass.

### Consistency, safety, and output requirements

- Support app-consistent capture with stopped containers; live capture remains
  best effort and must warn that databases may require native dumps or future
  quiesce hooks.
- Redact environment secrets and other sensitive configuration from plans and
  logs while retaining what exact local restoration requires.
- Use restrictive store permissions and revisit portable-backup encryption.
- Never replace a conflicting image tag, volume content, bind path, network,
  or container configuration without a visible plan and authorization.
- Preserve retryability and cancellation cleanup at every stage.
- Run post-restore health checks, but distinguish application health failure
  from mechanical restore failure.

### Questions to revisit before implementation

- Should the committed container writable filesystem remain the default exact
  image source, with the original image digest stored as provenance, or should
  users be able to declare the writable layer disposable?
- What precise normalized fields define container and network equality?
- Which non-interactive flags express restore/keep/abort decisions without
  embedding prompts in core packages?
- How should changed shared volumes interact with containers that were not
  present in the selected snapshot?
- How should secrets be encrypted in portable archives while remaining usable
  for exact local restoration?
- Which bind-mount host/daemon configurations can be supported safely in the
  first implementation?

## Verification checklist

After every integration stage:

```powershell
gofmt -w <changed-go-files>
$env:GOCACHE = 'C:\Users\ZJ\dockervc\.cache\go-build'
$env:GOTMPDIR = 'C:\Users\ZJ\dockervc\.cache\go-tmp'
go vet ./...
go test ./...
```

Before handoff:

- Build with version stamping:

```powershell
go build -ldflags "-s -w -X dockervc/internal/cli.Version=0.7.0" -o .\dockervc-progress-check.exe .
.\dockervc-progress-check.exe version
```

- Verify `snapshot --help`, `rollback --help`, `export --help`, and global
  `--no-progress` help.
- Verify redirected output contains no ANSI escape codes.
- Remove `.cache` and temporary binaries after testing.
- Run `git diff --check` and inspect `git status --short`.
- Do not commit or push unless the user explicitly requests it.

## Recovery note

If resuming after interruption, first run:

```powershell
git status --short
git diff --check
```

Then inspect this file and continue from the first unfinished numbered stage.
