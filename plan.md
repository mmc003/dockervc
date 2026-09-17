# Implementation Plan and Recovery Checkpoint — Progress/ETA and TUI

Last updated: 2026-09-17

This file records the completed v0.7.0 deep-status/progress work and the
remaining follow-ups. It is intentionally detailed enough to resume after an
interrupted session.

## Working-tree context

- Branch: `windows-port`
- Latest pushed implementation commit: `324a4dd`
  (`feat: add deep status and operation ETA`).
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
- This archive-inspection batch and the plan updates are currently uncommitted.

## Future archive extraction roadmap

This roadmap follows the archive-inspection commands (`archive list`,
`archive show`, `archive verify`, and `archive files`) without changing the
existing progress and TUI stage numbering above. The labels start at E2
because archive inspection is the first archive feature; E2–E5 are future
extraction and packaging work and are not part of the current
archive-inspection implementation.

### E2. Extract whole volumes and images from local snapshots

Goal:

- Recover one complete volume or image from a snapshot in the local store
  without restoring the full snapshot and without requiring a running Docker
  engine.

Recommended CLI:

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
- Refuse to overwrite an existing output unless an explicit `--force` flag
  is supplied. Write to a temporary sibling file and atomically rename it on
  success so cancellation or corruption cannot leave a plausible partial
  result at the requested path.
- Support `-o -` for stdout only when diagnostics and progress remain on
  stderr. Suppress terminal control sequences when stderr is redirected.
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
- Existing outputs are protected unless `--force` is set.
- Digest failure and cancellation remove temporary outputs.
- Stdout extraction contains only payload bytes; progress remains on stderr.
- Large objects are streamed with bounded memory and emit exact progress.

### E3. Extract individual files and directories from volumes

Goal:

- Extract a selected regular file or directory tree from a captured volume
  without restoring the volume or scanning unrelated snapshot resources.

Recommended CLI:

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
