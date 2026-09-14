# WINDOWS-PLAN.md — Windows amd64 full-implementation brief

> **What this is:** a self-contained prompt/brief for making the
> `windows-amd64` build of dockervc **functionally identical to the
> `darwin-arm64` build**, which is the reference platform (fully live-tested).
> Hand it to an agent or engineer who has a Windows 10 22H2+/11 **amd64**
> machine with Docker Desktop. Everything needed to do the work is in this
> document or the files it points at.
>
> Baseline at time of writing: **v0.5.0** (tagged, released). The windows
> packages cross-compile cleanly and passed a static audit (§3), but the
> binary has **never been executed on Windows**. That is the gap this brief
> closes.

---

## 0. Mission and definition of done

Every feature that works on darwin-arm64 works the same way on windows-amd64:

- all commands: `init`, `snapshot`, `log`, `show`, `diff`, `status`,
  `rollback` (+`--dry-run`/`--all`/granular flags/`--keep-current`),
  `export`, `import` (+`--apply`), `delete`, `prune`, `doctor`
  (+`--deep`/`--repair`), `config` (get/set/unset), `man`, `version`
- the **full-screen TUI** runs in Windows Terminal (not just the line menu)
- Tab completion works for snapshot ids **and Windows paths** (`C:\…`)
- archives exported on Windows import cleanly on darwin-arm64 and vice versa
- `install.ps1` end-to-end install → PATH → everything above passes
- no regression on darwin/linux: `go vet ./... && go test ./...` green,
  `make dist` still builds both release packages (darwin-arm64,
  windows-amd64)

## 1. Context to read first (in order)

1. `README.md` — user-facing behavior, install flows, store locations
2. `DESIGN.md` — architecture: CAS store, SQLite catalog, snapshot model,
   rollback semantics, `.dvca` archive format
3. `HANDOFF.md` — ground truth for what exists in code (§4 rollback, §5
   export/import)
4. `CHANGELOG.md` — what v0.5.0 shipped

Key architecture facts that constrain the work:

- **Menus add prompts, not logic.** Every interactive flow funnels into the
  real cobra commands (`runArgs`/`t.execTUI`). Do not fork behavior per-OS
  beyond what this brief specifies.
- **The store is single-writer** via a lock file (`internal/store/lock_*.go`).
- **Everything is in-process**: no `exec.Command`, no shelling out to tar,
  shasum, or docker CLI. Keep it that way.
- Pure-Go dependencies only (`modernc.org/sqlite`, `klauspost/compress`
  zstd) — `CGO_ENABLED=0` builds are load-bearing for cross-compilation.

## 2. Environment prerequisites

- Windows 10 22H2 or Windows 11, amd64
- Docker Desktop installed and **running** (Linux containers mode)
- Go 1.26+ (`go version`), git, PowerShell 5.1+
- Build: `go build .` on the Windows box, or test the shipped
  `dist/dockervc-0.5.0-windows-amd64.zip` package first to reproduce the
  pre-fix state
- A real terminal for TUI work: **Windows Terminal** (VT-capable). Also keep
  a legacy `conhost` (plain cmd.exe window) around for fallback testing.

### 2.1 Dev loop on Windows (no make needed)

The Makefile is the *mac* side of the workflow: `make dist` cross-compiles
and packages releases (it shells out to tar/zip/chmod, so it only runs on
the mac), and `make test` is a thin wrapper. Nothing in it is required on
the Windows box — Go and git are the whole toolchain:

| mac | Windows |
|---|---|
| `make build` | `go build .` → `dockervc.exe` in the repo root |
| `make test` | `go vet ./... ; go test ./...` |
| one test, verbose | `go test ./internal/cli -run TestName -v` |
| run the binary | `.\dockervc.exe <cmd>` from the repo root |
| `make dist` / `install.sh` | never on Windows — packages and releases are cut on the mac |

Throwaway store for anything live (session-scoped, per §6's rules):
`$env:DOCKERVC_HOME = "$env:TEMP\dvctest"`.

### 2.2 Demo engine bootstrap (mirrors the mac reference engine)

Run once in PowerShell with Docker Desktop up — recreates the disposable
containers + volumes the mac dev environment uses, plus the custom network
§5's matrix wants. Containers stay stateless; data lives in the volumes:

```powershell
docker network create demo-net
docker volume create demo-data
docker volume create demo-worker-data
docker run --rm -v demo-data:/data busybox sh -c "echo 'demo site v1' > /data/index.html; echo 42 > /data/counter.txt; mkdir -p /data/logs; echo app > /data/logs/app.log"
docker run --rm -v demo-worker-data:/data busybox sh -c "echo seed > /data/notes.txt"
docker run -d --name demo-web --network demo-net -v demo-data:/usr/share/nginx/html:ro nginx:alpine
docker run -d --name demo-worker -v demo-worker-data:/data busybox sh -c "while true; do date >> /data/heart.txt; sleep 30; done"
docker run -d --name demo-sidecar alpine sleep 1d
docker stop demo-sidecar
```

`demo-web` and `demo-worker` run (one with a web root, one heartbeating into
its volume); `demo-sidecar` is the stopped stateless one. Wipe it all with:
`docker rm -f demo-web demo-worker demo-sidecar; docker volume rm demo-data
demo-worker-data; docker network rm demo-net`.

## 3. What the 2026-09-12 audit already established (don't redo, do re-verify)

| Area | State | Where |
|---|---|---|
| Docker transport | `client.FromEnv` only → defaults to `npipe:////./pipe/docker_engine` on Windows; `DOCKER_HOST` honored | `internal/dockerapi/client.go:26` |
| Store lock | `LockFileEx` exclusive + `FAIL_IMMEDIATELY`, whole-file range — flock equivalent | `internal/store/lock_windows.go` |
| Store path | `%ProgramData%\dockervc` when creatable/writable (probe-written), else `~/.dockervc` | `internal/store/store.go:41` |
| `.dvca` format parity | tar entry names are forward-slash string concat (`objectsPrefix + hash`), never `filepath.Join` → Windows archives structurally identical | `internal/portable/write.go` |
| TUI gate | `runtime.GOOS != "windows" && IsTerminal(in) && IsTerminal(out)` → Windows always gets the line menu today | `internal/cli/tui.go:27` |
| No ANSI outside TUI | grep-verified zero escape sequences in non-TUI output | `internal/cli/*` |
| install.ps1 | copy to `%LOCALAPPDATA%\Programs\dockervc`, user PATH, `version` verify, docker check | `packaging/install.ps1` |

Known gaps the audit found (these become work items):

1. `freeSpace()` on Windows is a no-op stub → export skips the disk-space
   pre-flight (`internal/cli/freespace_windows.go`)
2. TUI never runs on Windows (line menu only)
3. Path completion splits tokens on `/` only — Windows `\`-style paths won't
   complete
4. UTF-8 glyphs (`✗ — · …`) may garble in legacy conhost codepage 437

## 4. Work items

### G1 — Enable the full-screen TUI on Windows

The TUI is pure ANSI: alt-screen (`\x1b[?1049h`), cursor hide, reverse video,
`\x1b[H` home, `\x1b[K` erase (see `runTUI`, `internal/cli/tui.go:190`, and
`render`, `tui.go:1038`). On Windows this needs virtual-terminal mode:

- **Input:** `term.MakeRaw(fd)` (already called) — x/term's Windows
  implementation sets `ENABLE_VIRTUAL_TERMINAL_INPUT` and disables
  processed/line/echo input, so arrow keys arrive as the same `ESC [ A/B/C/D`
  sequences `readKey`/`parseKey` already parse. Verify, don't rewrite.
- **Output:** nothing enables `ENABLE_VIRTUAL_TERMINAL_PROCESSING` on the
  stdout handle today. Add a small build-tagged helper
  (`internal/cli/vt_windows.go`, with a `//go:build windows` counterpart
  returning true elsewhere) that calls `SetConsoleMode` on `os.Stdout` to OR
  in `ENABLE_VIRTUAL_TERMINAL_PROCESSING`, and reports whether it succeeded.
- **Gate:** change `openMenu` (`internal/cli/tui.go:24`) to attempt the TUI
  on Windows **only when** stdin+stdout are a terminal **and** VT processing
  could be enabled; otherwise fall back to `RunInteractive()` (the line
  menu), exactly as piped input does today. Concretely:
  `if term.IsTerminal(in) && term.IsTerminal(out) && enableVT() { return runTUI() }`
  — drop the `runtime.GOOS != "windows"` clause, keep `runTUI`'s existing
  MakeRaw-failure fallback.
- **Check on the Windows box:** resize handling (`term.GetSize` is called in
  `refresh`/render — confirm it tracks Windows Terminal resizes), Ctrl-C
  (arrives as `0x03` with processed input off — `keyCtrlC` already quits),
  Esc-Esc quit prompt, dialog navigation, scrollable output pane.
- **Test updates:** the TUI tests construct `&tui{}` directly and are
  OS-independent; add one test that `openMenu`'s gate falls back to the line
  menu when `enableVT()` is false (inject the helper as a package var so the
  test can force it).

### G2 — Path completion for Windows paths

`internal/cli/tui_path.go`:

- `splitPathToken` (`tui_path.go:48`) splits on `/` only via
  `strings.LastIndexByte(tok, '/')`. A user typing `C:\Users\me\Des` gets
  dir `"."` and the whole string as prefix — completion silently lists the
  cwd. Fix: also treat `\` as a separator **on Windows**
  (`runtime.GOOS == "windows"` guard so darwin behavior is untouched —
  backslash is a legal filename char on Unix).
- Directory candidates get a trailing `/` appended after `filepath.Join`
  (`pathMatches`, `tui_path.go:39`). On Windows `filepath.Join` yields `\`
  separators; appending `/` gives mixed separators (`C:\exports/sub/`). Go's
  filepath/os accept mixed separators, but normalize anyway: complete with
  the OS separator (`string(filepath.Separator)`) so what lands in the input
  line is consistent, and make sure a completed `\`-terminated token
  re-splits correctly next Tab (covered by the G1 change to
  `splitPathToken`).
- `displayDir` (`tui_path.go:142`) shortens under home with
  `home + string(filepath.Separator)` — already separator-correct. Verify
  `~/…` display on Windows.
- Drive roots: `C:` (no slash) and `C:\` must list sensibly;
  `splitPathToken`'s `case 0` ("/" root) logic needs a Windows analog for
  `C:\` (dir `C:\`, empty prefix). Consider that `C:` alone means the *current directory
  of drive C* to Windows APIs — simplest correct move: treat a trailing
  `C:` token's dir as `C:\` for listing purposes, and document it.
- Update/extend `tui_path_test.go` with table cases for both separators
  (tests run on all platforms; use `runtime.GOOS` guards or test the pure
  split logic with injected separator behavior).

### G3 — Real free-space check on Windows

Replace the stub in `internal/cli/freespace_windows.go` with
`GetDiskFreeSpaceExW` from `golang.org/x/sys/windows` (already a dependency):

```go
func freeSpace(dir string) (uint64, bool) {
    var avail, total, free uint64
    p, err := windows.UTF16PtrFromString(dir)
    if err != nil { return 0, false }
    if err := windows.GetDiskFreeSpaceEx(p, &avail, &total, &free); err != nil {
        return 0, false
    }
    return avail, true
}
```

Match `freespace_unix.go`'s contract: `(available-to-user, ok)` — that's
`lpFreeBytesAvailableToCaller`, the third-argument **first** out-param.
Verify with: `export` prints no spurious space error on a healthy drive, and
the pre-flight fires on a nearly-full drive (a small USB/quota'd volume is
the easy test rig).

### G4 — UTF-8 console output (best-effort)

`✗ BROKEN` (log), `—`, `·`, `…` notes: fine in Windows Terminal, mojibake in
legacy conhost (codepage 437). In the same `vt_windows.go` helper (or beside
it), best-effort call `SetConsoleOutputCP(65001)` at TUI startup; never fail
startup over it. Do **not** transliterate the glyphs — functional parity with
darwin means UTF-8 stays. If VT enable fails on a legacy console the line
menu is used anyway (G1), where the odd glyph is cosmetic only.

### G5 — Live verification and fix what surfaces

Execute §5's matrix on real Windows + Docker Desktop with throwaway stores
(`$env:DOCKERVC_HOME = $env:TEMP + "\dvctest"`). Fix anything that fails;
record it in the report. Expect the plausible surprises to be: ProgramData
permissions when installed per-user, Docker Desktop pipe availability when
the engine is still starting (error message quality), and antivirus
sensitivity to a fresh unsigned exe writing to ProgramData.

## 5. Verification matrix (mirror of the darwin-arm64 testing)

Setup: fresh store in `$env:TEMP\dvctest`; demo engine entities to create
and later delete: one running container, one stopped container, a named
volume with known files, a custom network.

1. **Install:** `install.ps1` → new terminal → `dockervc version`,
   `dockervc man`
2. **Store:** `init` → confirm `C:\ProgramData\dockervc` (or `~/.dockervc`
   fallback) → **lock check:** start `dockervc snapshot` of something slow
   and run `dockervc log` concurrently → second must refuse with the lock
   message, not corrupt
3. **Snapshot / history:** `snapshot -m` (full + `--only containers,volumes`
   + `--stop`), `log`, `show`, `status`, `diff` (+`--files <vol>`)
4. **Rollback:** change volume contents + container state → `rollback --all`;
   byte-exact volume check; granular `--volumes`; `--dry-run`; a second
   rollback to roll back the checkpoint; broken-snapshot refusal (move one
   object file aside temporarily, like the darwin test did)
5. **Export/import:** `export` (default `exports\` folder created on demand;
   `-o` to a `C:\`-nested path) → copy the `.dvca` to a darwin-arm64 mac →
   `dockervc import` there → verify + `show` → **cross-platform parity
   proven**; then the reverse direction (mac-exported archive → Windows
   import, `--apply` rollback chain). Tamper a byte in a copy → import must
   reject atomically. Re-import → verified no-op.
6. **Health:** `doctor` (clean), `--deep`, object-file corruption → broken
   marker in `log` → `doctor --repair` interactive flow
7. **Maintenance:** multi-`delete`, `prune` reclaim count
8. **Settings:** `config set export.folder C:\Users\...\backups` → bare
   `export` honors it → `config unset export.folder` → back to `exports\`;
   `config unset bogus` refused; `config unset zstd_level` → 3
9. **TUI:** `dockervc cli` in Windows Terminal — menus, dialogs, snapshot-id
   Tab completion, **path Tab completion of `C:\`-style paths** (after G2),
   scrollable output, Esc-Esc quit; piped `echo 1 | dockervc cli` still gets
   the line menu; legacy conhost gets the line menu (after G1 gate)
10. **Free space:** export to a nearly-full volume → pre-flight error names
    real free space (after G3)
11. **Regressions on the reference platform:** on the darwin-arm64 mac:
    `go vet ./... && go test ./...` green, `make dist` builds both release
    packages, and a spot-check
    of TUI + export/import

## 6. Constraints and conventions

- **Never mutate a real store.** All testing goes through throwaway
  `DOCKERVC_HOME` directories. `--store <path>` exists for targeted cases.
- **No binaries in git.** `dist/`, the root `dockervc`/`dockervc.exe` are
  gitignored; never commit them.
- **gofmt only files you author.** `doctor.go`, `maintenance.go`,
  `snapshot.go`, `status.go` are pre-existing non-gofmt — leave them.
- **Version policy:** bump `Makefile` `VERSION` only for what ships
  (patch = fixes, minor = features); every bump ships as a commit naming the
  version + push; tag + `gh release` with all 6 packages only on explicit
  user approval. Suggested label for this work when approved: **v0.5.1**
  (fixes + platform parity, no new user-facing feature).
- **Keep the line-menu fallback** — piped input and non-VT consoles depend on
  it; it is also the Windows CI-friendly path.
- **No new dependencies** unless unavoidable; `golang.org/x/sys/windows` and
  `golang.org/x/term` are already in go.mod.
- Commit messages end with:
  `Co-Authored-By: Claude Code <noreply@anthropic.com>`

## 7. Deliverables

1. Code changes for G1–G4 (+ anything G5 surfaced), with tests
2. Windows-side `go vet ./... && go test ./...` green, and darwin-side green
3. `make dist` builds both release packages (built on the mac or any *nix
   box; Windows packaging is
   zip via the Makefile)
4. A `WINDOWSREPORT.md`: per-feature verdict table, findings register
   (severity + fix status), and the §5 matrix with actual results. State
   plainly what was **not** tested.

## 8. Out of scope

- winget/choco/scoop packaging, code signing, SmartScreen appeasement
- Windows Containers mode (Docker Desktop Linux containers only, matching
   darwin behavior)
- cygwin/msys terminal quirks beyond "line menu works there"
- Any behavior change on darwin/linux other than the shared `splitPathToken`
   Windows guard, which must be a no-op off Windows
