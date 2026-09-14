# WINDOWS-PLAN.md — build windows-amd64 support for dockervc from scratch

> **What this is:** a self-contained implementation brief. You are a coding
> agent on a Windows 10 22H2+/11 **amd64** machine with Docker Desktop, Go
> 1.26+ and git, in a fresh directory that contains only this file. Read all
> of it before writing code, then work top to bottom. Everything you need is
> here or in the files it points at inside the repository you will clone.
>
> **Background:** dockervc is a released, working tool on Apple-silicon macOS
> — `darwin-arm64` is the reference platform. A first Windows port shipped in
> v0.5.0 and turned out broken, so Windows support was **removed from the
> repository entirely** (build files, installer, release packaging, all of
> it). Your job is to build it back so `windows-amd64` is functionally
> identical to the mac version — implementing each piece per the specs in
> §4–§7 and proving parity with the matrix in §9.

## 0. Mission and definition of done

Every feature that works on darwin-arm64 works the same way on windows-amd64:

- all commands: `init`, `snapshot`, `log`, `show`, `diff`, `status`,
  `rollback` (+`--dry-run`/`--all`/granular flags/`--keep-current`),
  `export`, `import` (+`--apply`), `delete`, `prune`, `doctor`
  (+`--deep`/`--repair`), `config` (get/set/unset), `man`, `version`
- the **full-screen TUI** runs in Windows Terminal (not just the line menu)
- Tab completion works for snapshot ids **and Windows paths** (`C:\…`)
- archives exported on Windows import cleanly on a mac and vice versa
- `install.ps1` end-to-end install → PATH → everything above passes
- the full `go test ./...` suite passes **on Windows** (it already runs
  cross-platform; see §4.3)

You are done when §9's matrix passes with real recorded output and §10's
deliverables exist. Anything you could not test, say so plainly.

## 1. Ground rules

- **Work on a branch.** After cloning: `git checkout -b windows-port`. Commit
  early and often; never touch `main`; push the branch when finished.
- **Commit messages** end with:
  `Co-Authored-By: Claude Code <noreply@anthropic.com>`
- **Never mutate a real store.** Anything live runs against a throwaway
  store: `$env:DOCKERVC_HOME = "$env:TEMP\dvctest"` (session-scoped). The
  `--store <path>` flag exists for targeted cases.
- **No binaries in git.** `dist/`, `dockervc.exe` are gitignored — never
  commit them.
- **No CGO, ever.** SQLite is `modernc.org/sqlite` (pure Go);
  `CGO_ENABLED=0` static builds are load-bearing.
- **No shelling out** to docker/tar/shasum — Docker access is the official
> Go SDK (`github.com/docker/docker/client`), archives are in-process
  libraries. Keep it that way.
- **No new dependencies** unless truly unavoidable; `golang.org/x/sys` and
  `golang.org/x/term` are already in go.mod and are exactly what you need.
- **gofmt only files you author.** Some pre-existing files are deliberately
  not gofmt-clean; leave them.
- **Menus add prompts, not logic.** Interactive flows funnel into the real
  cobra commands; do not fork behavior per-OS beyond what this brief says.
- **Do not bump `Makefile` VERSION yourself.** Version bumps, tags and
  releases are cut by the maintainer on the mac after merge (this work ships
  as **v0.6.0** — platform support is a minor bump).

## 2. Set up (Go + git is the whole toolchain — there is no make on Windows)

```powershell
git clone https://github.com/mmc003/dockervc.git   # private — ask the user if auth fails
cd dockervc
git checkout -b windows-port
go vet ./...     # FAILS right now — on purpose; §3 explains
```

Dev-loop mapping:

| mac | Windows (you) |
|---|---|
| `make build` | `go build .` → `dockervc.exe` in the repo root |
| `make test` | `go vet ./... ; go test ./...` |
| one test, verbose | `go test ./internal/cli -run TestName -v` |
| run the binary | `.\dockervc.exe <cmd>` from the repo root |
| `make dist` / `install.sh` | never on Windows — release packaging is cut on the mac |

Terminal requirements: set **Windows Terminal** as your default terminal
(VT-capable — the TUI depends on it), and keep one plain `cmd.exe` window
around as the legacy-conhost fallback test case. Docker Desktop must be
installed and **running** (Linux containers mode); check `docker version`.

## 3. Where the build stands: broken on purpose

`go vet ./...` fails with undefined `lockFile`/`unlockFile`
(internal/store) and `freeSpace` (internal/cli). That is the removal: those
symbols live in build-tagged Windows files that were deleted along with
`packaging/install.ps1`, the Makefile's windows packaging, and the
`%ProgramData%` store-path logic. `go test ./...` on the mac is green — the
repo is healthy, it just has no Windows half.

**Order of work:** make it compile and the suite pass (§4), then the TUI
(§5), then Windows parity behaviors (§6). Verify as you go; do not batch
verification to the end.

## 4. Phase 1 — compile + green suite

### 4.1 The store lock — write `internal/store/lock_windows.go`

The store is single-writer: every opened store holds an exclusive lock on
`<store>/lock` until closed. The Unix half exists (`internal/store/
lock_unix.go`, build tag `//go:build !windows`, uses `flock` via
`golang.org/x/sys/unix`). Write the Windows half with
`//go:build windows`, same two functions, same signatures:

```go
func lockFile(f *os.File) error    // exclusive + non-blocking, whole file
func unlockFile(f *os.File) error  // releases it
```

Use `windows.LockFileEx` with `LOCKFILE_EXCLUSIVE_LOCK |
LOCKFILE_FAIL_IMMEDIATELY` over the whole-file range, and
`windows.UnlockFileEx` over the **identical** range (ranges must match
exactly). Sketch:

```go
func lockFile(f *os.File) error {
    ol := new(windows.Overlapped)
    return windows.LockFileEx(windows.Handle(f.Fd()),
        windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
        0, ^uint32(0), ^uint32(0), ol)
}
```

Contract you must hold (the suite tests all of this):

- A second `store.Open` on a locked store fails **immediately** with a
  clear error, and closes everything it opened on the way out
  (`openLocked` in `internal/store/store.go` already structures this —
  your functions slot in; read it first).
- `Store.Close()` closes the DB, then unlocks, then closes the lock file
  handle, all unconditionally. Closing the handle releases the lock even
  if the unlock call failed — keep that ordering.
- **Windows gotcha the first port learned the hard way:** `LockFileEx`
  byte-range locks conflict **even within one process** across handles
  (Darwin's flock forgives a same-process second lock, which is why a
  leaked-handle bug was invisible on the mac). Any store handle you fail
  to close poisons every later command in that process AND keeps the
  sqlite file undeletable (Windows cannot delete open files).
  `TestRunArgsClosesStoreOnError` in `internal/cli/runargs_test.go`
  guards exactly this — read its comment.

### 4.2 Free-space check — write `internal/cli/freespace_windows.go`

Export's pre-flight asks "is there room for this archive?" The Unix half
(`internal/cli/freespace_unix.go`, `//go:build !windows`) returns
`(availableToUser uint64, ok bool)` via `Statfs`. Windows half, same
signature, via `GetDiskFreeSpaceExW` — the caller-available figure is the
**first** out-param:

```go
//go:build windows

package cli

import "golang.org/x/sys/windows"

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

Verify: `export` prints no spurious space error on a healthy drive.

### 4.3 Make the suite green on Windows

With §4.1–§4.2 in, `go vet ./...` compiles and `go test ./...` should pass
with **zero** modifications — the suite is cross-platform by design. The
helpers that make that true already exist; know them, don't rewrite them:

- `setHomeEnv` (internal/cli/tui_path_test.go) stubs the home dir via both
  `HOME` and `USERPROFILE` — `os.UserHomeDir` reads USERPROFILE on Windows,
  HOME on Unix.
- `TestSplitPathSep` exercises path-completion splitting for both separator
  styles on every platform.
- The CLI tests run the real cobra tree against temp stores via
  `DOCKERVC_HOME`; on Windows they will catch any handle leak as a cleanup
  failure ("being used by another process") — treat those as real bugs,
  never as flakiness.

## 5. Phase 2 — the full-screen TUI on Windows

The TUI (`internal/cli/tui.go`) is pure ANSI: alt-screen `\x1b[?1049h`,
cursor hide, reverse video, `\x1b[H` home, `\x1b[K` erase. `openMenu`
(tui.go) currently runs the TUI whenever stdin+stdout are terminals,
falling back to `RunInteractive()` (line menu) otherwise. On Windows two
things are missing:

1. **VT output is not enabled.** Nothing turns on
   `ENABLE_VIRTUAL_TERMINAL_PROCESSING` for stdout, so ANSI sequences print
   literally in a legacy console. Add a build-tagged helper pair:
   `internal/cli/vt_windows.go` (`//go:build windows`) calling
   `SetConsoleMode` on `os.Stdout`'s handle to OR in the flag and reporting
   success, and `internal/cli/vt_other.go` (`//go:build !windows`)
   returning `true` (Unix terminals are always VT). Make it a package var
   (`var enableVT = enableVTImpl` or similar) so a test can force it false.
2. **The gate must require it.** `openMenu` becomes:
   `if term.IsTerminal(in) && term.IsTerminal(out) && enableVT() { return runTUI() }`
   — otherwise the line menu, exactly as piped input gets today. Keep
   `runTUI`'s existing fallback if `term.MakeRaw` fails.

Also best-effort at TUI startup: `SetConsoleOutputCP(65001)` so the UTF-8
glyphs (✓ — · …) render; never fail startup over it, and **do not**
transliterate the glyphs — functional parity means UTF-8 stays.

Input needs no new work: `term.MakeRaw` (x/term) already sets
`ENABLE_VIRTUAL_TERMINAL_INPUT` on Windows, so arrow keys arrive as the
same `ESC [ A/B/C/D` sequences `readKey` parses. Verify, don't rewrite.

Check on your machine: resize handling (`term.GetSize` in `refresh` —
confirm it tracks Windows Terminal resizes), Ctrl-C (arrives as `0x03`
with processed input off; `keyCtrlC` already quits), Esc-Esc quit prompt,
dialog navigation, the scrollable output pane, and that `echo 1 |
.\dockervc.exe cli` still gets the line menu, as does a plain cmd.exe
window if VT enable failed.

Add one test: `openMenu`'s gate falls back to the line menu when the
enableVT var is false (that's why it's injectable).

## 6. Phase 3 — Windows parity behaviors

### 6.1 Default store location — `%ProgramData%`

`store.DefaultPath()` (internal/store/store.go) currently resolves
`$DOCKERVC_HOME` → `/var/lib/dockervc` if usable → `~/.dockervc`. Windows
order: `$DOCKERVC_HOME` → `%ProgramData%\dockervc` when creatable and
writable → `~/.dockervc`. Reuse the existing `isUsableDir` probe
(mkdir + write a `.probe` file). `%ProgramData%` via `os.Getenv`, falling
back to `C:\ProgramData`. Guard with `runtime.GOOS` so the mac path is
byte-identical today; add a table test for the pure resolution logic.

### 6.2 Installer — write `packaging/install.ps1`

Per-user install, deliberately admin-free:

- params: `-InstallDir` (default `$env:LOCALAPPDATA\Programs\dockervc`),
  `-NoPath`
- expects `dockervc.exe` next to the script (`$PSScriptRoot`) — that's how
  the release zip lays it out
- copies the exe into InstallDir, adds InstallDir to the **user** PATH if
  absent (and tells the user to open a new terminal), runs
  `dockervc.exe version` to verify, and checks `docker version` with a
  friendly warning when Docker Desktop isn't running
- `$ErrorActionPreference = "Stop"`; usage header comments like
  packaging/install.sh has
- test it end-to-end from an unzipped-style layout, and with `-NoPath`

### 6.3 Packaging + docs (verified on Windows, executed on the mac)

- Makefile: restore a `dist/windows-amd64` target
  (`CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -ldflags "$(LDFLAGS)"
  -o dist/windows-amd64/dockervc.exe .`) and a zip packaging step
  (`cp packaging/install.ps1 README.md dist/windows-amd64/` then zip the
  folder as `dockervc-$(VERSION)-windows-amd64.zip`). You can't run make;
  verify the equivalent `go build` command works, and that the zip's
  layout matches what install.ps1 expects.
- README.md: restore a Windows install section mirroring the macOS one
  (download zip → extract → `powershell -ExecutionPolicy Bypass -File
  .\install.ps1`), note the named-pipe Docker transport is automatic, and
  add `%ProgramData%\dockervc` back to the store-location table.

## 7. Known Windows facts — already handled, verify don't rewrite

These exist in cross-platform code today; your job is to confirm they hold
live on Windows:

- **Path completion separators.** `splitPathToken`/`splitPathSep`
  (internal/cli/tui_path.go) splits on `/` plus `\` on Windows, and bare
  `C:` / `C:\` list the drive root. Directory completions carry the OS
  separator. Covered by `TestSplitPathSep`; verify live in the TUI with a
  real `C:\Users\…` path.
- **Docker transport.** `client.FromEnv` only — defaults to
  `npipe:////./pipe/docker_engine` on Windows; `DOCKER_HOST` honored.
  Nothing to write; verify against a running Docker Desktop.
- **Archive byte-parity.** `.dvca` tar entry names are forward-slash string
  concatenation (`objectsPrefix + hash`), never `filepath.Join` — so a
  Windows-built archive is structurally identical to a mac's. Never
  regress this; §9 item 5 is the proof.
- **Restore tags.** Containers restore under deterministic tags
  `<name>-restored-from-<snapID>` — no Windows angle, but they appear in
  every rollback verification.

## 8. Demo engine bootstrap

Run once in PowerShell with Docker Desktop up — disposable containers,
data in volumes, mirroring the mac reference engine, plus the network §9
wants:

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

Wipe it later with `docker rm -f demo-web demo-worker demo-sidecar; docker
volume rm demo-data demo-worker-data; docker network rm demo-net`.

## 9. Verification matrix (record real output for each)

Setup: fresh store in `$env:TEMP\dvctest` via `$env:DOCKERVC_HOME`, and
the §8 engine.

1. **Build & smoke:** `go build .` → `.\dockervc.exe version`, `man`
2. **Store:** `init` → confirm `%ProgramData%\dockervc` (or home fallback)
   → **lock check:** run `.\dockervc.exe snapshot` and, while it runs,
   `.\dockervc.exe log` in a second terminal → the second must refuse with
   the lock message, not corrupt
3. **Snapshot / history:** `snapshot -m` (full + `--only containers,volumes`
   + `--stop`), `log`, `show`, `status`, `diff` (+`--files demo-data`)
4. **Rollback:** mutate a volume (`docker run --rm -v demo-data:/data busybox
   sh -c "echo mutated > /data/notes.txt"`), delete demo-web, then
   `rollback <snap> --all --dry-run` → apply → volume byte-restored,
   demo-web recreated; granular `--volumes demo-data` touches nothing else;
   `--keep-current` skips the checkpoint; re-run → skips (idempotent)
5. **Export/import cross-platform (the parity keystone):** `export` to a
   `C:\`-nested `-o` path → get the `.dvca` to the maintainer's mac
   (ask the user) and have it imported there; then the reverse direction
   (mac-exported archive → Windows `import`, then `import --apply`).
   Tamper one byte in a copy → import must reject atomically; re-import →
   verified no-op
6. **Health:** `doctor` clean, `--deep`; move one object file aside →
   broken marker in `log` → `doctor --repair` flow
7. **Maintenance:** multi-`delete`, `prune` reclaim count
8. **Settings:** `config set export.folder C:\Users\...\backups` → bare
   `export` honors it → `config unset export.folder` → back to `exports\`;
   `config unset bogus` refused; `config unset zstd_level` → 3
9. **TUI:** `.\dockervc.exe cli` in Windows Terminal — menus, dialogs,
   snapshot-id Tab completion, **path Tab completion of `C:\` paths**,
   scrollable output, Esc-Esc quit; piped input → line menu; legacy
   conhost → line menu (the §5 gate)
10. **Free space:** export to a nearly-full volume (small USB drive) →
    pre-flight error names the real free space
11. **Suite:** `go vet ./... ; go test ./...` green on Windows. Mac-side
    regression (`go vet ./... && go test ./...`, `make dist`) is run by
    the maintainer — request it before hand-back.

## 10. Deliverables and hand-back

1. Branch `windows-port` pushed, with code for §4–§6 (+ anything §9
   surfaced), tests included
2. Windows-side `go vet ./... && go test ./...` green
3. `WINDOWSREPORT.md` in the repo root: per-feature verdict table, findings
   register (severity + fix status), the §9 matrix with actual results, and
   a plain statement of what was **not** tested
4. Hand back to the maintainer (the user): they merge to main, then cut
   **v0.6.0** from the mac — `make dist` (which by then packages both
   platforms again), tag, GitHub release — on their explicit approval only

## 11. Out of scope

- winget/choco/scoop packaging, code signing, SmartScreen appeasement
- Windows Containers mode (Docker Desktop Linux containers only, matching
  the mac)
- cygwin/msys terminal quirks beyond "line menu works there"
- Any behavior change on darwin/linux beyond `runtime.GOOS`-guarded
  additions, which must be no-ops off Windows
