// doctor: the store health check. The default pass is structural and instant
// — snapshots whose object files are missing, orphaned catalog rows, untracked
// files — while --deep additionally re-hashes every object to catch silent
// corruption (bit rot, truncation, tampering). --repair interactively fixes
// what it finds. `verify` remains as a deprecated alias for --deep.
package cli

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"dockervc/internal/model"
	"dockervc/internal/snapshot"
	"dockervc/internal/store"
)

// snapHealth is one snapshot's diagnosis.
type snapHealth struct {
	ID       string
	Message  string
	Created  string // formatted timestamp
	Missing  []string
	Corrupt  []string // files present but content no longer hashes right (deep only)
	ParseErr bool     // manifest row couldn't be read/parsed at all
}

func (h *snapHealth) reason() string {
	if h.ParseErr {
		return "manifest unreadable"
	}
	var parts []string
	if n := len(h.Missing); n > 0 {
		parts = append(parts, fmt.Sprintf("%d object file%s missing", n, pluralIf("", "s", n > 1)))
	}
	if n := len(h.Corrupt); n > 0 {
		parts = append(parts, fmt.Sprintf("%d corrupted", n))
	}
	return strings.Join(parts, ", ")
}

// corruptedObject is a stored object whose bytes no longer match its hash.
type corruptedObject struct {
	Hash   string
	Reason string
}

// untrackedFile is a file under objects/ with no catalog row (crash debris,
// manual copies — the store will never reference it).
type untrackedFile struct {
	Path string // store-relative
	Size int64
}

// doctorReport is the full diagnosis.
type doctorReport struct {
	DBOK        bool
	Deep        bool
	SnapsTotal  int
	HashChecked int                // objects re-hashed (deep pass only)
	Broken      []*snapHealth      // snapshots missing data or (deep) resting on corruption
	Corrupted   []corruptedObject  // deep pass only
	Orphans     []model.ObjectInfo // catalog rows no snapshot references
	Untracked   []untrackedFile    // files no catalog row owns
}

func (r *doctorReport) problemCount() int {
	n := len(r.Broken) + len(r.Orphans) + len(r.Untracked) + len(r.Corrupted)
	if !r.DBOK {
		n++
	}
	return n
}

func (r *doctorReport) Healthy() bool { return r.problemCount() == 0 }

var (
	doctorRepair bool
	doctorDeep   bool
)

var doctorCmd = &cobra.Command{
	Use:   "doctor [--deep] [--repair]",
	Short: "Diagnose store health; --deep re-hashes objects, --repair fixes what it finds",
	Args:  cobra.NoArgs,
	Annotations: map[string]string{needsStore: "true"},
	RunE: func(cmd *cobra.Command, args []string) error {
		rep, err := diagnose(st, doctorDeep)
		if err != nil {
			return err
		}
		printDoctorReport(cmd.OutOrStdout(), st, rep)

		if !doctorRepair {
			if !rep.Healthy() {
				return fmt.Errorf("%d problem(s) found — run `dockervc doctor --repair` to fix them", rep.problemCount())
			}
			if !doctorDeep {
				fmt.Println("\ntip: `dockervc doctor --deep` additionally re-hashes every object's content.")
			}
			return nil
		}

		ask := confirm
		if forceYes {
			ask = func(string) bool { return true }
		}
		if err := applyRepairs(st, rep, ask, cmd.OutOrStdout()); err != nil {
			return err
		}

		rep, err = diagnose(st, doctorDeep)
		if err != nil {
			return err
		}
		if !rep.Healthy() {
			return fmt.Errorf("repair finished, %d problem(s) remain (declined or failed fixes)", rep.problemCount())
		}
		fmt.Println("\nstore is healthy.")
		return nil
	},
}

// diagnose collects every structural problem and, when deep, every content
// problem too. The structural pass only stats files — no hashing, no blob
// reads — so it stays instant even on large stores.
func diagnose(s *store.Store, deep bool) (*doctorReport, error) {
	rep := &doctorReport{Deep: deep}
	rep.DBOK = s.QuickCheck() == nil

	rows, err := s.ListSnapshots()
	if err != nil {
		return nil, err
	}
	rep.SnapsTotal = len(rows)

	// Structural pass. Manifests are cached for the deep pass below, and
	// broken-by-id so the deep pass can merge into existing entries instead
	// of double-listing a snapshot.
	manifests := map[string]*model.Manifest{}
	brokenByID := map[string]*snapHealth{}
	for _, row := range rows {
		m, err := s.GetSnapshot(row.ID)
		if err != nil {
			h := &snapHealth{
				ID: row.ID, Message: row.Message,
				Created: row.CreatedAt.Local().Format("2006-01-02 15:04"),
				ParseErr: true,
			}
			rep.Broken = append(rep.Broken, h)
			brokenByID[row.ID] = h
			continue
		}
		manifests[row.ID] = m
		if missing := missingObjects(s, m); len(missing) > 0 {
			h := &snapHealth{
				ID: row.ID, Message: row.Message,
				Created: row.CreatedAt.Local().Format("2006-01-02 15:04"),
				Missing: missing,
			}
			rep.Broken = append(rep.Broken, h)
			brokenByID[row.ID] = h
		}
	}

	// Deep pass: re-hash every object, then mark the snapshots that rest on
	// corrupted bytes as broken too — they cannot be restored.
	if deep {
		corrupted, checked, err := hashCheck(s)
		if err != nil {
			return nil, err
		}
		rep.Corrupted, rep.HashChecked = corrupted, checked
		if len(corrupted) > 0 {
			bad := map[string]bool{}
			for _, c := range corrupted {
				bad[c.Hash] = true
			}
			for _, row := range rows {
				m := manifests[row.ID]
				if m == nil {
					continue // ParseErr rows are already broken
				}
				var corrupt []string
				for _, h := range m.ObjectHashes() {
					if bad[h] {
						corrupt = append(corrupt, h)
					}
				}
				if len(corrupt) == 0 {
					continue
				}
				if h := brokenByID[row.ID]; h != nil {
					h.Corrupt = corrupt // merge: one entry per snapshot
					continue
				}
				rep.Broken = append(rep.Broken, &snapHealth{
					ID: row.ID, Message: row.Message,
					Created: row.CreatedAt.Local().Format("2006-01-02 15:04"),
					Corrupt: corrupt,
				})
			}
		}
	}

	if rep.Orphans, err = s.UnreferencedObjects(); err != nil {
		return nil, err
	}
	if rep.Untracked, err = findUntracked(s); err != nil {
		return nil, err
	}
	return rep, nil
}

// hashCheck re-hashes every catalog object file and returns the ones whose
// bytes no longer match their content hash (or are unreadable), plus how many
// were checked. This is the expensive pass doctor's structural checks avoid.
func hashCheck(s *store.Store) ([]corruptedObject, int, error) {
	objs, err := s.AllObjects()
	if err != nil {
		return nil, 0, err
	}
	var bad []corruptedObject
	checked := 0
	for _, o := range objs {
		if _, statErr := os.Stat(s.ObjectPath(o.Hash)); statErr != nil {
			continue // absent files are the structural pass's finding, not corruption
		}
		checked++
		got, size, err := store.HashFile(s.ObjectPath(o.Hash))
		if err != nil {
			bad = append(bad, corruptedObject{Hash: o.Hash, Reason: fmt.Sprintf("unreadable: %v", err)})
			continue
		}
		if got != o.Hash || size != o.Size {
			bad = append(bad, corruptedObject{Hash: o.Hash,
				Reason: fmt.Sprintf("stored %s/%d, on disk %s/%d", o.Hash, o.Size, got, size)})
		}
	}
	return bad, checked, nil
}

// missingObjects returns the referenced object hashes whose files are absent
// (stat only — no hashing). Shared by doctor and the `log` broken marker.
func missingObjects(s *store.Store, m *model.Manifest) []string {
	var missing []string
	for _, h := range m.ObjectHashes() {
		if _, err := os.Stat(s.ObjectPath(h)); err != nil {
			missing = append(missing, h)
		}
	}
	return missing
}

// BrokenSuffix returns the `log` marker for a snapshot row: "" when healthy.
// It is deliberately structural (stat only) so `log` stays fast; content
// corruption is `doctor --deep`'s job.
func BrokenSuffix(s *store.Store, row store.SnapshotRow) string {
	m, err := s.GetSnapshot(row.ID)
	if err != nil {
		return "✗ BROKEN (manifest unreadable)"
	}
	if n := len(missingObjects(s, m)); n > 0 {
		return fmt.Sprintf("✗ BROKEN (%d object%s missing)", n, pluralIf("", "s", n > 1))
	}
	return ""
}

// findUntracked lists files under objects/ that no catalog row owns.
func findUntracked(s *store.Store) ([]untrackedFile, error) {
	known := map[string]bool{}
	objs, err := s.AllObjects()
	if err != nil {
		return nil, err
	}
	for _, o := range objs {
		known[o.Hash] = true
	}
	var out []untrackedFile
	root := filepath.Join(s.Path, "objects")
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil // unreadable entries are reported by other means
		}
		if known[d.Name()] {
			return nil
		}
		rel := p
		if r, err := filepath.Rel(s.Path, p); err == nil {
			rel = r
		}
		var size int64
		if info, err := d.Info(); err == nil {
			size = info.Size()
		}
		out = append(out, untrackedFile{Path: rel, Size: size})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

func printDoctorReport(w io.Writer, s *store.Store, rep *doctorReport) {
	fmt.Fprintf(w, "store: %s\n", s.Path)
	if rep.DBOK {
		fmt.Fprintln(w, "catalog: OK")
	} else {
		fmt.Fprintln(w, "catalog: FAILED SQLite quick_check")
	}
	fmt.Fprintf(w, "snapshots: %d checked", rep.SnapsTotal)
	if len(rep.Broken) == 0 {
		fmt.Fprintln(w, ", all reference their objects")
	} else {
		fmt.Fprintf(w, ", %d broken:\n", len(rep.Broken))
		for _, b := range rep.Broken {
			fmt.Fprintf(w, "  ✗ %s  %s  %q — %s\n", b.ID, b.Created, b.Message, b.reason())
		}
	}
	if rep.Deep {
		if len(rep.Corrupted) == 0 {
			fmt.Fprintf(w, "content: %d object(s) re-hashed, all intact\n", rep.HashChecked)
		} else {
			fmt.Fprintf(w, "content: %d of %d object(s) CORRUPTED:\n",
				len(rep.Corrupted), rep.HashChecked)
			for i, c := range rep.Corrupted {
				if i >= 5 {
					fmt.Fprintf(w, "  … %d more\n", len(rep.Corrupted)-i)
					break
				}
				fmt.Fprintf(w, "  ✗ %s  %s\n", shortID(c.Hash), c.Reason)
			}
		}
	} else {
		fmt.Fprintln(w, "content: not checked (`--deep` re-hashes every object)")
	}
	var orphanBytes int64
	for _, o := range rep.Orphans {
		orphanBytes += o.Size
	}
	fmt.Fprintf(w, "objects: %d unreferenced row(s) (%s)", len(rep.Orphans), snapshot.HumanBytes(orphanBytes))
	var untrackedBytes int64
	for _, f := range rep.Untracked {
		untrackedBytes += f.Size
	}
	fmt.Fprintf(w, ", %d untracked file(s) (%s)\n", len(rep.Untracked), snapshot.HumanBytes(untrackedBytes))
}

// applyRepairs walks the repair options, asking before each destructive
// action. Declining one option never blocks the others.
func applyRepairs(s *store.Store, rep *doctorReport, ask func(string) bool, out io.Writer) error {
	if len(rep.Broken) > 0 {
		fmt.Fprintf(out, "\nBroken snapshots cannot be restored locally (their data is\n")
		fmt.Fprintf(out, "missing or corrupted); the only repair is deleting them:\n")
		for _, b := range rep.Broken {
			fmt.Fprintf(out, "  %s  %q\n", b.ID, b.Message)
		}
		if ask(fmt.Sprintf("Delete these %d broken snapshot(s)", len(rep.Broken))) {
			n := 0
			for _, b := range rep.Broken {
				if err := s.DeleteSnapshot(b.ID); err != nil {
					fmt.Fprintf(out, "failed to delete %s: %v\n", b.ID, err)
					continue
				}
				n++
			}
			fmt.Fprintf(out, "Deleted %d snapshot(s).\n", n)
		} else {
			fmt.Fprintln(out, "Keeping broken snapshot(s) — they will keep failing verify/restore.")
		}
	}

	// Re-query: deleting snapshots may have orphaned their objects too (this
	// also sweeps corrupted rows once nothing references them).
	orphans, err := s.UnreferencedObjects()
	if err != nil {
		return err
	}
	if len(orphans) > 0 {
		var total int64
		for _, o := range orphans {
			total += o.Size
		}
		if ask(fmt.Sprintf("Remove %d unreferenced catalog row(s) (%s)", len(orphans), snapshot.HumanBytes(total))) {
			var fail int
			for _, o := range orphans {
				if err := s.DeleteObject(o.Hash); err != nil {
					fmt.Fprintf(out, "failed to remove %s: %v\n", o.Hash[:12], err)
					fail++
				}
			}
			fmt.Fprintf(out, "Removed %d row(s)%s.\n", len(orphans)-fail, pluralIf("", " (some failed)", fail > 0))
		}
	}

	if len(rep.Untracked) > 0 {
		var total int64
		for _, f := range rep.Untracked {
			total += f.Size
		}
		fmt.Fprintln(out, "\nUntracked files (in objects/, not in the catalog):")
		for i, f := range rep.Untracked {
			if i >= 5 {
				fmt.Fprintf(out, "  … %d more\n", len(rep.Untracked)-i)
				break
			}
			fmt.Fprintf(out, "  %s  %s\n", f.Path, snapshot.HumanBytes(f.Size))
		}
		if ask(fmt.Sprintf("Delete these %d untracked file(s) (%s)", len(rep.Untracked), snapshot.HumanBytes(total))) {
			var fail int
			for _, f := range rep.Untracked {
				if err := os.Remove(filepath.Join(s.Path, f.Path)); err != nil {
					fmt.Fprintf(out, "failed to delete %s: %v\n", f.Path, err)
					fail++
				}
			}
			fmt.Fprintf(out, "Deleted %d file(s)%s.\n", len(rep.Untracked)-fail, pluralIf("", " (some failed)", fail > 0))
		}
	}

	s.CleanEmptyShards()
	return nil
}

func pluralIf(single, multi string, useMulti bool) string {
	if useMulti {
		return multi
	}
	return single
}

func init() {
	doctorCmd.Flags().BoolVar(&doctorRepair, "repair", false,
		"offer to delete broken snapshots, orphaned rows and untracked files")
	doctorCmd.Flags().BoolVar(&doctorDeep, "deep", false,
		"additionally re-hash every object to detect content corruption")
	doctorCmd.Flags().BoolVarP(&forceYes, "yes", "y", false, "skip confirmation (with --repair)")
	rootCmd.AddCommand(doctorCmd)
}
