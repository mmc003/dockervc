# Windows amd64 port verification report

Date: 2026-09-15 (Asia/Shanghai)
Branch: `windows-port`
Host: Windows 64-bit, build 26200
Go: `go1.27.0 windows/amd64`
Docker: Docker Desktop 4.90.0, Linux engine 29.7.2

## Outcome

The Windows implementation in phases 1 through 3 is complete and the
Windows test suite is green. A static `windows/amd64` binary and release zip
were built with `CGO_ENABLED=0`. The installer passed both `-NoPath` and user
PATH flows in temporary install directories; the original user PATH was
restored exactly after the test.

The final verification pass deliberately did not repeat Docker-mutating
snapshot, rollback, or `import --apply` operations because another user
project shares this Docker Desktop engine. Earlier disposable `demo-*` run
artifacts were retained and inspected. All fresh tests on 2026-09-15 that
needed mutation used only throwaway dockervc stores under `%TEMP%`; they did
not alter Docker objects.

The intended release remains v0.6.0. Per the implementation brief, this
branch does not bump `Makefile`'s version; the maintainer performs that step
when cutting the release on macOS.

## Feature verdicts

| Area | Verdict | Evidence |
|---|---|---|
| Windows store lock | Pass | `LockFileEx`/`UnlockFileEx` implementation plus a Windows-only regression test: a second same-process open fails in 0.03 s with the clear lock message, and reopen succeeds after `Close`. |
| Windows free-space query | Pass, healthy-drive path | `GetDiskFreeSpaceEx` uses the caller-available first out-parameter. Multiple exports completed without a spurious capacity error. The nearly-full removable-drive case was not available. |
| Full-screen TUI gate | Pass | PTY run entered alt-screen TUI and exited cleanly; injected `enableVT=false` unit test selected the line menu. |
| Windows TUI VT/UTF-8 | Pass in PTY; manual terminal checks remain | Windows helper enables `ENABLE_VIRTUAL_TERMINAL_PROCESSING` and best-effort UTF-8 output. UTF-8 glyphs rendered in the PTY capture. Windows Terminal resize/Ctrl-C/dialog/scroll and physical legacy-conhost checks remain manual. |
| TUI completion | Pass | Live TUI completion expanded `C:\Us` to `C:\Users\` and displayed seven candidates; a unique snapshot prefix expanded to `snap-20260914-121631-1893`. Cross-platform path tests also pass. |
| Default store path | Pass | Pure resolver tests and Windows tests cover `DOCKERVC_HOME`, writable `%ProgramData%\dockervc`, and home fallback. A real `%ProgramData%` store was intentionally not created. |
| Windows installer | Pass | Both `-NoPath` and PATH modes installed and ran `dockervc 0.5.1 (windows/amd64)`; Docker Desktop was detected; the temporary PATH entry was observed and the original user PATH restored exactly. |
| Windows packaging | Pass | Zip contains `windows-amd64/dockervc.exe`, `install.ps1`, and `README.md`. SHA-256: `CB2B3F21B3E8257DEB1CE62B7933CC2F3855A3290802C0378F23995F6CFC7BE1`. |
| Windows docs | Pass | README includes Windows install, named-pipe transport, TUI behavior, and `%ProgramData%\dockervc`. |
| Settings defaults | Pass | `config unset zstd_level` now restores `3`; `export.folder` unset removes the optional value; unknown keys are rejected. |

## Findings register

| ID | Severity | Finding | Status |
|---|---|---|---|
| W-01 | Blocker | Windows build lacked store locking and free-space implementations. | Fixed. |
| W-02 | High | The full-screen TUI did not enable or require Windows VT output. | Fixed and covered by fallback test. |
| W-03 | Medium | Default Windows store resolution did not use `%ProgramData%\dockervc`. | Fixed and covered by resolver/Windows tests. |
| W-04 | Medium | `config unset zstd_level` deleted a required default row, so `config get zstd_level` failed instead of returning `3`. | Found during the live matrix, fixed, and covered by regression test. |
| W-05 | Medium | The checkout had no direct Windows test for same-process lock exclusion/release, despite that behavior being load-bearing. | Fixed with `TestWindowsStoreLockIsExclusiveAndReleasedOnClose`. |
| W-06 | Informational | A long live TUI completion initially appeared unchanged because the 80-column capture truncated the completed suffix. | Not a product bug; a shorter live Windows-path probe passed. |
| W-07 | Informational | The first maintenance rerun used the source store's checkpoint ID instead of the checkpoint archive's embedded ID. | Command failed atomically; rerun with IDs from `log` passed. |

## Verification matrix

| # | Area | Verdict | Actual result |
|---:|---|---|---|
| 1 | Build and smoke | Pass | Static package binary reported `dockervc 0.5.1 (windows/amd64)`; `man` exited 0 and emitted 77 lines. Version remains intentionally unbumped. |
| 2 | Store and lock | Pass with safe default-path substitution | Windows lock regression passed, including immediate conflict and release. `%ProgramData%` resolution was tested with an isolated temporary `ProgramData` value rather than writing a real system store. The retained `lock-check` snapshot is `snap-20260914-121054-027d`. |
| 3 | Snapshot/history | Pass from retained demo run plus fresh read checks | The retained store contains two valid snapshots. `log` and `show` passed. `diff` reported removed `demo-web`; `demo-data` showed 1 added, 2 modified, and 1 deleted file. The final pass did not take another engine snapshot. |
| 4 | Rollback | Pass from retained disposable-demo evidence; not reapplied | The retained pre-rollback checkpoint is `snap-20260914-121631-1893`. Docker contains `demo-web`, `demo-worker`, and `demo-sidecar` under deterministic `*-restored-from-snap-20260914-121054-027d` image tags, and restored `demo-data` content was readable. No rollback was repeated after the user identified another Docker project on the shared engine. |
| 5 | Export/import round trip | Pass except apply was not repeated | Nested `baseline.dvca` imported into a fresh store. A tampered archive was rejected with exit 1 before publication; `log` remained empty. The good archive imported with 15 verified objects, and re-import was a verified no-op. Earlier `import --apply` evidence includes the retained `pre-apply checkpoint` archive/store; apply was not repeated on the shared engine. |
| 6 | Health | Pass | `doctor --deep` re-hashed all objects cleanly. In a disposable store, one exact object was moved aside: `log` marked the snapshot `BROKEN`, doctor exited 1, `doctor --repair --yes` deleted the unrestorable snapshot and 15 rows, and the final deep check was healthy. |
| 7 | Maintenance | Pass | Two imported snapshots were deleted in one command; prune found 26 unreferenced objects and reclaimed 128.5 MiB; final deep doctor check was healthy. An intentional wrong-ID attempt failed atomically and pruned nothing. |
| 8 | Settings | Pass | Existing configured folder accepted a no-`-o` export and produced a 68,260,352-byte archive. Unsetting `export.folder` restored the unset state. Unsetting `zstd_level` returned it to `3`; unsetting `bogus` exited 1. |
| 9 | TUI | Partial | PTY full-screen entry/exit, UTF-8 rendering, unique snapshot completion, Windows path completion, piped line-menu fallback, and VT-disabled unit fallback passed. Manual Windows Terminal resize, Ctrl-C, dialog navigation, scroll-pane behavior, and a physical legacy-conhost session were not performed. |
| 10 | Free space | Partial | Healthy-drive exports passed without a false warning. No nearly-full USB/removable volume was available, so the real low-space rejection was not tested. |
| 11 | Suite | Pass | `go vet ./...` and uncached `go test -count=1 ./...` both passed on Windows after the final source change. |

## Commands and durable artifacts

- Final package: `dist/dockervc-0.5.1-windows-amd64.zip` (ignored build output).
- Retained original store: `%TEMP%\dvctest`.
- Retained import store: `%TEMP%\dvctest-import`.
- Round-trip fixtures: `%TEMP%\dockervc-windowsport\nested\baseline.dvca`,
  `baseline-tampered.dvca`, and `checkpoint.dvca`.
- Fresh isolated verification stores: `%TEMP%\dvctest-final-import` and
  `%TEMP%\dvctest-final-maint`.
- Final suite command: `go vet ./...` followed by
  `go test -count=1 ./...`.

## Not tested / maintainer hand-back

- No macOS regression run (`go vet ./... && go test ./...`) and no macOS
  `make dist`; the maintainer must run both before release.
- No Windows-to-macOS archive interchange, per scope.
- No Windows Containers mode, per scope.
- No nearly-full physical removable-drive export.
- No manual Windows Terminal resize/Ctrl-C/dialog/scroll session and no
  physical legacy-conhost session.
- Docker-mutating checks were not repeated during the final pass to avoid
  affecting the user's other Docker project. Only exact `demo-*` objects
  were inspected; no broad Docker prune, stop, remove, or rollback command
  was run.

The branch still needs commits with the required co-author trailer and a
push before hand-back. The maintainer should then review/merge it, run the
macOS checks, bump/cut v0.6.0, tag, and publish only with explicit approval.
