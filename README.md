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

Prebuilt packages live on the [Releases page](../../releases) for Windows
amd64 and Apple-silicon macOS (the `dist/` directory is build output and is
not committed). Other platforms can build from source with `go build .`.

### Windows (amd64)

After cloning the repository, build and install from source with one command
from its root (Go must be on `PATH`):

```powershell
.\install.cmd
```

This creates the Windows release layout under `dist\windows-amd64`, then runs
the per-user installer. Pass `-NoPath` to skip the PATH change or
`-InstallDir <path>` to choose another location.

Alternatively, download and extract a prebuilt
`dockervc-<version>-windows-amd64.zip` from [Releases](../../releases). Open
PowerShell inside the extracted `windows-amd64` folder, then run:

```powershell
powershell -ExecutionPolicy Bypass -File .\install.ps1
```

The installer copies `dockervc.exe` to
`%LOCALAPPDATA%\Programs\dockervc` and adds that folder to your user PATH;
open a new terminal afterward. It is per-user and does not require
administrator privileges. Use `-NoPath` to skip the PATH change, or
`-InstallDir <path>` to choose another location.

Docker Desktop must be running in Linux containers mode. The Docker API's
Windows named-pipe transport is selected automatically; a standard
`DOCKER_HOST` override is also honored.

### macOS (Apple silicon)

Download the `darwin-arm64` archive from
[Releases](../../releases), then:

```sh
tar xzf dockervc-<version>-darwin-arm64.tar.gz
cd darwin-arm64
sudo ./install.sh          # or: ./install.sh --prefix ~/bin
```

> On an Intel Mac or a Linux box, build from source instead — `go build .`
> produces a binary for the machine you're on.

### Uninstall

On Windows, run the uninstaller from the extracted release folder. Pass the
same `-InstallDir` used during installation if you chose a custom location:

```powershell
powershell -ExecutionPolicy Bypass -File .\uninstall.ps1
# or: powershell -ExecutionPolicy Bypass -File .\uninstall.ps1 -InstallDir C:\Tools\dockervc
```

It removes `dockervc.exe` and its user `PATH` entry, then asks whether to
permanently delete the snapshot store. The safe default is to keep the store.
If you used `--store` without setting `DOCKERVC_HOME`, identify that custom
store with `-StoreDir <path>` so the uninstaller can offer to remove it.

On macOS, run the uninstaller from the same extracted release folder (or grab
`packaging/uninstall.sh` from the repo — it needs nothing next to it):

```sh
sudo ./uninstall.sh          # or: ./uninstall.sh --prefix ~/bin
```

It removes the `dockervc` executable(s) it finds — every copy on `PATH`,
plus the `/usr/local/bin` default — and prints where your snapshot store
lives (`~/.dockervc`, `/var/lib/dockervc`, or `$DOCKERVC_HOME`). The store
itself is left alone: it holds your snapshots, so deleting it stays a
decision you make by hand with `rm -rf`.

## Quick start

Don't want to memorize commands? Run `dockervc cli` for an interactive menu
of everything, or `dockervc man` for the full command reference.

`dockervc cli` is a full-screen TUI in VT-capable terminals, including
Windows Terminal: arrow keys or numbers
select commands, the argument each command takes is shown next to its name,
command output appears in a scrollable pane, and snapshot ids Tab-complete
(`show 16` + Tab); filesystem prompts also complete Windows paths such as
`C:\Users\...`. Quit with Ctrl-C, or press Esc twice. Piped input and consoles
without VT support fall back to a line-based menu.

```sh
dockervc init                     # one-time: create the snapshot store
dockervc snapshot -m "baseline"   # capture everything now
dockervc log                      # list snapshots
dockervc show snap-...            # inspect one snapshot
dockervc status                   # what changed since the last snapshot?
dockervc status --deep            # also hash and compare live volume files
dockervc doctor                   # instant health check (missing files, broken snapshots)
dockervc doctor --deep            # also re-hash every object (catches corruption)
dockervc doctor --repair          # interactively fix what it finds
dockervc delete snap-... [snap-...]  # drop one or more snapshots
dockervc prune                    # reclaim storage from unreferenced objects
```

Restore a snapshot (fully or in part):

```sh
dockervc rollback snap-... --dry-run     # print the restore plan, change nothing
dockervc rollback snap-... --all         # restore everything it captured
dockervc rollback snap-... --volumes demo-data   # just some volumes (or --containers/--images/--networks)
dockervc rollback snap-... --containers app --recreate-with-current-dependencies
dockervc rollback snap-... --containers app --reuse-existing-by-name
```

An ordinary rollback compares live state first: unchanged containers and
volumes are retained, while changed container configuration/images are
recreated and changed volume contents are replaced exactly. The
`--recreate-with-current-dependencies` strategy instead treats the snapshot as
a container configuration recipe: it recreates selected containers while
keeping the current same-name image, volume contents, and networks. The
`--reuse-existing-by-name` strategy is narrower and creates only missing
containers. Both name-based strategies abort if a required dependency is
missing, and `--all` means all snapshot containers rather than restoring
independent snapshot resources. The guided CLI offers all three strategies.

When an ordinary container or image rollback needs a captured image, dockervc
restores its original recorded image name whenever that name is free. If the
name currently points to a different image, dockervc reports both identities
before the safety checkpoint. Container restores also show the two
current-dependency strategies when all their named dependencies are present;
otherwise dockervc asks before moving the conflicting tag to the exact
snapshot image. The default answer is no. Replacing a tag does not delete the
previous image object.

Unless `--keep-current`, a pre-rollback checkpoint snapshot is taken first, so
the operation can itself be rolled back. Broken snapshots (missing object
files) are refused outright.

Move a snapshot to another machine (or off-site path) as one portable file:

```sh
dockervc export snap-...                  # writes exports/<snapshot-id>.dvca (folder created on demand)
dockervc export --latest -o /path/state.dvca
dockervc export latest --volume app_data # writes a standard volume tar
dockervc export latest --image postgres:17  # writes a Docker-save tar
dockervc export latest --volume app_data --path etc/nginx  # filtered tar
dockervc export latest --volume app_data --path etc/app.conf --raw  # one plain file
dockervc files latest app_data [prefix]   # browse a local snapshot's volume index
dockervc config set export.folder ~/backups   # default folder for exports (menus and bare command)
dockervc config unset export.folder           # back to exports/ in the current directory
dockervc archive list                         # quickly list exports in the configured folder
dockervc archive show state.dvca              # inspect contents without importing
dockervc archive verify state.dvca            # fully hash-check every archived object
dockervc archive files state.dvca demo-data   # list files captured for one volume
dockervc import state.dvca                # verify + add to this machine's store
dockervc import state.dvca --apply        # ... then roll this engine back to it (asks once)
```

The `.dvca` file is an uncompressed tar holding the manifest, every object
verbatim, and a GNU `sha256sum`-style checksums file — after `tar xf` you can
verify it with plain `shasum -a 256 -c checksums.sha256`, no dockervc needed.
Import re-hashes every object while streaming it in; a tampered or truncated
archive is rejected by name, and re-importing the same snapshot is a no-op.
Whole-snapshot exports remain importable `.dvca` archives. Selective exports
are ordinary tar files (or one plain file with `--raw`) and are intended for
recovery or use with tools such as `docker load`; they are not importable
snapshot bundles. Without `-o`, selective artifacts are organized below
`exports/<snapshot-id>/` and recorded in `export-index.json`; `archive list`
shows those indexed artifacts alongside `.dvca` files. In the full-screen
`dockervc cli`, file and directory export uses the captured volume index as a
filterable folder browser: Enter opens directories, “export this folder”
selects a subtree, and manual entry handles legacy indexes or empty folders.

`archive list` and `archive show` read only metadata, so browsing large
exports is fast. `archive verify` performs the deliberately slower full
structure and checksum pass. `archive files` authenticates and reads the
selected volume's stored file index; snapshots from before file indexing report
that no index is available. All four actions are also available through the
guided `dockervc cli` Archive menu.

Useful snapshot flags:

| Flag | Effect |
|---|---|
| `-m "msg"` | describe the snapshot |
| `--stop` | stop containers first for app-consistent volume data, restart after |
| `--only containers,volumes` | capture just some kinds |
| `--include-anonymous` | also capture anonymous volumes |
| `--include-bind-mounts` | also archive host bind-mount paths (run on the Docker host) |

## Progress, elapsed time, and ETA

Long-running snapshot, export, restore, and deep-status streams report their
current item, bytes, transfer rate, and elapsed time on stderr. Export and
restore know their stored-byte totals and show `ETA`; snapshot and live-volume
scans use the previous snapshot as a size baseline and mark estimates as
`~ETA`. When no credible total exists, dockervc shows bytes/rate/elapsed without
inventing an ETA.

Interactive terminals update progress in place. Redirected output contains
plain periodic lines with no ANSI control sequences. Pass the global
`--no-progress` flag to suppress progress entirely:

```sh
dockervc --no-progress snapshot -m "quiet backup"
```

The full-screen TUI continues to show its existing `running: ...` indicator;
live in-pane progress is deferred to a later TUI concurrency update.

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

$ dockervc status --deep --volumes demo-data
scanning volume demo-data: elapsed 00:00 · ETA calculating…
volumes:
  ~ demo-data (+1 created, ~2 modified, -1 deleted)

$ dockervc diff snap-A --files demo-data
volume demo-data, snap-A → live:

  M  log.txt      36.5 KiB
```

Snapshots taken before file indexing existed fall back to a plain
"content changed" note; take a fresh snapshot to start collecting indexes.
Plain `status` stays fast and compares volume names only. `status --deep`
mounts matching live volumes read-only, hashes their files, and discards the
temporary indexes without writing a snapshot or CAS objects. Use `--volumes`
to limit the scan. Like a normal snapshot without `--stop`, the result is
crash-consistent when applications write during the scan.

In `dockervc cli`, choose **Status**, enable the deep scan, then select one or
more volumes from the checklist (press `a` to select all).

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
| Rollback (full engine / granular entities) | ✅ working |
| Export to portable archive / import on another machine | ✅ working |
| Browse and verify exported archives without importing | ✅ working |

## Design

See [DESIGN.md](DESIGN.md) for the full architecture: content-addressed
storage, SQLite catalog, snapshot data model, and rollback semantics; the
export/import format (Step 4) is specified there too.
