# Changelog

User-facing changes per version. Format loosely follows
[Keep a Changelog](https://keepachangelog.com/); versions match the
[GitHub releases](https://github.com/mmc003/dockervc/releases).

## v0.6.0 — 2026-09-16

### Windows uninstaller

- Windows release ZIPs now include `uninstall.ps1`. It removes the installed
  executable and its user `PATH` entry, then asks whether to permanently
  delete the snapshot store; declining or pressing Enter keeps snapshot data.
- Custom install and store locations can be selected with `-InstallDir` and
  `-StoreDir`.
- `make clean` now uses PowerShell on Windows and removes the Windows
  executable as well as the `dist` directory.

## v0.5.1 — 2026-09-14

### Uninstaller

- Release tarballs now ship `uninstall.sh` next to `install.sh` (also in
  `packaging/`; it runs from anywhere). It removes the `dockervc`
  executable(s) — every copy found on `PATH` plus the `/usr/local/bin`
  default, or just a `--prefix` install — asking you to re-run with `sudo`
  when a binary needs root to remove.
- The snapshot store is intentionally kept: snapshots are your data, so the
  script instead prints each store it finds (`$DOCKERVC_HOME`,
  `/var/lib/dockervc`, `~/.dockervc` — the invoking user's home too, when
  run under `sudo`), its size, and the exact `rm -rf` to run by hand.

## v0.5.0 — 2026-09-12

Everything since v0.4.3. Headlines: snapshots can now be **rolled back**,
**moved between machines** as one file, and driven entirely from the
**interactive menus**.

### Rollback — restore engine state from a snapshot

- `dockervc rollback <snap> --all` restores everything the snapshot captured;
  granular restores via `--containers`, `--volumes`, `--images`, `--networks`
  (comma-separated lists). A container selection carries its image, mounted
  volumes and networks along.
- `--dry-run` prints the restore plan and changes nothing.
- A pre-rollback checkpoint is taken first, so a rollback can itself be rolled
  back; `--keep-current` skips it.
- Volume contents are replaced byte-exact (files created after the snapshot
  don't survive); existing networks and images are reused, never deleted.
- Broken snapshots (missing object files) are refused outright.

### Export / import — move a snapshot as one portable file

- `dockervc export <snap>` writes `exports/<snapshot-id>.dvca` in the current
  directory (folder created on demand); `-o` puts it anywhere, `--latest`
  picks the newest snapshot.
- The `.dvca` file is an uncompressed tar holding the manifest, every object
  verbatim, and GNU `sha256sum`-style checksums — after `tar xf`, a plain
  `shasum -a 256 -c checksums.sha256` verifies it with no dockervc installed.
- `dockervc import <file.dvca>` re-hashes every object while streaming it in;
  a tampered or truncated archive is rejected before anything touches the
  store. Importing a snapshot that's already present is a verified no-op.
- `import --apply` chains straight into a rollback of the engine to that
  snapshot (one confirmation covers both).

### Interactive menus (`dockervc cli`)

- Every command is guided now, not just the basics: rollback with per-kind
  scope checklists, multi-select delete, export with a suggested output path,
  import with an archive picker, doctor offering repair when it finds
  problems.
- Tab completion: snapshot ids anywhere they're typed (`show 16` + Tab), and
  filesystem paths for import archives, `export -o` values, and
  `config set export.folder` — both on the `:` command line and inside path
  dialogs.
- The import menu lists `.dvca` archives found in the configured export
  folder, the current directory and its `exports/`, and `~/Downloads` —
  newest first, with size and location.
- Piped input and Windows fall back to a line-based menu with the same flows.

### Settings (`dockervc config`)

- `config set export.folder <dir>` — default folder for exports (used by the
  bare command and both menus; the directory must exist). `config unset
  export.folder` restores the default `exports/` behavior. Changeable from
  the menus too.
- `config unset <key>` is new; `zstd_level` unsets back to 3.

### Fixes

- `import --apply` silently skipped the rollback when the snapshot was
  already in the store — it now asks and runs like a fresh import.
- The menu's delete action only offered one snapshot at a time; it now takes
  a multi-select batch, with the command's own single confirmation naming the
  whole batch.

## v0.4.3 — 2026-09-12

Initial public prototype: snapshots (containers, images, volumes, networks),
content-addressed zstd store, `log`/`show`/`diff`/`status`, volume file
indexes with file-level diff, `doctor` (+`--deep`, `--repair`), `delete`,
`prune`, the `cli` TUI, and `man`.
