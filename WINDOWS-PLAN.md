# WINDOWS-PLAN.md — build dockervc for windows-amd64 from scratch

> **What this is:** a complete, self-contained functional specification.
> You are a coding agent on a Windows 10 22H2+/11 **amd64** machine with
> Docker Desktop (Linux containers mode) running, git, and a toolchain of
> your choosing, in an empty directory that contains only this file.
> There is **no repository to clone and no reference implementation to
> read**: build the entire tool — `dockervc` — from this document alone,
> natively for Windows. Read all of it before writing code, then work
> top to bottom.
>
> **Background:** dockervc exists on macOS (v0.5.1). That codebase is not
> available to you; this spec was distilled from it. Where this document
> is precise, follow it exactly — those parts are contracts (CLI
> behavior, store format, archive format, id/tag grammar). Where it is
> silent, choose sensible behavior and record the decision in
> WINDOWSREPORT.md (§14).

## 0. Mission and definition of done

Build a single executable `dockervc.exe` — local version control and
backup for a Docker engine — such that:

- all commands work as specified in §6: `init`, `snapshot` (alias
  `commit`), `log`, `show`, `diff`, `status`, `rollback`, `export`,
  `import`, `delete`, `prune`, `doctor`, `verify` (deprecated alias),
  `config`, `cli` (interactive), `man`, `version`
- the **full-screen TUI** runs in Windows Terminal, with the line menu
  as automatic fallback (§10)
- Tab completion works for snapshot ids **and Windows paths** (`C:\…`)
- archives exported on one Windows machine import cleanly on another
  (Windows-to-Windows only; mac interchange is **out of scope**)
- `install.ps1` end-to-end install → PATH → everything above passes
- your own test suite (§12) is green on Windows

You are done when §13's matrix passes with real recorded output and
§14's deliverables exist. Anything you could not test, say so plainly.

## 1. Ground rules

- **Own git history from hour one.** `git init` in the working
  directory before the first file; commit early and often. This project
  is not merged into anything — its history *is* the deliverable.
- **Commit messages** end with:
  `Co-Authored-By: Claude Code <noreply@anthropic.com>`
- **Never mutate a real store.** Anything live runs against a throwaway
  store: `$env:DOCKERVC_HOME = "$env:TEMP\dvctest"` (session-scoped).
  The `--store <path>` flag exists for targeted cases.
- **No binaries in git.** Build output, `dist/` are gitignored.
- **Docker access goes through the Docker Engine API** over the named
  pipe (`npipe:////./pipe/docker_engine`, the default when
  `DOCKER_HOST` is unset). Use a proper API client library for your
  language (Docker.DotNet for C#, bollard for Rust, docker-py for
  Python, docker SDK for Go). Do not drive the `docker` CLI as the
  primary mechanism — you need streaming tar capture/restore and
  container lifecycle calls, which the CLI does not expose cleanly.
- **Language is your choice.** Requirement: the end state is a single
  `dockervc.exe` (plus `install.ps1`) that runs on a clean Windows 11
  box with Docker Desktop. State your language and key libraries in
  WINDOWSREPORT.md. Go, Rust, and C# are all known-good fits.
- **Windows paths are strings, not parsed.** Store tar entry names and
  archive paths use `/` (§4, §8). Never build them with the OS path
  join of the host when the format demands `/`.
- **Do not transliterate the UTF-8 glyphs** (✓ — · … ✗). Functional
  parity means UTF-8 stays.

## 2. Toolchain and project layout

You need: your language toolchain, git, Docker Desktop **running**
(check `docker version`), and **Windows Terminal** set as the default
terminal (the TUI depends on VT sequences). Keep one plain `cmd.exe`
window around as the legacy-conhost fallback test case.

Suggested dev loop: build → `.\dockervc.exe <cmd>` from the repo root
against the throwaway store. Version stamping: builds report
`0.6.0` (see §6 `version`).

## 3. Concepts and core data model

**Snapshot.** A point-in-time capture of the Docker engine: containers
(committed filesystems), volumes (full content), images, networks
(config only), optionally bind mounts. Identified by:

```
snapshot id:  snap-YYYYMMDD-HHMMSS-xxxx      (local time; xxxx = 4 hex
              chars of cryptographic randomness — collision-unlikely)
restore tag:  <sanitized-container-name>-restored-from-<snapshot-id>
              sanitize(name) = lowercase; keep [a-z0-9._-]; every other
              char becomes '-'
commit ref:   dockervc/snap/<snapshot-id>/<sanitized-container-name>
```

Snapshot ids are sortable by creation time and prefix-resolvable.

**Manifest** — one JSON document per snapshot (stored in the DB and in
archives). Field names exactly:

```json
{
  "id": "", "created_at": "", "message": "", "docker_version": "",
  "engine_id": "", "consistent": false,
  "containers": [{ "name": "", "id": "", "inspect_json": {},
    "image_object": "", "layer_hash": "", "running": false, "size": 0 }],
  "images": [{ "refs": [], "digest": "", "object": "", "size": 0 }],
  "volumes": [{ "name": "", "driver": "", "options": {}, "object": "",
    "index_object": "", "files": 0, "size": 0 }],
  "networks": [{ "name": "", "inspect_json": {} }],
  "bind_mounts": [{ "host_path": "", "object": "", "size": 0 }],
  "total_size": 0,
  "stats": { "new_objects": 0, "reused_objects": 0, "new_bytes": 0 }
}
```

`inspect_json` is the complete container/network inspect document from
the Docker API (not a subset). `manifest_hash` (stored beside it, not
inside) = SHA-256 hex of the canonical JSON serialization. `consistent`
is true when captured with `--stop`. Omit empty optional fields
(`layer_hash`, `index_object`, `files`, `options`, `bind_mounts`).

**Objects (CAS).** All bulk data is content-addressed: SHA-256 hex of
the **stored bytes** (which are zstd-compressed), 64 lowercase hex
chars. Object kinds: `image` (image saves and committed container
filesystems), `volume`, `volindex` (file index of a volume, for
file-level diffing), `bindmount`. Objects are deduplicated by hash;
images and container filesystems additionally dedup by a *logical key*
(image repo digest, or `containerfs:<layer-hash>`) so logically
unchanged content is stored once across snapshots.

**File index (volindex).** For each captured volume, a JSON map of
`path → {type: file|symlink|hardlink|other, size, mtime (unix sec),
mode, hash (sha256 of content, regular files), link (target)}` with
directories skipped and paths normalized as clean slash-relative.
Enables `diff --files` with zero extra Docker I/O.

**Drift/diff model.** Comparing two states per kind: added `+`,
changed `~`, removed `-`. Container change reasons: image changed,
running state changed, filesystem changed (layer-hash compare). Volume
change reasons: content changed (via file indexes), metadata-only.

## 4. Store

### 4.1 On-disk layout (contract)

```
<path>/dockervc.db            SQLite catalog
<path>/objects/<hh>/<hash>    CAS blobs (hh = first 2 hex of hash)
<path>/tmp/                   staging; files atomically renamed into objects/
<path>/lock                   exclusive lock file, held while open
```

`DefaultPath()` resolution order:
1. `DOCKERVC_HOME` env var if non-empty (used verbatim)
2. `%ProgramData%\dockervc` if usable (directory creatable and a
   probe file can be written and removed there; fall back to literal
   `C:\ProgramData` if the env var is unset)
3. `%USERPROFILE%\.dockervc`

`init` creates `objects/` and `tmp/`, the DB, and writes the single
default config row `zstd_level = 3`. Re-init when `dockervc.db` exists
fails: `store at <path> is already initialized`. Success prints
`Initialized empty dockervc store at <path>` plus a hint to take a
first snapshot. Every store-requiring command on a missing DB fails
with: `dockervc store is not initialized (run "dockervc init" first)`.

### 4.2 SQLite schema (contract)

Driver settings: WAL journal, `busy_timeout=5000`, foreign keys ON,
and use a **single connection** to sidestep write contention. Schema
(all `IF NOT EXISTS`, applied on every open):

```sql
CREATE TABLE snapshots (
    id            TEXT PRIMARY KEY,
    created_at    INTEGER NOT NULL,          -- unix seconds
    message       TEXT NOT NULL DEFAULT '',
    manifest      TEXT NOT NULL,             -- full JSON
    manifest_hash TEXT NOT NULL
);
CREATE TABLE objects (
    hash       TEXT PRIMARY KEY,             -- 64-hex sha256
    kind       TEXT NOT NULL,                -- image|volume|volindex|bindmount
    size       INTEGER NOT NULL,             -- stored (compressed) bytes
    created_at INTEGER NOT NULL,
    image_key  TEXT                          -- logical dedup key, NULL else
);
CREATE TABLE refs (
    snapshot_id TEXT NOT NULL REFERENCES snapshots(id) ON DELETE CASCADE,
    hash        TEXT NOT NULL REFERENCES objects(hash),
    PRIMARY KEY (snapshot_id, hash)
);
CREATE TABLE config (key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE INDEX idx_refs_hash ON refs(hash);
CREATE INDEX idx_objects_image_key ON objects(image_key);
```

Semantics: `refs` is written when a snapshot is inserted (never during
capture); deleting a snapshot cascades its refs; object *files* are
only reclaimed by `prune`/`doctor --repair`, never by `delete`.
Snapshot lookup: exact id first, else unique prefix match (0 hits →
`no snapshot matching "<x>"`; >1 → `ambiguous snapshot prefix "<x>" —
be more specific`). `log` lists newest-first. `prune` selects objects
with zero refs.

### 4.3 Lock (single-writer)

Every opened store holds an **exclusive, non-blocking** lock on
`<store>/lock` until closed. A second open of a locked store fails
immediately with: `another dockervc operation is holding the store at
<path>: <cause>` — and must close everything it opened on the way out.
`Close()` order is fixed: close DB → release lock → close the lock
file handle, all unconditionally.

**Windows implementation (§10.1) gotcha:** byte-range locks conflict
*within one process* across handles. Any leaked store handle poisons
every later command in that process AND keeps the sqlite file
undeletable (Windows cannot delete open files). Treat "file in use by
another process" during test cleanup as a real handle-leak bug, never
flakiness. Write a regression test that a command failing partway
still releases the store.

### 4.4 CAS write path

`PutBlob(kind, key, stream)`: write through a zstd compressor
(level = `zstd_level` config, default 3) into a temp file in `tmp/`,
hashing the **compressed** bytes with SHA-256 as they are written;
flush + close; if `objects/<hh>/<hash>` already exists → dedup hit,
delete the temp, still ensure the catalog row exists (kind/key may
differ); else create the shard dir and atomically rename into place.
Always insert-or-ignore the `objects` row. Readers decompress on the
way out. `doctor --deep` re-hashes the raw on-disk (compressed) file —
that is why the hash is over stored bytes.

## 5. Docker engine capture and restore mechanics

Engine client: built from environment (`DOCKER_HOST` honored;
default transport is the Windows named pipe). API version negotiation
on. A `Ping` precedes anything live: `cannot reach docker engine: …`.

### 5.1 Capture (snapshot)

Fixed order: engine info (id, server version) → new snapshot id →
optional stop → **containers → volumes → bind mounts → images →
networks** → totals → insert row. Per-item failures are warnings on
**stderr** (`warning: …`), never fatal unless nothing can proceed; a
failed snapshot writes no row (already-stored objects are reclaimed
later by prune).

- **Containers** (all, including stopped): inspect each (full document
  into the manifest). Commit the filesystem:
  `commit dockervc/snap/<id>/<name>` → save the committed image as a
  tar stream → store as kind `image`. While saving, tee the stream and
  compute a **layer hash**: read `manifest.json` inside the image-save
  tar (array of `{Config, Layers}`), SHA-256 each named layer member,
  then SHA-256 the sorted layer hashes joined by `\n` with trailing
  `\n`. Layer hashing failure degrades to a warning. Dedup by
  `containerfs:<layer-hash>`: if an existing object has that key,
  delete the fresh copy and reuse; else claim the key. Remove the
  committed intermediate image afterwards (failure → warning).
- **Volumes**: list; skip anonymous (64-hex names) unless
  `--include-anonymous`. Content via a helper container (§5.2) `tar
  cf -` of the volume mounted read-only at `/src`, teed to a file
  indexer. Non-`local` drivers captured the same way but warned:
  `volume <v> uses driver "<d>"; captured via helper container, verify
  on restore`. If the file index parses, store it as a `volindex`
  object; tar-parse failure → warning, snapshot proceeds without it.
- **Bind mounts** (only with `--include-bind-mounts`): distinct bind
  sources across all containers, sorted; walk the host path and tar it
  directly (dirs and regular files; symlinks stored with targets).
- **Images**: dedup key = first repo digest else image id; on key hit,
  reuse the stored object (no engine I/O). Else `image save` the first
  repo tag (or id if dangling) → kind `image` with that key. Save
  failure → warning, image skipped.
- **Networks**: inspect → config JSON only, no data object. Skip
  builtins `bridge`, `host`, `none`, `docker`.

`--stop` (app-consistent): stop every running container (30s
timeout) *before* capture, restart them after — even on failure — and
set `consistent=true`.

`--only` restricts capture to a comma-separated subset of
`containers,volumes,images,networks,bindmounts` (validated; anything
else → `invalid --only scope "<x>" (valid: containers, volumes,
images, networks, bindmounts)`).

### 5.2 Helper-container tar streaming (volumes)

Pick the helper image from local tags in order: `alpine:3`,
`alpinelatest`, `busybox:latest`, `busybox:stable`,
`debian:bookworm-slim`; else any local tag containing alpine/busybox/
debian/ubuntu; else pull `alpine:3` (the only network touch, announced
on stderr). Run a disposable container (unique name, network disabled,
label `dockervc=helper`) with `tar cf - -C /src .` and the volume
mounted `:ro` at `/src`; attach and demultiplex the docker attach
stream (8-byte framed: stream-type byte + 4-byte big-endian size) —
stdout frame bytes are the tar; stderr frames are diagnostics.
Validate the tar magic (`ustar` at offset 257) before committing
anything to the CAS; force-remove the helper on EOF/error.

### 5.3 Restore (rollback)

- **Images**: `image load` the stored object's decompressed stream —
  parse the progress lines to learn what was loaded (loaded-by-id
  lines for untagged committed filesystems); then `image tag` it as
  each desired ref. Tag-only steps re-tag an already-present digest.
- **Volumes**: byte-exact clear-then-copy. Helper container (as above,
  but volume mounted read-**write** at `/dst`, command `sleep 300`):
  start it, clear the destination with a detached exec
  `find /dst -mindepth 1 -delete` polled to completion, then
  `copy to container` the decompressed tar into `/dst`, then
  force-remove the helper. Idempotent by construction.
- **Containers**: recreate from the stored inspect document: Config
  (env, cmd, entrypoint, working dir, labels, exposed ports, tty,
  healthcheck, stop signal/timeout, user, domainname, stdin,
  network-disabled) with `Image` replaced by the restore tag;
  HostConfig (binds, mounts, log config, network mode, port bindings,
  restart policy, auto-remove, caps add/drop, dns/dnssearch/dnsopts,
  extra hosts, ipc/pid modes, links, privileged, read-only rootfs,
  security opts, sysctls, tmpfs, shm size, and the resources block
  wholesale: memory, cpus, ulimits, devices…); NetworkingConfig per
  network with only aliases, links, driver-opts (static IPAM config
  and operational fields dropped). Keep the original name.
- **Networks**: create from stored inspect: driver, internal,
  attachable, options, labels, enable-ipv4/6 when true, IPAM config
  (subnet, ip range, gateway, aux addresses) — drop id/created/scope/
  containers/peers. Config-only networks are skipped with a warning.

## 6. CLI reference

Conventions for every command: report/progress on **stdout**; warnings
and per-item failures on **stderr** with prefixes `warning: ` and
`failed to …`; final errors print as `Error: <msg>` on stderr, exit
code 1 (0 otherwise — no other exit codes exist). Confirmations are
`<prompt> [y/N] `, accepting exactly `y`, `Y`, `yes`; every
destructive command has `-y/--yes`. Global flag `--store <path>`
(overrides `DOCKERVC_HOME`). HumanBytes: `%d B` below 1024, else
`%.1f <K|M|G|T|P>iB`.

**init** — §4.1.

**snapshot** (alias `commit`) — flags: `-m/--message`, `--stop`,
`--only <scopes>`, `--include-anonymous`, `--include-bind-mounts`.
Per-item progress lines like `  container <name>  <size>` /
`  volume <name>  <size>  [N files]` / `  image …` / `  network <n>
(config)`; dedup note `(filesystem unchanged, reused)`. Success:
blank line, `Snapshot <id> created (<size>) — <C> container(s), <I>
image(s), <V> volume(s), <N> network(s)`, then `Storage: <n> new
object(s) (<size>), <m> reused from earlier snapshots`.

**log** — table `SNAPSHOT  WHEN  SIZE  MESSAGE`, newest first, local
`YYYY-MM-DD HH:MM:SS`. Broken snapshots get a trailing
`✗ BROKEN (manifest unreadable)` or `✗ BROKEN (<n> object file(s)
missing)` (stat-only check, so log stays fast). Empty store → hint to
create one.

**show <snapshot>** — header (id, created RFC-1123 local, message,
engine `docker <ver> (engine <id12>)`, consistency
`crash-consistent` | `app-consistent (containers stopped)`, stored
size with new/reused split), then `containers:` / `volumes:` /
`images:` / `networks:` / `bind mounts:` sections when non-empty.

**diff <snapA> [snapB]** (or snapshot vs live) — header `Comparing
A → B`; per section (containers, volumes, images): `unchanged` or
`  + added`, `  ~ changed (reason)`, `  - removed`; no drift at all →
`No drift — engine matches the snapshot.` Flag `--files <volume>`
(two snapshots required): git-name-status list per file (`A`/`M`/`D`,
sorted by path, size column, symlink arrows), footer `<n> file(s)`;
missing index → graceful "file detail unavailable" notice.

**status** — `diff` of latest snapshot vs live, with header naming
the snapshot.

**rollback <snapshot>** — §7.

**export <snapshot> [-o <file>] | --latest** — §8. Flags: `-o/--out`,
`--latest`, `-y/--yes` (overwrite existing output), `--split`
(present but rejected: deferred). Default output: `export.folder`
config if set else `exports\<id>.dvca`, announced when defaulted.

**import <archive.dvca>** — §8. Flag: `--apply` (after import, roll
the engine back to it; one confirm), `-y/--yes`.

**delete <snapshot>…** — resolves *all* ids first (prefixes ok,
duplicates collapsed; one bad name deletes nothing), confirms, deletes
rows (objects stay until prune). Success: `Deleted <ids>. Run
"dockervc prune" to reclaim unreferenced objects.`

**prune** — finds objects with zero refs: `Found <n> unreferenced
object(s), <size>.` → confirm `Delete them?` → `Reclaimed <size>.`
Cleans empty shard dirs. Nothing to do → says so.

**doctor [--deep] [--repair] [-y]** — §9.

**verify** — deprecated alias of the deep check; warns and points at
`doctor --deep`.

**config [get <k> | set <k> <v> | unset <k>]** — bare lists
`<key> = <value>` lines (or `(no settings)`). Known keys, exactly
two: `zstd_level` (integer 1–22, else refused) and `export.folder`
(must be an existing directory, else refused). Unknown key → `unknown
setting "<k>" (known: zstd_level, export.folder)`. Unset returns the
key to its default (`zstd_level`→3, `export.folder`→`exports\` in the
current directory) and says so.

**cli** (aliases `interactive`, `menu`, `ui`) — §11.

**man** — prints a full command reference: `DOCKERVC(1)`, NAME,
SYNOPSIS (`dockervc <command> [flags] [args]`, plus the `cli` line),
COMMANDS (sorted; each with aliases, short help, flags with types and
usage), GLOBAL FLAGS (`--store`), NOTES (content-addressed dedup
paragraph). Auto-include `help` and shell `completion` commands if
your CLI framework provides them.

**version** — `dockervc 0.6.0 (windows/amd64)` — stamp 0.6.0 in
release builds; dev builds may say `dev`.

## 7. Rollback

**Scope.** `--all`, or any of `--containers/--volumes/--images/
--networks` with comma-separated names/refs/digests (validated against
the snapshot: unknown names refuse the whole run; image selectors may
be exact ref, exact digest, or unique digest prefix). No scope flags
outside the TUI → interactive per-kind confirms (`Restore all <n>
container(s)?` …); nothing selected → error naming the flags. Inside
the TUI a scope is mandatory.

**Flow.** ping → resolve snapshot → broken gate (§9) → resolve scope →
fetch live state (one inventory pass) → build plan → empty plan prints
`Nothing to restore — the engine already matches the selection.` →
`--dry-run` prints the plan and exits → **pre-rollback checkpoint**
(unless `--keep-current`): a normal snapshot with message
`pre-rollback checkpoint before <id>`; failure aborts; success prints
`pre-rollback checkpoint <id> created (--keep-current skips this)` →
print plan → confirm `Restore snapshot <id> — <n> step(s), including
<c> container removal(s) and <v> volume content replacement(s)?` →
apply → `Engine rolled back to <id>.`

**Plan** (pure computation, no engine access) — step order:
1. `stop+remove container <name>` — selected containers whose name
   exists live (why: name conflict)
2. `create network <name>` — needed networks missing live (existing
   networks are never deleted; config drift → warning, reuse)
3. `load image` — per selected image record: digest present with all
   refs tagged → skip `already present`; refs missing → tag-only step;
   else full load. Then committed container filesystems grouped by
   object: all restore tags present → skip `filesystem already
   loaded`; else one load named by the first owner's restore tag
4. `restore volume <name>` — live → `contents will be replaced`
   (+warning that current contents are lost); missing → `missing`
5. `create container <name>` — from inspect, image = restore tag;
   containers without a committed filesystem are skipped with warning
6. `start container <name>` — those recorded running

Implicit dependencies: selecting a container pulls in its named
volumes and attached networks (parsed from the stored inspect).
Warnings for: uncaptured volumes (recreated empty), uncaptured
networks (create fails unless present), missing bind sources.

**Apply** — sequential, fail-fast, but every step is idempotent:
already-done work is *skipped by the plan builder*, and re-running the
same command after a mid-apply failure is the documented recovery
(the deterministic restore tags are what make re-runs skip loaded
filesystems). Progress: `  N. ✓ <step>` per applied step, `  N.
<step>` per skipped; failure reports `N of M steps completed, failed
at: …`; end `rollback applied: M step(s) (K skipped as already done).`

## 8. Portable archives `.dvca` (format contract)

Outer container: an **uncompressed** tar (the blobs inside are
already zstd). Entry order and names, exactly:

```
manifest.json            full snapshot manifest JSON
objects/<hash>           one per referenced object, sorted, deduped —
                         the stored (compressed) file bytes VERBATIM
checksums.sha256         written LAST (tar is append-only; covers all)
```

All tar headers regular-file, deterministic (fixed mode, modtime =
snapshot creation) so two exports of one snapshot are byte-identical.
`checksums.sha256` uses GNU `sha256sum -c` lines `<hash>␣␣<path>`
(two spaces): first line = manifest hash for `manifest.json`, then
per object `<hash>  objects/<hash>` (an object's checksum is its
name — the CAS invariant). Entry names use `/` always.

**Export**: broken-snapshot gate; free-space pre-flight —
`needed = total_stored_bytes + (n_objects + 2) * 1 KiB + 4 KiB`
against the caller-available free space (Windows: `GetDiskFreeSpaceExW`,
**first** out-param; failure = skip the check); staged write to a
temp file in the destination directory, flush, then rename — an
interrupted export never leaves a half-written archive under the real
name; progress `  exporting object i/n (<name>)…`; success `Exported
<id> → <path> (<n> object(s), <size>)`.

**Import** — three passes over the file:
1. **Index** (no writes): validate structure — only the three entry
   kinds above, no duplicates, object names exactly 64 lowercase hex,
   both meta entries present, manifest parses; checksum lines must
   cover the manifest (hash equality — else `archive tampered or
   truncated`) and every object (hash must equal its entry name).
2. **Adopt**: every referenced object must be present as an entry
   (extras ignored); stream each into the CAS **verbatim, no
   recompression**, hashing in flight — advertised size must match
   bytes read and the hash must equal the name, else reject (atomic
   in the sense that no snapshot row is written; adopted objects are
   self-verifying CAS files and may remain, reclaimed by prune).
   Already-present objects are still fully verified (re-hash).
   Progress `  verifying object i/n…`.
3. **Register**: same id + same manifest hash already present →
   `already imported — <id> is in this store unchanged (N object(s)
   verified, 0 new).` (the verified no-op); same id, different
   content → hard error telling the user to delete or use another
   store; else insert the snapshot row.

Pre-flight print: `archive holds snapshot <id> — "<message>"` and
`  created <when> · <n> object(s) · <size> · <k> already present
locally`. `--apply` chains into §7 with `--all`, taking a
`pre-apply checkpoint before imported <id>` first, one combined
confirm; declining leaves the import intact. Import into a store that
doesn't exist yet is the migration path (`init` first).

## 9. doctor

`diagnose` checks: (1) SQLite `PRAGMA quick_check` — `catalog: OK` /
`catalog: FAILED SQLite quick_check`; (2) every snapshot's manifest
parses and every referenced object file exists (stat-only) — broken
snapshots are the failure unit, reason `manifest unreadable` or
`<n> object file(s) missing`; (3) `--deep` re-hashes every object's
stored bytes (hash or size mismatch → `<n> corrupted`, snapshots
resting on them also broken); (4) unreferenced catalog rows
(orphans); (5) untracked files under `objects/` whose names aren't
known hashes.

Report lines: `store: <path>`, `catalog: …`, `snapshots: <n> checked,
…`, `content: …` (`not checked (`--deep` re-hashes every object)` when
shallow), `objects: <a> unreferenced row(s) (<size>), <b> untracked
file(s) (<size>)`. Unhealthy → `<n> problem(s) found — run "dockervc
doctor --repair" to fix them` (exit 1). Healthy shallow run tips about
`--deep`.

`--repair` offers, each with its own confirm (declining one never
blocks others; `-y` auto-accepts): delete broken snapshots (the only
repair for them), remove orphaned rows (re-queried after snapshot
deletions — deleting snapshots orphans more), delete untracked files
(list capped at 5 + `… <n> more`); then cleans empty shard dirs and
re-diagnoses: `store is healthy.` or `repair finished, <n> problem(s)
remain (declined or failed fixes)`.

## 10. Windows-specific requirements

1. **Store lock** — `LockFileEx` exclusive + fail-immediately over
   the whole file (and `UnlockFileEx` over the *identical* range —
   ranges must match exactly). See the §4.3 in-process-handle gotcha.
2. **Free space** — `GetDiskFreeSpaceExW`; the caller-available value
   is the **first** out-parameter.
3. **TUI console mode** — enable
   `ENABLE_VIRTUAL_TERMINAL_PROCESSING` on stdout at startup (report
   success/failure). The TUI gate is: stdin is a terminal AND stdout
   is a terminal AND VT enable succeeded — otherwise the line menu,
   exactly as piped input gets. Best-effort `SetConsoleOutputCP(65001)`
   at TUI startup so UTF-8 glyphs render; never fail startup over it.
4. **Path completion** — split the token being completed on both `/`
   and `\`; bare `C:` and `C:\` list the drive root; completions carry
   the OS separator; `~` expands to the profile dir. Snapshot ids
   never appear in path completions (they have their own machinery).
5. **Docker transport** — environment-configured client defaults to
   the named pipe on Windows; nothing to configure.
6. **Installer `install.ps1`** — per-user, admin-free: params
   `-InstallDir` (default `%LOCALAPPDATA%\Programs\dockervc`) and
   `-NoPath`; expects `dockervc.exe` next to the script; copies it,
   adds InstallDir to the *user* PATH if absent (and says to open a
   new terminal), runs `dockervc.exe version` to verify, warns
   friendly when `docker version` fails; `$ErrorActionPreference =
   "Stop"`; usage header comments.

## 11. Interactive front-ends

**Dispatch:** both stdin and stdout are terminals **and** VT is on →
full-screen TUI; otherwise the line menu. This one gate covers piped
input and legacy consoles automatically.

**Full-screen TUI** — alternate screen + hidden cursor, reverse-video
selection, home/erase-line repaints, size from the terminal (fallback
80×24, min width 40). Frame: header (`dockervc 0.6.0 │ <store status
line>`), divider, body, divider, hint bar, input line with a block
cursor. The store status line shows `<path> — <n> snapshot(s), <size>`
or `(not initialized — run "dockervc init")`.

Menu actions (the product's spine): status, snapshot, log, show,
diff, delete, doctor, prune, config, rollback, export, import. Menu
keys: arrows/j/k move, Enter run, `1-9` jump, `:` command mode, `?`
man, `q` quit, **Esc-Esc** quit (first Esc arms with a flash, second
quits; Ctrl-C always quits).

Command mode `:` — type any CLI invocation; Tab completes **snapshot
ids** in the argument slots of show/diff/delete/rollback/export, and
**paths** for `import <p>`, `export … -o <p>`, and
`config set export.folder <p>`; a candidate pane lists matches
(`N match(es) for "<tok>" — Tab completes:`), Tab replaces with the
unique match or the longest common prefix. Enter runs via the real
CLI parser (menus add prompts, never logic). Destructive commands —
`delete`, `prune`, `rollback`, `doctor --repair`, `import --apply` —
get one extra `run <argv> ?` confirm unless `-y/--yes` is present.
Output is captured into a scrollable pane (`↑↓/j/k` scroll, `g`/`G`
top/bottom, `:` command while reading, `q`/Esc back).

Dialogs: text (optional default; path variant Tab-completes), bool
(`y/n`, Enter = default), snapshot picker (type-to-filter on id
prefix and message substring), list/single-choice and checklist
(space/x toggle, `a` all/none, Enter confirm, Esc cancel). Guided
flows funnel into the commands: snapshot (message + stop?),
rollback (pick → scope menu or per-kind checklists → optional dry-run
preview → apply confirm), delete (checklist), export (pick + path),
import (archive discovery in export folder/cwd/`exports`/Downloads,
`.dvca` case-insensitive), config (export.folder get/set/unset),
doctor (deep? → offers repair when problems found).

**Line menu** (the fallback): banner + store status, numbered action
list with args-hints and descriptions, `dockervc> ` prompt accepting
a number, any command with flags, `help`/`man`, or `quit`; guided
prompts for the same flows (`message [interactive snapshot]: `,
`stop containers first for app-consistent data? [y/N]: `, numbered
snapshot pickers, etc.).

## 12. Test suite

Author a real suite in your language's standard framework; it is a
deliverable. Required coverage (port the *behavior*, not the code):

- store: init idempotence-refusal, lock contention (second open fails
  fast with the message), **close-on-error releases the lock** (the
  leak regression), prefix resolution (none/unique/ambiguous)
- CAS: round-trip through compression, dedup by hash, atomic staging
- volindex: created/modified/deleted diffing incl. metadata-only
- rollback plan: pure step-building from two manifests (skips,
  ordering, implicit container deps)
- paths: completion splitting on `/` and `\`, drive roots `C:` and
  `C:\`, `~` expansion — exercised on Windows
- archives: write→read round-trip, deterministic bytes, tamper
  rejection (flip a byte → refuse, no snapshot row), idempotent
  re-import, adopt-verbatim (no recompression)
- config: validation rules for both keys, unknown-key refusal
- CLI: every command's success/error shapes against temp stores via
  `DOCKERVC_HOME`
- TUI gate: VT-off → line menu (injectable VT flag)

Lint + full suite green on Windows is §13 item 11.

## 13. Verification matrix (record real output for each)

Setup: fresh store in `$env:TEMP\dvctest` via `$env:DOCKERVC_HOME`,
and the §13-demo engine below.

1. **Build & smoke:** build → `.\dockervc.exe version`, `man`
2. **Store:** `init` → confirm `%ProgramData%\dockervc` (or home
   fallback) → **lock check:** run `snapshot` and, while it runs,
   `log` in a second terminal → the second must refuse with the lock
   message, not corrupt
3. **Snapshot / history:** `snapshot -m` (full + `--only
   containers,volumes` + `--stop`), `log`, `show`, `status`, `diff`
   (+`--files demo-data`)
4. **Rollback:** mutate a volume, delete demo-web, then `rollback
   <snap> --all --dry-run` → apply → volume byte-restored, demo-web
   recreated under its restore tag; granular `--volumes demo-data`
   touches nothing else; `--keep-current` skips the checkpoint;
   re-run → skips (idempotent)
5. **Export/import round-trip (the keystone):** `export` to a
   `C:\`-nested `-o` path → point `DOCKERVC_HOME` at a second, fresh
   store, `init`, `import` the archive → `import --apply` restores
   there. Tamper one byte in a copy → import must reject atomically;
   re-import the good archive → verified no-op. (If a second physical
   machine or VM is available, repeat once across machines — that is
   the actual interop claim; otherwise say so in the report.)
6. **Health:** `doctor` clean, `--deep`; move one object file aside →
   broken marker in `log` → `doctor --repair` flow
7. **Maintenance:** multi-`delete`, `prune` reclaim count
8. **Settings:** `config set export.folder C:\Users\...\backups` →
   bare `export` honors it → `config unset export.folder` → back to
   `exports\`; `config unset bogus` refused; `config unset
   zstd_level` → 3
9. **TUI:** `.\dockervc.exe cli` in Windows Terminal — menus,
   dialogs, snapshot-id Tab completion, **path Tab completion of
   `C:\` paths**, scrollable output, Esc-Esc quit; piped input → line
   menu; legacy conhost → line menu (the §10.3 gate)
10. **Free space:** export to a nearly-full volume (small USB drive)
    → pre-flight error names the real free space
11. **Suite:** lint + `test` green on Windows.

**Demo engine** (run once in PowerShell, Docker Desktop up):

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

Wipe later with `docker rm -f demo-web demo-worker demo-sidecar;
docker volume rm demo-data demo-worker-data; docker network rm
demo-net`.

## 14. Deliverables and hand-back

1. The working directory: complete source, `install.ps1`, release
   zip layout (`dockervc.exe` + `install.ps1` + README beside it),
   `.gitignore`d build output — all committed to **its own git
   history** (init from the first commit; descriptive messages)
2. Lint + full test suite green on Windows
3. `WINDOWSREPORT.md` in the root: language + key library choices,
   per-feature verdict table, findings register (severity + fix
   status), the §13 matrix with actual results, and a plain statement
   of what was **not** tested
4. Hand the whole directory back to the maintainer (the user) —
   USB copy or a git remote they designate. Nothing merges anywhere
   else; this is a standalone Windows implementation of dockervc 0.6.0

## 15. Out of scope

- mac/Linux interoperability of any kind (archives are
  Windows-to-Windows only)
- winget/choco/scoop packaging, code signing, SmartScreen appeasement
- Windows Containers mode (Docker Desktop Linux containers only)
- cygwin/msys terminal quirks beyond "line menu works there"
- reading or writing any macOS dockervc store
