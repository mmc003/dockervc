# dockervc — Local Version Control & Backup for Docker

> **Status: basic function prototype.** Core workflows (snapshot, log, show,
> diff, delete, doctor) work end-to-end, but this is not production-ready —
> expect rough edges and stubbed features (see the build-out roadmap in
> DESIGN.md). Do not trust it as your only backup yet.

`dockervc` takes git-like snapshots of your local Docker engine — containers
(including their filesystem changes), images, named volumes, and custom
networks — stores them **entirely on your machine**, and lets you inspect,
compare, and (in the current roadmap) roll back and migrate them.

100% local: no cloud accounts, no SaaS, no telemetry.

## Install

### Windows (primary target)

1. Download and extract the release `.zip` (pick `windows-amd64`, or
   `windows-arm64` for ARM devices).
2. Open PowerShell **inside the extracted folder** and run:

   ```powershell
   powershell -ExecutionPolicy Bypass -File .\install.ps1
   ```

   This copies `dockervc.exe` to `%LOCALAPPDATA%\Programs\dockervc` and puts
   it on your user PATH. Open a new terminal afterwards.

> Docker Desktop must be installed and running. On Windows the Docker API is
> reached over its named pipe — nothing to configure, `dockervc` finds it
> automatically (standard `DOCKER_HOST` overrides work too).

### Linux / macOS

Pick the archive matching your OS and CPU (`uname -m` prints `x86_64` →
amd64, `aarch64`/`arm64` → arm64):

```sh
tar xzf dockervc-<version>-<os>-<arch>.tar.gz
cd <os>-<arch>             # e.g. linux-amd64, darwin-arm64
sudo ./install.sh          # or: ./install.sh --prefix ~/bin
```

## Quick start

Don't want to memorize commands? Run `dockervc cli` for an interactive menu
of everything, or `dockervc man` for the full command reference.

`dockervc cli` is a full-screen TUI (unix terminals): arrow keys or numbers
select commands, the argument each command takes is shown next to its name,
command output appears in a scrollable pane, and snapshot ids Tab-complete
(`show 16` + Tab). Quit with Ctrl-C, or press Esc twice. Piped input and
Windows fall back to a line-based menu.

```sh
dockervc init                     # one-time: create the snapshot store
dockervc snapshot -m "baseline"   # capture everything now
dockervc log                      # list snapshots
dockervc show snap-...            # inspect one snapshot
dockervc status                   # what changed since the last snapshot?
dockervc doctor                   # instant health check (missing files, broken snapshots)
dockervc doctor --deep            # also re-hash every object (catches corruption)
dockervc doctor --repair          # interactively fix what it finds
dockervc delete snap-...          # drop a snapshot
dockervc prune                    # reclaim storage from unreferenced objects
```

Useful snapshot flags:

| Flag | Effect |
|---|---|
| `-m "msg"` | describe the snapshot |
| `--stop` | stop containers first for app-consistent volume data, restart after |
| `--only containers,volumes` | capture just some kinds |
| `--include-anonymous` | also capture anonymous volumes |
| `--include-bind-mounts` | also archive host bind-mount paths (run on the Docker host) |

Snapshots whose object files went missing (or, per `--deep`, no longer hash
right) are marked `✗ BROKEN` in `log`; `doctor --repair` offers to delete
them, asking before each destructive step.

## What changed inside a volume?

Every captured volume also gets a **file index** — path, size, mode, and a
SHA-256 of each file's content — built from the same tar stream that goes
into the store (no extra Docker I/O). `diff` uses it to show file-level
change counts, and `--files` lists the individual files, git-style:

```console
$ dockervc diff snap-A snap-B
volumes:
  ~ demo-data (+1 created, ~2 modified, -1 deleted)

$ dockervc diff snap-A snap-B --files demo-data
  A  created.txt       3 B
  M  log.txt      36.5 KiB
  M  notes.txt          3 B
  D  temp.txt           4 B
```

Snapshots taken before file indexing existed fall back to a plain
"content changed" note; take a fresh snapshot to start collecting indexes.

## Where data lives

| OS | Default store |
|---|---|
| Windows | `%ProgramData%\dockervc` |
| Linux | `/var/lib/dockervc` |
| macOS | `~/.dockervc` (or `/var/lib/dockervc` if you `sudo mkdir` it first) |
| Any | override with `DOCKERVC_HOME` or `--store <path>` |

Objects are content-addressed (SHA-256) and zstd-compressed: an image or
volume that hasn't changed between snapshots is stored once, referenced by
many snapshots — repeated snapshots are cheap.

## Status of the build-out

| Capability | Status |
|---|---|
| Snapshots (containers, images, volumes, networks) | ✅ working |
| History, inspection, drift status, diff | ✅ working |
| Integrity verification, prune/GC | ✅ working |
| Rollback (full engine / granular entities) | 🚧 Step 3 — stub command registered |
| Export to portable archive / import on another machine | 🚧 Step 4 — stub commands registered |

## Design

See [DESIGN.md](DESIGN.md) for the full architecture: content-addressed
storage, SQLite catalog, snapshot data model, and the planned rollback and
export/import formats.
