# dockervc — Local Version Control & Backup for Docker

**Status:** Steps 1–3 complete (snapshot engine, CAS store, TUI, volume file
indexes, rollback). Step 4 (export/import) is specified but not implemented —
**see [HANDOFF.md](HANDOFF.md) for the implementation brief**;
this document is the architecture intent, HANDOFF.md is ground truth for what
exists in code today.
**Target:** Linux and macOS hosts; 100% local operation, no cloud
dependencies. Host-to-host migration via export/import is the primary
deployment story (see §5). (Windows support shipped broken in v0.5.0 and was
removed; see WINDOWS-PLAN.md for the from-scratch rebuild.)

---

## 1. Language & Libraries

| Choice | Selection | Rationale |
|---|---|---|
| Language | **Go** | Official Docker SDK; compiles to a single static binary (`CGO_ENABLED=0`); trivial cross-compile (macOS arm64 → linux/amd64, linux/arm64); zero runtime deps on target machines. |
| CLI framework | `spf13/cobra` | Same framework as the Docker CLI itself; POSIX flags, subcommands, shell completion. |
| Docker access | `github.com/docker/docker/client` | Talk to the engine over the Unix socket (`/var/run/docker.sock`) via API — no shelling out for inspect/create; shell out only for `docker save`/`load` streams where the API is awkward. |
| Metadata store | `modernc.org/sqlite` | Pure-Go SQLite — **no CGO**, so the binary stays fully static. |
| Compression | `klauspost/compress/zstd` | Fast, high-ratio; pure Go. |
| Hashing | `crypto/sha256` (stdlib) | Content-addressed object IDs, integrity verification on import. |

Single binary name: `dockervc`.

---

## 2. High-Level Architecture

```
┌─────────────────────────── dockervc (single Go binary) ───────────────────────────┐
│                                                                                    │
│  CLI layer (cobra)          Core engine                  Storage layer             │
│  ┌───────────────┐   ┌─────────────────────────┐   ┌──────────────────────────┐   │
│  │ snapshot      │──▶│ SnapshotService          │──▶│ ObjectStore (CAS)        │   │
│  │ rollback      │   │  capture / plan / apply  │   │  objects/<sha256[:2]>/   │   │
│  │ export/import │   │ RollbackService          │   │  <sha256>                │   │
│  │ log/prune     │   │ ExportService (tar+zstd) │──▶│ SQLite catalog           │   │
│  └───────────────┘   │ ImportService (verify)   │   │  snapshots, refs, config │   │
│                      └───────────┬─────────────┘   └──────────────────────────┘   │
│                                  │                                                 │
│                                  ▼                                                 │
│                     Docker Engine API (/var/run/docker.sock)                       │
│                     inspect · commit · save · load · create · start                │
└────────────────────────────────────────────────────────────────────────────────────┘
```

### Storage layout (default: `/var/lib/dockervc` on Linux, `~/.dockervc` on macOS; override with `DOCKERVC_HOME` or `--store`)

```
/var/lib/dockervc/
├── dockervc.db                  # SQLite catalog: snapshots, object refs, config
├── objects/                     # content-addressed blob store (git-like)
│   ├── ab/ab9c3f…               # zstd-compressed image/volume tars, file indexes
│   └── …
└── tmp/                         # staging area (atomically renamed into objects/)
```

**Content-addressable storage (CAS):** every artifact is zstd-compressed, hashed
(SHA-256 of the *compressed* bytes), and stored under that hash. A snapshot is
just a *manifest* referencing object hashes. Consequence: **unchanged
images/volumes across snapshots are stored exactly once** — dedup is free, and
exports/imports verify integrity by re-hashing.

---

## 3. Snapshot Data Model — what is captured and how

A snapshot = immutable manifest + referenced objects.

| Entity | Config / metadata | Data | Method |
|---|---|---|---|
| **Container** | Full inspect JSON: image ref, env, cmd, entrypoint, ports, labels, mounts, network membership, restart policy, run-state (running/stopped) + layer-content fingerprint | Container's writable layer | `docker commit` → tagged `dockervc/snap-<id>/<name>` → `docker save` → CAS object; intermediate tag then removed (configurable). Each commit re-tars the filesystem (new config timestamps), so the object hash differs even for identical content — the stored **layer hash** (over layer content only) is what `diff` compares and what lets an unchanged container reuse an earlier snapshot's object instead of storing a duplicate. |
| **Image** | Repo tags, digest | Layer data | `docker save` → CAS object (deduped by digest — already-saved images are skipped). |
| **Named volume** | Driver + options + per-file index (path, size, mode, content SHA-256) | Volume contents | Tar via a short-lived helper container mounting the volume read-only (`tar cf - -C /src .`, attached stdout), streamed into CAS zstd-compressed. The file index is parsed from the same stream — no extra Docker I/O — and stored as its own tiny object, enabling file-level `diff --files`. |
| **Bind mount** *(opt-in)* | Host path | Directory contents | Tarred directly from host path (requires running on the Docker host). Only with `--include-bind-mounts`. |
| **Network** | Inspect JSON (custom networks only) | — | Recreated from JSON at rollback. Built-ins (`bridge`, `host`, `none`) skipped. |

**Consistency model:**
- Default: *crash-consistent* — volumes tarred while containers run (like pulling the power).
- `--stop` flag: application-consistent — containers stopped before capture, restarted after
  (their run-state is recorded either way, so rollback restores it correctly).
- Compose deployments: `com.docker.compose.*` labels are captured, so restored containers keep
  their project membership.

**What is intentionally NOT captured** (documented limitations): in-memory process state,
ephemeral/anonymous volumes (opt-in), container IPs (reassigned by Docker), Swarm services.

---

## 4. SQLite Catalog (actual schema)

The full manifest is stored as JSON in one row per snapshot; entity rows are
*not* normalized out (the manifest is the portable unit — export/import copies
it verbatim). `refs` is the only join needed for GC.

```sql
snapshots(id TEXT PK, created_at INT, message TEXT,
          manifest TEXT NOT NULL,      -- full model.Manifest JSON
          manifest_hash TEXT NOT NULL) -- sha256 of that JSON

objects(hash TEXT PK,                 -- sha256 of the stored (zstd) bytes
        kind TEXT NOT NULL,           -- image | volume | volindex | bindmount
        size INT NOT NULL,            -- stored (compressed) bytes
        created_at INT NOT NULL,
        image_key TEXT)               -- docker image digest, for dedup; NULL otherwise

refs(snapshot_id TEXT REFERENCES snapshots(id) ON DELETE CASCADE,
     hash TEXT REFERENCES objects(hash),
     PRIMARY KEY (snapshot_id, hash))

config(key TEXT PK, value TEXT NOT NULL)
```

GC is reference-based: `dockervc prune` deletes objects not referenced by any
snapshot (via `refs`).

---

## 5. CLI Command Reference

```
dockervc init                                     # create + lock store, record engine id
dockervc status                                   # engine state vs. last snapshot (drift report)
                [--deep] [--volumes pgdata,redis] # hash selected live volume files; no store writes
dockervc cli                                      # full-screen interactive menu (TUI)
                                                  #   ':' command line (also over output panes),
                                                  #   Tab-completes snapshot ids, double-Esc quits
dockervc man                                      # man-page-style command reference

# ── Version control ─────────────────────────────────────────────────────────────
dockervc snapshot  [-m "message"]                 # capture everything
                  [--only containers,volumes,images]
                  [--include-bind-mounts] [--include-anonymous]
                  [--stop]                        # app-consistent (stop → capture → start)
dockervc log                                      # list snapshots (id, date, msg, size)
dockervc show     <snap>                          # contents of one snapshot
dockervc diff     <snapA> [<snapB>]               # compare (snapB defaults to live engine);
                  [--files <volume>]              #   volumes show +created/~modified/-deleted counts,
                                                  #   --files lists individual files, including
                                                  #   snapshot-to-live when snapB is omitted
dockervc delete   <snap> [--yes]                  # drop snapshot (objects GC'd on prune)

# ── Rollback ────────────────────────────────────────────────────────────────────
dockervc rollback <snap>                          # interactive scope prompt
                  [--all]                         # full engine state (destructive)
                  [--containers web,db]           # granular: only these containers
                  [--volumes pgdata,redis]        # granular: only these volumes
                  [--images myapp:v2]             # granular: only load these images
                  [--networks backend]            # granular: only these networks
                  [--dry-run]                     # print the plan, change nothing
                  [--yes]                         # skip confirmation
                  [--keep-current]                # don't auto-checkpoint current state (default: do)

# ── Portability ─────────────────────────────────────────────────────────────────
dockervc export   <snap> -o state.dvca            # single-file portable archive
                  [--latest]                      # [--split 4g] deferred
dockervc archive  list [directory]                # fast manifest-only export listing
dockervc archive  show state.dvca                 # inspect without importing
dockervc archive  verify state.dvca               # full structure + checksum pass
dockervc archive  files state.dvca <volume> [path] # list an archived volume index
dockervc import   state.dvca [--apply]            # verify + add to local store
                                                  # --apply additionally rolls back to it

# ── Maintenance ─────────────────────────────────────────────────────────────────
dockervc prune                                    # GC unreferenced objects
dockervc doctor                  [--deep]         # structural health check (instant)
                                  [--repair]       # interactively fix what it finds
                                                  # --deep re-hashes every object (content
                                                  # integrity) — `verify` is a deprecated
                                                  # alias for it
dockervc config   [key value]                     # store path, retention, zstd level…

# Global: --no-progress suppresses stderr progress. Export/restore use exact
# stored-byte ETA; snapshot/deep-status estimates are marked ~ETA.
dockervc version                                  # build version
```

### Rollback semantics

Every destructive rollback **automatically takes a safety snapshot of current state first**
("pre-rollback checkpoint") unless `--keep-current` is passed — so rollbacks are themselves
reversible.

- **Full (`--all`)**: stop+remove all containers → recreate custom networks → `docker load`
  images → recreate+fill volumes → recreate containers from captured inspect JSON (mounted to
  restored volumes/networks, committed image as their image) → start those that were running.
- **Granular**: identical machinery, scoped to the named entities. Unnamed entities untouched.
- `--dry-run` prints the exact plan (what gets stopped/removed/created) before anything happens.

### Export archive format (`*.dvca`)

```
state.dvca                        # uncompressed outer tar (the blobs inside are
│                                 #   already zstd — double compression buys nothing)
├── manifest.json                 # the snapshot's model.Manifest JSON, verbatim
├── checksums.sha256              # GNU sha256sum lines: "<hash>  objects/<hash>"
│                                 #   per object + "<hash>  manifest.json" — a
│                                 #   plain `shasum -a 256 -c` can verify an
│                                 #   unpacked archive with no dockervc present
└── objects/<hash>                # the CAS blobs this snapshot references,
                                  #   stored bytes verbatim (zstd-compressed)
```

`export snap-…` writes `exports/<snapshot-id>.dvca` when `-o` is omitted
(or the `export.folder` setting's target — `dockervc config set/unset`); import
re-hashes every object while streaming it, adopts the bytes verbatim into the
local CAS (`store.AdoptObject` — never re-compressed, so hashes keep their
identity across machines), and registers
the snapshot — after which `rollback` works exactly as on the source machine.
Machine-independence: no absolute paths in the manifest are load-bearing;
engine-specific fields (engine id, container IDs) are recorded but never
required for restore. (Exact algorithms and edge cases: HANDOFF.md §5.)

---

## 6. Implementation Plan (maps to Steps 2–5)

| Step | Deliverable | Status |
|---|---|---|
| **2** | Core: Go module, `init`, `snapshot`, `log/show/diff/status`, CAS + SQLite catalog, `cli` TUI, `man`, volume file indexes | ✅ done |
| **2b** | Health: `doctor` (structural + `--deep` content re-hash, `--repair` with per-step confirmation), broken markers in `log`; `verify` deprecated to alias | ✅ done |
| **3** | Rollback: plan builder, full + granular apply, pre-rollback checkpoint (`internal/rollback`) | ✅ done — implementation notes at the end of HANDOFF.md §4 |
| **4** | Export/import: `.dvca` writer/reader, verification, `--apply` (`internal/portable`) | ✅ done — no `drives` command (de-scoped by user decision; see EXPORT-PLAN.md amendment) |
| **5** | `install.sh`, Makefile cross-compile (darwin/linux/windows × amd64/arm64), README | ✅ done (deb/rpm deferred) |

**Safety properties throughout:** single-writer via file lock on the store; atomic object
publication (write to `tmp/`, rename); every archive verified by hash before restore; no
remote network calls anywhere in the codebase.
