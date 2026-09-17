# Active Implementation Plan — Deep Status and Progress/ETA

Last updated: 2026-09-17

This file is a recovery checkpoint for the in-progress v0.7.0 work. It is
intentionally detailed enough to resume after an interrupted session.

## Working-tree context

- Branch: `windows-port`
- Last pushed commit before this work: `1181aab` (`feat: show snapshot storage sharing`)
- The deep-status and progress changes described below are currently
  uncommitted unless a later checkpoint says otherwise.
- Preserve all existing snapshot-storage-accounting changes.

## Completed: snapshot storage accounting

- `show <snapshot>` reports capture counts plus referenced, exclusive, and
  shared object-store bytes/counts.
- Store query deduplicates repeated manifest references and calculates current
  cross-snapshot sharing.
- Version was raised from 0.6.0 to 0.7.0 and documented.
- Tests passed and commit `1181aab` was pushed.

## Completed locally: deep live-volume status

Implemented but not yet committed at the time this checkpoint was written:

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

## Implemented locally: reusable progress and ETA system

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

### Current code checkpoint

The following files were just added/modified and must be compiled and tested
before further integration:

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

The core, renderer, and deterministic tests now exist. Export, restore,
snapshot, and deep status have been connected to the reporter. Final
`go vet ./...` and `go test ./...` both pass. A version-stamped Windows build
reports `dockervc 0.7.0 (windows/amd64)` and exposes the new global
`--no-progress` flag plus the deep-status flags.

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
