package cli

import (
	"bufio"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dockervc/internal/store"
)

// seedPathDir creates a throwaway directory with known entries for
// completion tests: alpha.dvca, beta.dvca, sub/ and a .hidden file.
func seedPathDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, name := range []string{"alpha.dvca", "beta.dvca", ".hidden"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestPathMatches(t *testing.T) {
	dir := seedPathDir(t)
	join := func(names ...string) []string {
		var out []string
		for _, n := range names {
			out = append(out, filepath.Join(dir, n))
		}
		return out
	}

	if ms := pathMatches(filepath.Join(dir, "alpha")); len(ms) != 1 || ms[0] != join("alpha.dvca")[0] {
		t.Fatalf("unique prefix: got %v", ms)
	}
	// both .dvca files, sorted
	ms := pathMatches(filepath.Join(dir, "b"))
	if len(ms) != 1 || ms[0] != join("beta.dvca")[0] {
		t.Fatalf("beta prefix: got %v", ms)
	}
	// listing a directory: trailing slash keeps completion walking into it
	if ms := pathMatches(filepath.Join(dir, "s")); len(ms) != 1 || !strings.HasSuffix(ms[0], "sub/") {
		t.Fatalf("dir match must end with '/': got %v", ms)
	}
	// a trailing slash lists the directory (dotfiles only on '.'+prefix);
	// built by concat because filepath.Join would strip the slash.
	all := pathMatches(dir + "/")
	if len(all) != 4 { // alpha.dvca, beta.dvca, .hidden, sub/
		t.Fatalf("dir listing: got %v", all)
	}
	// raw concat — filepath.Join would collapse the trailing "/."
	if ms := pathMatches(dir + "/."); len(ms) != 1 || !strings.HasSuffix(ms[0], ".hidden") {
		t.Fatalf("dot prefix must match dotfiles: got %v", ms)
	}
	if ms := pathMatches(filepath.Join(dir, "zzz")); ms != nil {
		t.Fatalf("no match: got %v", ms)
	}
	if ms := pathMatches(filepath.Join(dir, "no-such-dir", "x")); ms != nil {
		t.Fatalf("unreadable dir: got %v", ms)
	}
}

func TestPathToken(t *testing.T) {
	cases := []struct {
		toks []string
		want bool
	}{
		{[]string{"import", "/tmp/a.dvca"}, true},
		{[]string{"import", "/tmp/a"}, true},
		{[]string{"export", "snap-1", "-o", "/tmp/a"}, true},
		{[]string{"export", "snap-1", "--output", "/tmp/a"}, true},
		{[]string{"export", "snap-1", "-o"}, false}, // the flag itself, not its value
		{[]string{"export", "/tmp/a"}, false},       // export's positional is a snapshot
		{[]string{"show", "snap-1"}, false},         // snapshot slot, not a path
		{[]string{"config", "set", "export.folder", "/tmp/ex"}, true},
		{[]string{"config", "set", "export.folder"}, false},   // no value yet
		{[]string{"config", "unset", "export.folder"}, false}, // unset takes no path
		{[]string{"config", "set", "zstd_level", "5"}, false}, // not every value is a path
		{[]string{"import"}, false},                           // nothing typed yet
	}
	for _, c := range cases {
		if got := pathToken(c.toks); got != c.want {
			t.Fatalf("pathToken(%v) = %v, want %v", c.toks, got, c.want)
		}
	}
}

func TestColonModePathCompletion(t *testing.T) {
	dir := seedPathDir(t)
	tt := &tui{w: 80, h: 24, out: bufio.NewWriter(io.Discard)}

	// import + partial archive path → path candidates, no snapshot candidates
	tt.input = "import " + filepath.Join(dir, "alp")
	tt.updateCompletions()
	if len(tt.comp) != 0 {
		t.Fatalf("snapshot candidates leaked into a path slot: %v", tt.comp)
	}
	if len(tt.pathComp) != 1 || !strings.HasSuffix(tt.pathComp[0], "alpha.dvca") {
		t.Fatalf("pathComp = %v", tt.pathComp)
	}
	tt.tabComplete()
	if !strings.HasSuffix(tt.input, "alpha.dvca") {
		t.Fatalf("Tab did not complete to alpha.dvca: %q", tt.input)
	}

	// export -o value completes too
	tt.input = "export snap-1 -o " + filepath.Join(dir, "be")
	tt.updateCompletions()
	tt.tabComplete()
	if !strings.HasSuffix(tt.input, "beta.dvca") {
		t.Fatalf("export -o completion failed: %q", tt.input)
	}

	// the snapshot slot of export still completes snapshots, not paths
	tt.snaps = []store.SnapshotRow{{ID: "snap-1111-aaaa", Message: "one"}}
	tt.input = "export snap-1"
	tt.updateCompletions()
	if len(tt.pathComp) != 0 || len(tt.comp) != 1 {
		t.Fatalf("snapshot slot: comp=%v pathComp=%v", tt.comp, tt.pathComp)
	}

	// candidates render with a path header
	tt.input = "import " + dir + "/"
	tt.updateCompletions()
	rows := tt.candidateLines(10)
	if len(rows) == 0 || !strings.Contains(rows[0][0].(string), "path match") {
		t.Fatalf("candidate rows = %v", rows)
	}
}

func TestDefaultExportPath(t *testing.T) {
	seedStore(t, 1)
	if got := defaultExportPath("snap-1"); got != filepath.Join("exports", "snap-1.dvca") {
		t.Fatalf("default: exports/ under the current directory, got %q", got)
	}
	dir := t.TempDir()
	s, err := store.Open(storePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ConfigSet("export.folder", dir); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if got := defaultExportPath("snap-1"); got != filepath.Join(dir, "snap-1.dvca") {
		t.Fatalf("configured export.folder should win, got %q", got)
	}
}

func TestFindArchives(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	seedStore(t, 1)
	dl := filepath.Join(home, "Downloads")
	if err := os.Mkdir(dl, 0o755); err != nil {
		t.Fatal(err)
	}
	oldA := filepath.Join(dl, "old.dvca")
	newB := filepath.Join(dl, "new.dvca")
	for _, p := range []string{oldA, newB, filepath.Join(dl, "ignore.txt")} {
		if err := os.WriteFile(p, []byte("12345"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mustChtime(t, oldA, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	mustChtime(t, newB, time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC))

	// a configured export folder contributes archives too — newest of all
	cfg := t.TempDir()
	newest := filepath.Join(cfg, "newest.dvca")
	if err := os.WriteFile(newest, []byte("12345678"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustChtime(t, newest, time.Date(2026, 9, 12, 0, 0, 0, 0, time.UTC))
	s, err := store.Open(storePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ConfigSet("export.folder", cfg); err != nil {
		t.Fatal(err)
	}
	s.Close()

	items := findArchives()
	if len(items) != 3 {
		t.Fatalf("want the three .dvca files, got %v", items)
	}
	if items[0].value != newest || items[1].value != newB || items[2].value != oldA {
		t.Fatalf("newest first: got %v", items)
	}
	if !strings.Contains(items[2].note, "~/Downloads") || !strings.Contains(items[2].note, "5 B") {
		t.Fatalf("note should carry size and ~-shortened location, got %q", items[2].note)
	}
}

func TestValidateConfigExportFolder(t *testing.T) {
	dir := t.TempDir()
	if err := validateConfig("export.folder", dir); err != nil {
		t.Fatalf("existing directory must pass: %v", err)
	}
	file := filepath.Join(dir, "afile")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := validateConfig("export.folder", file); err == nil {
		t.Fatal("a file must be rejected")
	}
	if err := validateConfig("export.folder", filepath.Join(dir, "missing")); err == nil {
		t.Fatal("a missing path must be rejected")
	}
	if err := validateConfig("bogus", "x"); err == nil {
		t.Fatal("unknown keys must be rejected")
	}
}

func mustChtime(t *testing.T, path string, tm time.Time) {
	t.Helper()
	if err := os.Chtimes(path, tm, tm); err != nil {
		t.Fatal(err)
	}
}

func TestAskImportArchiveFlow(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dl := filepath.Join(home, "Downloads")
	if err := os.Mkdir(dl, 0o755); err != nil {
		t.Fatal(err)
	}
	arch := filepath.Join(dl, "found.dvca")
	if err := os.WriteFile(arch, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	seedStore(t, 1) // import runs for real below; keep it off the real store

	tt := &tui{w: 80, h: 24, out: bufio.NewWriter(io.Discard)}
	tt.refresh()
	tt.askImportArchive()

	d := tt.dlg
	if d == nil || d.kind != dlgList || d.multi || len(d.items) != 2 {
		t.Fatalf("expected a 2-row single-choice list (archive + manual), got %+v", d)
	}
	if d.items[0].value != arch || d.items[1].value != "" {
		t.Fatalf("rows = %+v", d.items)
	}

	// Enter picks the archive → path dialog pre-filled with it → Enter
	// accepts → --apply question → 'n' → the real import runs (and fails on
	// the fake archive — the flow is what's under test).
	tt.dialogKey(0, keyEnter)
	d = tt.dlg
	if d == nil || !d.path || d.def != arch {
		t.Fatalf("expected a path dialog defaulting to the picked archive, got %+v", d)
	}
	tt.dialogKey(0, keyEnter)
	if tt.dlg == nil || tt.dlg.kind != dlgBool || !strings.Contains(tt.dlg.title, "--apply") {
		t.Fatalf("expected the --apply question, got %+v", tt.dlg)
	}
	tt.dialogKey('n', keyNone)
	if tt.dlg != nil || !tt.inOutput || tt.outTitle != "import "+arch {
		t.Fatalf("import should have run: dlg=%v outTitle=%q", tt.dlg, tt.outTitle)
	}

	// Esc at the list cancels without opening any dialog
	tt.dlg = nil
	tt.askImportArchive()
	tt.dialogKey(0, keyEsc)
	if tt.dlg != nil {
		t.Fatalf("Esc must cancel, got %+v", tt.dlg)
	}
}

func TestPathDialogTabCompletes(t *testing.T) {
	dir := seedPathDir(t)
	tt := &tui{w: 80, h: 24, out: bufio.NewWriter(io.Discard)}
	tt.askPath("archive path (.dvca)", "", func(val string, ok bool) {})
	d := tt.dlg
	if d == nil || !d.path {
		t.Fatalf("askPath must open a path dialog, got %+v", d)
	}

	for _, r := range filepath.Join(dir, "alp") {
		tt.dialogKey(r, keyNone)
	}
	tt.dialogKey(0, keyTab)
	if !strings.HasSuffix(d.value, "alpha.dvca") {
		t.Fatalf("dialog Tab completion failed: %q", d.value)
	}

	// the matches render above the input line while typing
	d.value = filepath.Join(dir, "b")
	rows := tt.dialogView()
	found := false
	for _, row := range rows {
		if strings.Contains(row[0].(string), "beta.dvca") {
			found = true
		}
	}
	if !found {
		t.Fatalf("dialog candidates not rendered: %v", rows)
	}
}

func TestConfigUnsetCommand(t *testing.T) {
	seedStore(t, 1)
	dir := t.TempDir()
	out := captureOutput(func() { runArgs("config", "set", "export.folder", dir) })
	if !strings.Contains(out, "export.folder = "+dir) {
		t.Fatalf("set output: %q", out)
	}
	if got := defaultExportPath("snap-x"); got != filepath.Join(dir, "snap-x.dvca") {
		t.Fatalf("default export path after set: %q", got)
	}

	out = captureOutput(func() { runArgs("config", "unset", "export.folder") })
	if !strings.Contains(out, "export.folder unset") {
		t.Fatalf("unset output: %q", out)
	}
	if got := defaultExportPath("snap-x"); got != filepath.Join("exports", "snap-x.dvca") {
		t.Fatalf("default export path after unset: %q", got)
	}

	// unknown keys are refused; unsetting twice is fine
	out = captureOutput(func() { runArgs("config", "unset", "bogus") })
	if !strings.Contains(out, `unknown setting "bogus"`) {
		t.Fatalf("unknown key output: %q", out)
	}
	captureOutput(func() { runArgs("config", "unset", "export.folder") })
}

func TestAskConfigFlow(t *testing.T) {
	seedStore(t, 1)
	tt := &tui{w: 80, h: 24, out: bufio.NewWriter(io.Discard)}
	tt.refresh()

	// nothing set yet: two rows (show / set), no reset row
	tt.askConfig()
	d := tt.dlg
	if d == nil || d.kind != dlgList || d.multi || len(d.items) != 2 {
		t.Fatalf("expected a 2-row single-choice list, got %+v", d)
	}

	// "set export folder…" → path dialog (Tab-completing) → Enter runs the
	// real config set against the seeded store
	dir := t.TempDir()
	tt.dialogKey(0, keyDown) // cursor → row 1 (set…)
	tt.dialogKey(0, keyEnter)
	d = tt.dlg
	if d == nil || !d.path {
		t.Fatalf("expected a path dialog, got %+v", d)
	}
	for _, r := range dir {
		tt.dialogKey(r, keyNone)
	}
	tt.dialogKey(0, keyEnter)
	if tt.dlg != nil || !tt.inOutput || tt.outTitle != "config set export.folder "+dir {
		t.Fatalf("config set should have run: dlg=%v outTitle=%q", tt.dlg, tt.outTitle)
	}

	// a set folder adds the reset row; picking it runs config unset
	tt.inOutput = false
	tt.askConfig()
	d = tt.dlg
	if d == nil || len(d.items) != 3 || d.items[2].value != "unset" {
		t.Fatalf("expected show/set/reset rows, got %+v", d)
	}
	tt.dialogKey(0, keyDown)
	tt.dialogKey(0, keyDown) // → reset
	tt.dialogKey(0, keyEnter)
	if tt.dlg != nil || !tt.inOutput || tt.outTitle != "config unset export.folder" {
		t.Fatalf("config unset should have run: dlg=%v outTitle=%q", tt.dlg, tt.outTitle)
	}
	if got := exportFolderSetting(); got != "" {
		t.Fatalf("export.folder should be cleared after the flow, got %q", got)
	}
}

func TestGuidedConfig(t *testing.T) {
	seedStore(t, 1)

	// no setting yet: decline → bare config (lists all settings)
	if argv := guidedConfig(bufio.NewReader(strings.NewReader("\n"))); len(argv) != 1 || argv[0] != "config" {
		t.Fatalf("decline should list settings, got %v", argv)
	}

	// accept + type a folder → config set
	dir := t.TempDir()
	argv := guidedConfig(bufio.NewReader(strings.NewReader("y\n" + dir + "\n")))
	want := []string{"config", "set", "export.folder", dir}
	if len(argv) != len(want) {
		t.Fatalf("argv = %v, want %v", argv, want)
	}
	for i := range want {
		if argv[i] != want[i] {
			t.Fatalf("argv = %v, want %v", argv, want)
		}
	}

	// accept + blank folder → cancelled
	if argv := guidedConfig(bufio.NewReader(strings.NewReader("y\n\n"))); argv != nil {
		t.Fatalf("blank folder must cancel, got %v", argv)
	}

	// setting present: keep it, then reset it
	s, err := store.Open(storePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ConfigSet("export.folder", dir); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if argv := guidedConfig(bufio.NewReader(strings.NewReader("n\nn\n"))); len(argv) != 1 || argv[0] != "config" {
		t.Fatalf("decline both should list settings, got %v", argv)
	}
	argv = guidedConfig(bufio.NewReader(strings.NewReader("n\ny\n")))
	if len(argv) != 3 || argv[0] != "config" || argv[1] != "unset" || argv[2] != "export.folder" {
		t.Fatalf("reset should unset, got %v", argv)
	}
}
