# Implementation Plan — Export/Import + Removable Drives (`dockervc` Step 4, v0.6.0)

Status: PLAN for implementation. Read HANDOFF.md §0/§2/§5 first; this plan was
validated against the code (including the in-flight Step 3 rollback code in
`internal/rollback/`, `internal/cli/rollback.go`), the live store, real
`diskutil` output on this Mac, and Go library documentation. Format and depth
follow ROLLBACK-PLAN.md.

## 1. Recommended basic-tier scope

```
dockervc export <snap> -o state.dvca [--latest]     # .dvca writer
dockervc import state.dvca [--apply]                # verify + adopt + register
dockervc drives                                      # list removable/external volumes + free space
```

In scope:
- `internal/portable`: streaming `.dvca` writer (uncompressed outer tar per
  HANDOFF §5) and reader (verify-while-streaming, adopt-verbatim, register).
- `store.AdoptObject`: the one new store method — places archive bytes into the
  CAS **verbatim** (no re-compression; HANDOFF §2's hard rule).
- `internal/drives`: best-effort removable/external volume detection, pure Go,
  zero new dependencies (see §2).
- `export` without `-o`: default to the single detected removable volume, or
  print the `drives` listing and ask for `-o` explicitly. Explicit `-o` always
  wins; detection is a convenience, never correctness-critical.
- `--latest` (export the newest snapshot). `--split` registered but rejected
  with a "deferred" message (HANDOFF §5 requires rejecting the flag).
- `import --apply`: import, confirm once, then run the rollback machinery
  against the imported snapshot via the **public** `internal/rollback` API
  (FetchLiveState/BuildPlan/Executor) — no edits to rollback.go, which the
  Step 3 agent owns.
- Full TUI/menu wiring per HANDOFF §6 checklist.

Out of scope (documented as deferred): multi-snapshot archives (§3.4 —
recommend deferring), `--split` (§3.5), import-as-repair
(`doctor --repair --from`, §7 — recommend follow-up), auto-mounting unmounted
disks (§2.1 option note), file-path Tab-completion in the TUI (skip: the
completion engine is snapshot-id-shaped; fs scanning is a separate mechanism
for little gain).

Version: Makefile `VERSION` → **0.6.0** (Step 4 ships after Step 3's 0.5.0).

## 2. Drive detection per OS — evidence, limitations, fallback

### 2.1 macOS (daily driver) — /Volumes scan + `statfs` + `diskutil info -plist`

Pure-Go options were evaluated honestly. DiskArbitration/IOKit are Objective-C
frameworks — callable only via cgo, which §0 forbids; no maintained pure-Go
DiskArbitration binding exists ([community consensus](https://stackoverflow.com/questions/23128148/how-can-i-get-a-listing-of-all-drives-on-windows-using-golang), [gousbdrivedetector](https://github.com/deepakjois/gousbdrivedetector) is the only cross-platform pure-Go attempt and is abandoned). The practical CGO-free pattern is
shelling out to `diskutil`, whose plist output carries exactly the
DiskArbitration flags we need. Verified live on this Mac:

- `/Volumes/` on this machine contains only `Macintosh HD -> /` (symlink to the
  internal disk). `nobrowse` system volumes do not appear there — a feature:
  the directory pre-filters to user-visible mounts. Network shares (smbfs/nfs)
  *do* mount under `/Volumes` and must be filtered out.
- `diskutil info -plist <mountpoint>` (accepts a mount path; verified with
  `diskutil info -plist /Volumes/Macintosh\ HD`) emits an XML plist whose
  load-bearing keys are stable and match DiskArbitration semantics:

```xml
<key>Internal</key><true/>
<key>RemovableMediaOrExternalDevice</key><false/>   <!-- THE external-ness key -->
<key>Ejectable</key><false/>
<key>Removable</key><false/>
<key>WritableVolume</key><false/>
<key>VolumeName</key><string>Macintosh HD</string>
<key>MountPoint</key><string>/</string>
<key>BusProtocol</key><string>Apple Fabric</string>
<key>FilesystemType</key><string>apfs</string>
<key>TotalSize</key><integer>…</integer>
```

`RemovableMediaOrExternalDevice` is Apple's own heuristic (true for USB sticks
**and** Thunderbolt/USB external SSDs — better than the raw `Removable` bit,
which many external SSDs don't set; eject-ability quirks on Thunderbolt bridges
are a known DiskArbitration caveat, [see](https://stackoverflow.com/questions/38499860/)).

**Strategy (darwin.go):** for each `/Volumes` entry (skip dot-entries, skip
symlinks resolving to `/`): `syscall.Statfs` → fstype (`Fstypename`) + free
space + read-only flag; **exclude** fstypes `smbfs, nfs, afpfs, autofs,
devfs, nullfs, webdavfs`; then `exec.LookPath("diskutil")` +
`diskutil info -plist <path>` → classify external when
`RemovableMediaOrExternalDevice == true` (and skip when not `WritableVolume`).
diskutil failure/absence (e.g. network mount, odd setup) → entry demoted to
the "all volumes" listing with a warning, never fatal.

**Unmounted-but-attached disks:** `diskutil list -plist` gives `AllDisks`
(whole disks incl. unmounted) but *not* the Internal/Removable flags (verified:
`AllDisksAndPartitions` has `MountPoint/VolumeName/Size` only), so the flow is
`diskutil list -plist` → `diskutil info -plist <diskN>` per whole disk →
externals without a MountPoint are listed as `attached, not mounted — run
diskutil mountDisk disk4 or use Disk Utility`. **Auto-mounting via
`diskutil mountDisk` is offered as an option only** (tradeoffs: changes system
state, may fail on foreign filesystems, surprises users; a printed hint is
honest and zero-risk) — recommended NOT in basic scope.

**Rejected alternatives** (one line each): `system_profiler SPUSBDataType`
(slow, seconds; USB-tree→mountpoint mapping is manual — diskutil already
aggregates it); `mount` output (no removability info, `\040` escaping);
`ioreg` (lowest signal-to-noise).

### 2.2 Linux — `lsblk -J` primary, sysfs fallback

- Primary: `lsblk -Jb -o NAME,RM,TRAN,TYPE,SIZE,MOUNTPOINT` (util-linux
  ≥2.27 for `-J`, ubiquitous on desktops/servers) parsed as JSON. Include a
  mounted block device when `RM==1` **or** `TRAN=="usb"` — this is the key
  correctness point: the kernel `removable` flag is **device-reported SCSI
  INQUIRY data and is 0 for most USB SSDs/HDDs** ([AskUbuntu](https://askubuntu.com/questions/168650/how-do-i-list-all-storage-devices-thumb-drives-external-hd-that-are-), [linuxize](https://linuxize.com/post/lsblk-command-in-linux/)); the transport
  column is the reliable signal and is exactly how lsblk derives it.
- Fallback (lsblk absent, e.g. slim containers): parse `/proc/self/mounts`
  (fields: dev, mountpoint with `\040` unescaping, fstype; exclude
  `squashfs, tmpfs, devtmpfs, overlay, iso9660, zram, loop`, and keep
  mountpoints under `/media` or `/run/media`), map mount device → block device
  (strip partition suffix: `sdb1→sdb`, `nvme0n1p2→nvme0n1`, `mmcblk0p1→mmcblk0`),
  then read `/sys/block/<dev>/removable` and test the sysfs device path for
  `/usb` (`readlink /sys/block/<dev>/device`, same check lsblk's TRAN encodes).
- Free space/fstype: `syscall.Statfs` (Linux `Statfs_t` has both `Bsize` and
  `Frsize`; use `Frsize` when non-zero else `Bsize`, matching `df`).

### 2.3 Windows — x/sys/windows only (already in go.mod)

`windows.GetLogicalDrives()` (bitmask) + `windows.GetDriveType(root)` with
`windows.DRIVE_REMOVABLE`/`DRIVE_FIXED`/`DRIVE_REMOTE`/`DRIVE_CDROM` — all in
`golang.org/x/sys/windows`, pure syscalls, no cgo ([pkg.go.dev](https://pkg.go.dev/golang.org/x/sys/windows), [classic pattern](https://stackoverflow.com/questions/23128148/how-can-i-get-a-listing-of-all-drives-on-windows-using-golang)); free space via
`windows.GetDiskFreeSpaceEx` ([SO](https://stackoverflow.com/questions/20108520/get-amount-of-free-disk-space-using-go)). `StackExchange/wmi` can query
`Win32_LogicalDisk DriveType=2` without cgo (go-ole syscalls) but is
**archived/maintenance-mode** and buys nothing over GetDriveType (same
DriveType data) — not added. **Limitation to display honestly:** many external
USB HDD/SSDs enumerate as `DRIVE_FIXED`; Windows cannot distinguish them
without the Storage WMI namespace (MSFT_PhysicalDisk bus type) — out of scope.
So `drives` lists *all* letters with their type labels, and export's
auto-default only fires when exactly one `DRIVE_REMOVABLE` exists; fixed
drives are listed so the user can `-o E:\...` explicitly. Volume labels:
`GetVolumeInformationW` is not wrapped by x/sys/windows ([docs](https://pkg.go.dev/golang.org/x/sys/windows)); if labels are wanted, bind it via
`windows.NewLazySystemDLL("kernel32.dll").NewProc(...)` in ~12 lines (pure
syscall, no dep) — optional polish.

### 2.4 Strategy table + common fallback

| OS | Enumeration | Externality signal | Free space | Biggest blind spot |
|---|---|---|---|---|
| macOS | `/Volumes` + `diskutil list -plist` (unmounted) | `diskutil info -plist`: `RemovableMediaOrExternalDevice` | `syscall.Statfs` (`Bavail*Bsize`) | parsing depends on Apple's key names (stable 10+ yrs); Ejectable unreliable on TB bridges |
| Linux | `lsblk -Jb` / `/proc/self/mounts` + sysfs | `RM==1` OR `TRAN=="usb"` | `syscall.Statfs` (`Bavail*Frsize`) | `RM` alone misses USB SSDs (hence TRAN); lsblk absent in slim containers |
| Windows | `GetLogicalDrives` | `GetDriveType==DRIVE_REMOVABLE` | `GetDiskFreeSpaceEx` | external USB disks often `DRIVE_FIXED` |

**Common fallback (all OS):** detection failure or empty result is never an
error for the tool — `drives` prints the full mounted-volume table (mountpoint,
label if known, fstype, free/total, how it was classified) with warnings
collected, and `export` without `-o` prints that listing plus
`pass -o <path>` and exits non-zero. No pretending certainty; explicit paths
always work.

### 2.5 Dependencies verdict

**Zero new dependencies.** darwin/linux = stdlib (`os/exec`, `syscall`,
`encoding/json`, `encoding/xml`); windows = `golang.org/x/sys/windows`
(present in go.mod for `golang.org/x/term` already). The one place a dep was
tempting — plist parsing — is handled by a ~100-line hand-rolled XML plist
decoder (`encoding/xml` token walk over `dict/array/string/integer/true/false`)
tested against captured fixtures; [howett.net/plist](https://github.com/DHowett/go-plist) is pure Go and maintained but not worth a dependency for six keys.

## 3. The `.dvca` writer/reader (internal/portable)

### 3.1 Archive layout (HANDOFF §5, binding)

Uncompressed outer tar; blobs are the zstd-compressed CAS bytes verbatim:

```
manifest.json          json.Marshal(m) — byte-stable round-trip of the stored blob
objects/<hash>         raw ObjectPath file bytes, verbatim, NOT OpenObject output
checksums.sha256       "sha256 <hash>  <path>" lines for manifest.json + every object
```

**Trap called out:** export must `os.Open(st.ObjectPath(hash))` and copy the
*file* — `OpenObject` returns a **decompressing** reader and would silently
change every hash (HANDOFF §2).

### 3.2 Writer — `internal/portable/write.go`

```go
type Exporter struct {
    St       *store.Store
    Progress func(done, total int, lastHash string, bytes int64) // nil = silent
}
// Export writes m as a .dvca archive to w. Refuses broken snapshots
// (missingObjects non-empty) before writing a byte. Entries: manifest.json
// first, then one objects/<hash> per m.ObjectHashes() (sorted for
// determinism), checksums.sha256 LAST (it covers the others; tar is append-only).
// tar header sizes come from os.Stat(ObjectPath) — cross-checked against the
// objects table, mismatch → warning + stat wins. File target: caller Sync()s.
func (e *Exporter) Export(m *model.Manifest, w io.Writer) error
```

Caller (CLI) does: `f, _ := os.CreateTemp(dir, ".dvca-*")` → Export →
`f.Sync()` (removable drives cache aggressively; yanking is the norm) → rename
→ progress line every 10 objects. Memory profile: one 32KiB copy buffer —
objects never buffered whole.

### 3.3 Reader — `internal/portable/read.go`

Two passes over the *file* (import always reads a path, so re-opening is free;
this buys pre-flight before writing gigabytes, and order-independence):

```go
// Pass 1 — index without touching the store.
type Index struct {
    Entries      []Entry               // Name ("manifest.json" | "objects/<64-hex>" | "checksums.sha256"), Size
    Manifest     *model.Manifest       // decoded from manifest.json
    ManifestHash string                // sha256 of the manifest.json bytes (== stored manifest_hash)
    Checksums    map[string]string     // path → hash, parsed leniently (optional leading "sha256 " token)
    Kinds        map[string]string     // object hash → kind: image|volume|volindex|bindmount
}
func IndexArchive(path string) (*Index, error)
// Rejects: non-regular/dir/symlink entries, duplicate names, non-hex or
// non-64-char object names, missing manifest.json, missing checksums.sha256,
// manifest bytes that fail json decode or whose canonical re-hash
// (json.Marshal(m) → sha256, same as db.InsertSnapshot) mismatches
// checksums.sha256's manifest.json line. Kinds per HANDOFF §5: images +
// container ImageObjects → image; volume Objects → volume; IndexObjects →
// volindex; bindmounts → bindmount.

// Pass 2 — verify + adopt, streaming, never buffering whole objects.
func Adopt(st *store.Store, ix *Index, progress func(n, total int, hash string)) (adopted, present int, err error)
// For each objects/<hash> entry: stream tar.Reader → io.MultiWriter(sha256, tmpFile);
// sha256(bytes) != filename → abort naming the entry (also covers tar truncation);
// cross-check checksums.sha256 line; then st.AdoptObject(hash, ix.Kinds[hash], n, ...).
// Pre-flight before starting: every m.ObjectHashes() must appear in Entries
// (error names the missing hashes) — export always includes them all.

// Pass 3 — register.
func Register(st *store.Store, ix *Index) error   // ID-collision policy + InsertSnapshot
```

**ID-collision policy (HANDOFF §5, restated and validated against db.go):** if
`snapshots.id` already exists: fetch via `GetSnapshot(m.ID)`, canonically
re-hash the stored manifest (`(*model.Manifest).Hash()`, the exact function
`InsertSnapshot` used to compute `manifest_hash`); same hash → print
`already imported`, exit 0 no-op; different hash → hard error suggesting
`dockervc delete <id>` or `--store <dir>`. Safety of the write path: objects
are adopted before `InsertSnapshot`, whose per-ref inserts are FK-constrained
(`refs.hash → objects.hash`, pragma `foreign_keys(1)`) inside one transaction —
a mid-insert failure rolls back and no snapshot row ever lands; objects adopted
before an abort are valid, self-verifying CAS files that `prune` reclaims (an
orphan, never corruption). That satisfies HANDOFF's "store left clean (no
partial snapshot row)".

### 3.4 New store method — `internal/store/adopt.go`

```go
// AdoptObject places content-addressed bytes into the store VERBATIM: no
// compression, no re-derivation of identity — the hash names exactly these
// bytes, and PutBlob would re-compress and change it (import path only).
// Bytes stream to tmp/, are re-hashed in flight (defense in depth: mismatch →
// error, tmp removed), then rename into objects/<xx>/<hash> and
// INSERT OR IGNORE the objects row with the actual byte count.
// Returns adopted=false on a dedup hit (file already present), where it only
// ensures the row exists.
func (s *Store) AdoptObject(hash, kind string, size int64, src io.Reader) (adopted bool, err error)
```

LOCKED deviation from HANDOFF's `error`-only signature: the bool is needed for
honest "N adopted, M already present" progress. Semantics otherwise exactly
§5 step 3 (tmp/ + rename, `INSERT OR IGNORE`, never PutBlob).

### 3.5 Evaluations (one paragraph each, as required)

**Multi-snapshot archives — defer.** Content-addressing makes dedup free
(objects/ is already share-only-once), but everything else in the basic tier
is single-snapshot-shaped: the frozen §5 layout has exactly one
`manifest.json`, the collision policy, `--apply`, the TUI pick-a-snapshot
flows, and `show`-symmetry all assume one snapshot per file. The future format
is mechanical (`manifests/<id>.json` + an `index.json`; objects/ unchanged)
and the phased reader below makes it additive. Not worth destabilizing the
binding spec now; recommend a 0.7.x follow-up if the user wants "export the
last 5 snapshots".

**`export --latest` — include (basic).** `st.LatestSnapshot()` exists; `--latest`
and a positional arg are mutually exclusive (error if both). It is the
scriptable path for the removable-drive backup story (`dockervc export
--latest -o /Volumes/Backup/`), which is the primary use case of this step.

**`--split 4g` — reject as deferred (per HANDOFF).** Registering the flag and
failing with `--split is deferred; archives are single-file (use exFAT/APFS/NTFS
destinations, or split(1) + cat on import)` costs three lines and matches
§5. What it would eventually entail: multi-volume continuation of the outer
tar byte stream (`state.dvca.001…N` + a size/hash manifest in part 001, or
per-part tars with object-aligned boundaries so each part self-verifies);
import accepting a part list and virtually concatenating. Nothing in the
current reader's phased design precludes it.

## 4. CLI / UX, menu & TUI wiring

### 4.1 New commands (`internal/cli/export.go`, `import.go`, `drives.go`)

- `export`: `Use: "export <snapshot> [-o <file.dvca>]"`, MaximumNArgs(1),
  `needsStore` (already annotated in stubs.go). Flags: `-o/--out string`,
  `--latest bool`, `--split string` (rejected at runtime). Exactly one of
  arg/`--latest` required. Resolution: no `-o` → `drives.List()`; exactly one
  external+ejectable-or-removable+ writable volume → default
  `<mount>/<snapID>.dvca` with a printed notice; zero or many → print the
  volume table + "pass -o <path>", exit 1 (never prompt — HANDOFF §6: no
  stdin reads outside confirm/guided). Best-effort free-space check (sum of
  object sizes + slack vs Statfs) → insufficient = hard error. Refuses broken
  snapshots (same `missingObjects` gate as rollback). `~` expanded via
  `os.UserHomeDir`.
- `import`: `Use: "import <archive.dvca>"`, ExactArgs(1), **add**
  `needsStore: "true"` (missing in the stub) — import requires an initialized
  store (`store.Open` → `ErrNotInitialized` is the natural error; document
  `init` → `import` as the migration path). Flags: `--apply bool`. Flow:
  IndexArchive → pre-flight print (id, date, message, N objects, total size,
  K present locally) → Adopt (progress) → Register → print
  `imported snap-… (N new objects, M already present)` → if `--apply`:
  `dockerapi.New()`+`Ping` → one combined confirm
  (`Import done. Roll the engine back to snap-… now?` — includes the
  rollback's own caveat text via `rollbackPrompt`-style summary) unless
  `--yes` → pre-rollback checkpoint (Capturer, same as rollback.go does) →
  `rollback.FetchLiveState` → `BuildPlan(scope All)` → `rollback.Executor.Apply`.
  Built only from the public rollback API — no changes to rollback.go, which
  the Step 3 agent owns.
- `drives`: `Use: "drives"`, NoArgs, **no** needsStore annotation (hardware
  info, works pre-init). Prints the §2.4 table + warnings; exit 0 even when
  nothing external is found (it's a listing, not a health check — that's also
  why it is not doctor-adjacent: doctor means "store health").

Confirmation conventions: plain `import` is additive like `snapshot` — no
confirm. `import --apply` is destructive-through-the-rollback and gets exactly
one `confirm()` gate honoring `--yes`/`forceYes` (same shared var as
prune/delete/doctor, per maintenance.go/doctor.go). Export writes a new file —
no confirm, but refuse to overwrite an existing `-o` target unless `--yes`.

### 4.2 Menu/TUI wiring (HANDOFF §6 checklist)

- `interactive.go` `menuActions`: update rows to
  `{"export", "<snap> -o <file>", "package a snapshot into a portable .dvca archive", guidedExport}`,
  `{"import", "<archive>", "add an archive's snapshot to this store", guidedImport}`,
  add `{"drives", "", "list removable/external drives", nil}`.
  `guidedExport`: pickSnapshot → promptLine "output path" (default: computed
  from `drives.List()` first hit, else `<snapID>.dvca`) → `["export", id, "-o", path]`.
  `guidedImport`: promptLine "archive path" → promptBool "also roll back to it
  now? (--apply)" → argv. `resetCommandFlags`: reset new `exportOpts` /
  `importOpts` structs (flags leak between TUI runs — bit us before).
- `tui.go` `snapArgSlots`: `"export": {0}` already present — keep; `import`
  takes a file path, deliberately NOT added. File-path Tab-completion: skip
  (the completer is store-snapshot-shaped; mention in docs that `:`-mode
  users type paths).
- `tui.go` `runMenuAction`: `case "export"` → askPick snapshot → askText path
  (pre-filled default from drives.List()) → `t.execTUI("export", id, "-o", path)`;
  `case "import"` → askText path → askBool "--apply?" → execTUI; `case
  "drives"` → `t.doExec("drives")` (read-only, no gate).
- `tui.go` `confirmDestructive`: add `(name == "import" && containsArg(argv,
  "--apply"))`. Also add `name == "rollback"` **if** the Step 3 agent hasn't
  already (their plan §5.8 includes it; coordinate — see §8).
- `printMan` self-updates from `LocalFlags()` — verify only, no change.

## 5. New / changed files, in dependency order

1. `internal/store/adopt.go` + `adopt_test.go` (new) — `AdoptObject`, §3.4.
2. `internal/drives/` (new package):
   - `drives.go` — `type Volume struct { Name, MountPoint, Device, FSType
     string; TotalBytes, FreeBytes uint64; Removable, Ejectable, External,
     Writable bool; Mounted bool; Source string }`;
     `func List() ([]Volume, []string)` (volumes + warnings, warnings never
     fatal); shared filtering/formatting helpers.
   - `drives_darwin.go` (`//go:build darwin`) — /Volumes scan + Statfs +
     `diskutil info -plist` / `diskutil list -plist` (unmounted pass).
   - `drives_linux.go` (`//go:build linux`) — lsblk -Jb + sysfs/mounts fallback.
   - `drives_windows.go` (`//go:build windows`) — GetLogicalDrives /
     GetDriveType / GetDiskFreeSpaceEx (+ optional lazy
     GetVolumeInformationW label helper).
   - `space_unix.go` (`//go:build darwin || linux`) — Statfs wrapper
     (Bsize on darwin — its `Statfs_t` has no `Frsize`, verified via
     `go doc syscall.Statfs_t`; Frsize-else-Bsize on linux).
   - `plist.go` — minimal XML plist decoder (dict/array/string/integer/
     true/false → `map[string]any`).
   - tests: `plist_test.go`, `drives_test.go` (fixture-driven, §6).
3. `internal/portable/` (new package): `write.go` (Exporter), `read.go`
   (IndexArchive / Adopt / Register), `export_test.go`, `import_test.go`.
   Phased deliberately so import-as-repair (§7) is additive later.
4. `internal/cli/export.go` (new), `internal/cli/import.go` (new) — delete the
   two stubs and the Step-4 comment from `internal/cli/stubs.go`.
5. `internal/cli/drives.go` (new) — the `drives` command.
6. `internal/cli/interactive.go` — menu rows, guidedExport/guidedImport,
   resetCommandFlags additions.
7. `internal/cli/tui.go` — runMenuAction cases, confirmDestructive addition.
8. Docs + version: README status row ("Export/import + drives ✅", quick-start
   gains an export/import example), DESIGN.md §5/§6 Step 4 → done + note
   deviations (AdoptObject bool return; GNU-vs-prefixed checksums per §8),
   HANDOFF.md §5 → done + deviations. Makefile `VERSION` → 0.6.0.

Store changes: the one method in (1) — nothing else in internal/store.

## 6. Test plan

Unit (`go test ./...`, no engine, no hardware):

- `adopt_test.go`: round-trip PutBlob("volume") → read `ObjectPath` file →
  AdoptObject(same hash, other kind, verbatim bytes) → row hash/size unchanged,
  `adopted==true`; second AdoptObject → `adopted==false`, still one file;
  mismatched bytes → error, no file, no row.
- `drives` parsers against captured fixtures:
  - macOS: the real `diskutil info -plist /Volumes/Macintosh HD` XML captured
    on this machine (§2.1) — asserts Internal=true → excluded; a hand-edited
    external twin (VolumeName "Backup", MountPoint /Volumes/Backup,
    Internal=false, RemovableMediaOrExternalDevice=true, Ejectable=true,
    BusProtocol USB, FilesystemType exfat) → included, ejectable; a plist
    missing keys entirely → warning, not panic.
  - Linux: lsblk -Jb fixture JSON with three disks — sda (rm 0, tran sata →
    excluded), sdb (rm 1, tran usb, mounted /media/user/USB → included),
    sdc (rm **0**, tran usb → included via TRAN — the regression case);
    `/proc/self/mounts` fixture + fake `/sys/block` tree in t.TempDir() for
    the fallback path; `\040` unescaping test.
  - plist decoder: nested arrays/dicts, integers, true/false, empty string.
- `portable` round-trip (mirrors store_test.go conventions): build a real
  mini-store (two PutBlob objects + one volindex + manifest via
  InsertSnapshot) → Export to a temp file → open a **fresh** store in another
  tmpdir → Import → identical `ListSnapshots` rows, identical `AllObjects`
  hashes/sizes, `show`-equivalent manifest equality, manifest_hash equality
  (cross-machine collision semantics).
- Corruption: rebuild the tar with one flipped byte in an objects entry →
  Import fails naming `objects/<hash>`, no snapshot row, prior adopted objects
  present-but-unreferenced (assert exactly that); truncated file; unexpected
  entry (`evil.txt`); duplicate object entry; non-hex hash name; missing
  checksums.sha256; manifest.json tampered (checksum mismatch).
- Collision: import twice → second run "already imported", exit 0, one row;
  same ID different manifest (rebuild archive with edited manifest) → hard error.
- Pre-flight: archive missing one referenced object → error naming it, nothing
  written. Export refusing a broken snapshot (delete one object file first).
- CLI: extend the runargs_test.go pattern — `runArgs("export", …)` with
  `DOCKERVC_HOME` tmpdir store; `resetCommandFlags` hygiene for the new opts.
- Windows: compile-only (`GOOS=windows go build ./...`, already in `make
  dist`); drives_windows has no runtime tests on this Mac — the fixture tests
  cover the parsers, and the winapi path is small and reviewable.

Manual checklist (needs real hardware; report real output per HANDOFF §7):

1. Insert a USB stick; `dockervc drives` shows it with correct free space on
   macOS (and in a Windows VM / Linux box if available).
2. `dockervc export --latest` with no `-o` → defaults to the stick; verify
   `tar tf` shows the three entry kinds; yank-recheck: file synced (re-insert,
   `shasum` the archive).
3. Free-space guard: export onto a nearly-full stick → clean error.
4. Import on a second machine/VM (`init` → `import` → `show` → `rollback
   --dry-run`); `import --apply` end-to-end on the demo env.
5. exFAT and FAT32 destinations (FAT32: document the 4GiB single-file limit →
   points to deferred `--split`); NTFS on Windows.
6. TUI: export/import/drives via menu + `:`-mode; `import --apply` shows the
   y/n dialog, not a hang; `export <Tab>` still completes snapshot ids.

## 7. Import-as-repair — recommendation: follow-up, not basic

A `doctor --repair --from <archive-or-dir>` heals broken snapshots (re-adopt
missing objects from an export or a plain store copy — layouts are identical
since objects live at `objects/<xx>/<hash>` in both) instead of deleting them;
snapshot rows recover automatically because `doctor`'s `missingObjects` is a
stat of files that simply start existing again. The phased reader makes this
cheap: repair = `IndexArchive` (or walking the foreign store dir) →
`ix.MissingObjects(st)` → `Adopt` subset → **no** Register (rows already
exist) → re-run `diagnose`. Estimated at ~120–150 lines plus tests in
doctor.go/portable — beyond that, `--from <dir>` needs its own walker (object
files are self-naming, so verification is the same stream-hash) and doctor's
repair menu grows a fifth option. That is real but not free, and Step 4 basic
is already sizeable; the phased API above is designed so the follow-up adds a
file, not a refactor. **Recommend: defer to the next minor (0.7.x); design
hook lands now via the phased portable API.**

## 8. Open questions for the user (recommendations given)

1. **checksums.sha256 line format** — HANDOFF §5 literally specifies
   `"sha256 <hash>  <path>"`. Dropping the leading `sha256 ` token makes the
   file directly verifiable with `sha256sum -c` / `shasum -a 256 -c` after
   `tar xf`. Recommendation: write GNU-compatible `<hash>  <path>` (parser
   accepts both shapes regardless). Plan defaults to the HANDOFF-literal
   format unless you approve the deviation.
2. **`dockervc drives` as its own command + menu row** — recommended yes (it
   is portability UX, not store health, so not doctor-adjacent); say the word
   if you'd rather keep the command surface minimal.
3. **Unmounted external disks on macOS** — list-with-mount-hint only
   (recommended), or auto-mount via `diskutil mountDisk` behind a confirm?
4. **Windows fixed-disk externals** — acceptable that `DRIVE_REMOVABLE`
   drives auto-default for `-o` while USB HDD/SSDs (usually `DRIVE_FIXED`)
   must be chosen explicitly via `-o E:\…` (they're still listed)? Digging
   further needs WMI/MSFT_PhysicalDisk.
5. **Coordination with Step 3** — both steps touch `interactive.go`/`tui.go`
   (menu rows, confirmDestructive, resetCommandFlags). Recommendation: land
   Step 4 after rollback merges; this plan's edits are disjoint from
   ROLLBACK-PLAN §5.7–5.8 but sit in the same regions.
6. **Import-as-repair** — confirm the §7 defer (or pull `--from <archive>`
   only, dropping `--from <dir>, into basic if the broken-snapshot demo
   scenario matters to you now).

Sources for the research claims: [golang.org/x/sys/windows docs](https://pkg.go.dev/golang.org/x/sys/windows), [drive enumeration on Windows (SO)](https://stackoverflow.com/questions/23128148/how-can-i-get-a-listing-of-all-drives-on-windows-using-golang), [disk free space via GetDiskFreeSpaceEx (SO)](https://stackoverflow.com/questions/20108520/get-amount-of-free-disk-space-using-go), [StackExchange/wmi (archived; go-ole, no CGO)](https://github.com/stackexchange/wmi), [Win32_LogicalDisk DriveType](https://learn.microsoft.com/en-us/windows/win32/cimwin32prov/win32-logicaldisk), [GetVolumeInformation (not wrapped by x/sys)](https://learn.microsoft.com/en-us/windows/win32/api/fileapi/nf-fileapi-getvolumeinformationa), [removable flag misses USB SSDs (AskUbuntu)](https://askubuntu.com/questions/168650/how-do-i-list-all-storage-devices-thumb-drives-external-hd-that-are-), [lsblk RM vs TRAN (linuxize)](https://linuxize.com/post/lsblk-command-in-linux/), [howett.net/go-plist](https://github.com/DHowett/go-plist), [gousbdrivedetector (abandoned)](https://github.com/deepakjois/gousbdrivedetector), [ejectable-flag quirks on Thunderbolt (SO)](https://stackoverflow.com/questions/38499860/).
