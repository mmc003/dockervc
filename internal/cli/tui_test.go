package cli

import (
	"strings"
	"testing"

	"dockervc/internal/store"
)

// TestInputKeyEmptyNoPanic guards the crash where backspacing (or Enter)
// emptied the ':' command line and updateCompletions indexed toks[0] of an
// empty slice — any executed :command or backspace-to-empty killed the TUI.
func TestInputKeyEmptyNoPanic(t *testing.T) {
	tt := &tui{inInput: true, input: "diff"}

	// Backspace every character down to (and past) the empty string.
	for range len("diff") + 2 {
		tt.inputKey(0, keyBackspace)
	}
	if tt.input != "" || tt.comp != nil {
		t.Fatalf("input=%q comp=%v, want empty", tt.input, tt.comp)
	}

	// The completion refresh itself must tolerate an empty line.
	tt.updateCompletions()
	if tt.comp != nil {
		t.Fatalf("comp=%v, want nil on empty input", tt.comp)
	}
}

// A line whose only token is a flag-looking fragment must not panic either.
func TestUpdateCompletionsSingleToken(t *testing.T) {
	tt := &tui{inInput: true, input: "-"}
	tt.updateCompletions() // used to reach toks[0] safely, but stay covered
	if tt.comp != nil {
		t.Fatalf("comp=%v, want nil for non-matching command", tt.comp)
	}
}

// ':' inside the output pane opens the command line with the output still
// visible (e.g. run `log`, then type `show <id>` while reading the ids).
func TestColonFromOutputView(t *testing.T) {
	tt := &tui{inOutput: true, outTitle: "log", outLines: []string{"snap-1", "snap-2"}, h: 24, w: 80}

	tt.outputKey(':', keyNone)
	if !tt.inInput || !tt.inOutput {
		t.Fatalf("inInput=%v inOutput=%v, want command mode over output", tt.inInput, tt.inOutput)
	}

	body := tt.bodyView()
	joined := ""
	for _, r := range body {
		joined += r[0].(string) + "\n"
	}
	if !strings.Contains(joined, "output of: log") || !strings.Contains(joined, "snap-1") {
		t.Fatalf("output pane lost in command mode:\n%s", joined)
	}

	// Esc returns to the output pane, not the menu.
	tt.inputKey(0, keyEsc)
	if tt.inInput || !tt.inOutput {
		t.Fatalf("inInput=%v inOutput=%v, want back at output", tt.inInput, tt.inOutput)
	}
}

// Plain ':' from the menu keeps working: candidates, no output pane.
func TestColonFromMenu(t *testing.T) {
	tt := &tui{h: 24, w: 80}
	tt.menuKey(':', keyNone)
	if !tt.inInput || tt.inOutput {
		t.Fatalf("inInput=%v inOutput=%v, want command mode from menu", tt.inInput, tt.inOutput)
	}
	if body := tt.bodyView(); len(body) != 0 {
		t.Fatalf("menu command mode body = %v, want empty (no candidates yet)", body)
	}
}

// `:dockervc doctor --repair` must route through the destructive-confirm
// dialog exactly like `:doctor --repair` — the prefix used to bypass the
// name check, sending doctor's invisible console prompt into raw-mode stdin
// and freezing the TUI (Ctrl-C swallowed, nothing recoverable but kill).
func TestConfirmDestructiveStripsPrefix(t *testing.T) {
	tt := &tui{}
	var ran []string
	tt.confirmDestructive([]string{"dockervc", "doctor", "--repair"},
		func(argv []string) { ran = argv })

	if len(ran) != 0 {
		t.Fatalf("prefixed repair ran directly: %v", ran)
	}
	if tt.dlg == nil || tt.dlg.kind != dlgBool {
		t.Fatalf("want y/n dialog for prefixed --repair, got %+v", tt.dlg)
	}
	// Answer y: runs with --yes and the prefix stripped.
	tt.dialogKey('y', keyNone)
	if !equalStrings(ran, []string{"doctor", "--yes", "--repair"}) {
		t.Fatalf("ran = %v, want doctor --yes --repair", ran)
	}
}

// A prefixed non-destructive command must still execute (prefix stripped),
// not silently no-op.
func TestConfirmDestructivePrefixNonDestructive(t *testing.T) {
	tt := &tui{}
	var ran []string
	tt.confirmDestructive([]string{"./dockervc", "log"},
		func(argv []string) { ran = argv })
	if !equalStrings(ran, []string{"log"}) {
		t.Fatalf("ran = %v, want [log]", ran)
	}
}

// `:import --apply x.dvca` routes through the y/n dialog (its console
// confirm() would hang raw mode); plain `import` is additive and runs
// directly, like snapshot.
func TestConfirmDestructiveImportApply(t *testing.T) {
	tt := &tui{}
	var ran []string
	tt.confirmDestructive([]string{"import", "x.dvca"},
		func(argv []string) { ran = argv })
	if !equalStrings(ran, []string{"import", "x.dvca"}) {
		t.Fatalf("plain import must run directly: %v", ran)
	}

	ran = nil
	tt.confirmDestructive([]string{"import", "--apply", "x.dvca"},
		func(argv []string) { ran = argv })
	if len(ran) != 0 {
		t.Fatalf("--apply ran without asking: %v", ran)
	}
	if tt.dlg == nil || tt.dlg.kind != dlgBool {
		t.Fatalf("want y/n dialog for import --apply, got %+v", tt.dlg)
	}
	tt.dialogKey('y', keyNone)
	if !equalStrings(ran, []string{"import", "--yes", "--apply", "x.dvca"}) {
		t.Fatalf("ran = %v, want import --yes --apply x.dvca", ran)
	}
}

// confirm() must never read stdin while the TUI is active — its prompt is
// invisible there (output captured) and raw-mode keys can't reliably end it.
func TestConfirmSuppressedInTUI(t *testing.T) {
	tuiActive = true
	defer func() { tuiActive = false }()
	if confirm("Delete everything?") {
		t.Fatal("confirm must auto-decline inside the TUI")
	}
}

// Completion also accepts the binary-name prefix (`:dockervc diff s…`).
func TestUpdateCompletionsSkipsPrefix(t *testing.T) {
	tt := &tui{inInput: true, input: "dockervc diff s",
		snaps: []store.SnapshotRow{{ID: "snap-1", Message: "m"}}}
	tt.updateCompletions()
	if len(tt.comp) != 1 || tt.comp[0].ID != "snap-1" {
		t.Fatalf("comp = %+v, want snap-1", tt.comp)
	}
}

// Tab completion in a rollback command's snapshot slot. The fixtures mirror a
// real rollback session, where a pre-rollback checkpoint's MESSAGE embeds the
// id of the snapshot it precedes — matchSnaps counts those as matches too, so
// Tab's result depends on message content, not just ids.
func TestTabCompletesRollbackSnapshotArg(t *testing.T) {
	rows := []store.SnapshotRow{
		{ID: "snap-20260912-121128-476e", Message: "pre-rollback checkpoint before snap-20260912-120452-3469"},
		{ID: "snap-20260912-120452-3469", Message: "pre-rollback checkpoint before snap-20260912-115819-d17d"},
		{ID: "snap-20260912-120246-fc26", Message: "pre-rollback checkpoint before snap-20260912-115819-d17d"},
		{ID: "snap-20260912-115819-d17d", Message: "rollback baseline"},
	}

	// Unique prefix — 3469 matches by id and nothing mentions it in a
	// message — Tab replaces the token with the full id. Replay real
	// keystrokes through inputKey so completion state updates exactly as it
	// does while typing.
	tt := &tui{inInput: true, snaps: []store.SnapshotRow{
		{ID: "snap-20260912-120452-3469", Message: "pre-rollback checkpoint before snap-20260912-115819-d17d"},
		{ID: "snap-20260912-115819-d17d", Message: "rollback baseline"},
	}}
	for _, r := range "rollback snap-20260912-1204" {
		tt.inputKey(r, keyNone)
	}
	tt.inputKey(0, keyTab)
	if want := "rollback snap-20260912-120452-3469"; tt.input != want {
		t.Fatalf("after Tab: input = %q, want %q", tt.input, want)
	}

	// Two matches — 3469 by id prefix AND 476e, whose checkpoint message
	// embeds 3469's id — so Tab extends only to the longest common prefix.
	// This is exactly the case observed live (1204 → Tab → …-12): correct
	// lcp behavior over two real matches, not a completion defect.
	tt = &tui{inInput: true, snaps: rows}
	for _, r := range "rollback snap-20260912-1204" {
		tt.inputKey(r, keyNone)
	}
	if len(tt.comp) != 2 {
		t.Fatalf("comp = %d rows, want 2 (id prefix + message match)", len(tt.comp))
	}
	tt.inputKey(0, keyTab)
	if want := "rollback snap-20260912-12"; tt.input != want {
		t.Fatalf("two-match Tab: input = %q, want %q", tt.input, want)
	}

	// An ambiguous prefix (the baseline's id appears in every checkpoint
	// message) also extends to the longest common prefix.
	tt = &tui{inInput: true, snaps: rows}
	for _, r := range "rollback snap-20260912-11" {
		tt.inputKey(r, keyNone)
	}
	tt.inputKey(0, keyTab)
	if want := "rollback snap-20260912-1"; tt.input != want {
		t.Fatalf("ambiguous Tab: input = %q, want %q", tt.input, want)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// The menu repair offer triggers on the check's "problem(s) found" error line
// but not on a finished repair's "problem(s) remain" line (no nag loop).
func TestOutputHadProblems(t *testing.T) {
	found := []string{
		"snapshots: 1 checked, 1 broken:",
		"Error: 9 problem(s) found — run `dockervc doctor --repair` to fix them",
	}
	if !outputHadProblems(found) {
		t.Fatal("problems line must match")
	}
	healthy := []string{
		"snapshots: 2 checked, all reference their objects",
		"tip: `dockervc doctor --deep` additionally re-hashes every object's content.",
	}
	if outputHadProblems(healthy) {
		t.Fatal("healthy report must not match")
	}
	remain := []string{
		"Error: repair finished, 2 problem(s) remain (declined or failed fixes)",
	}
	if outputHadProblems(remain) {
		t.Fatal("completed-repair line must not re-trigger the offer")
	}
}

// The repair offer quotes the count parsed from the captured error line.
func TestProblemCountIn(t *testing.T) {
	n, ok := problemCountIn([]string{
		"content: 1 of 5 object(s) CORRUPTED:",
		"Error: 3 problem(s) found — run `dockervc doctor --repair` to fix them",
	})
	if !ok || n != 3 {
		t.Fatalf("problemCountIn = %d, %v; want 3, true", n, ok)
	}
	if _, ok := problemCountIn([]string{"snapshots: 1 checked, all reference their objects"}); ok {
		t.Fatal("healthy report must not parse as problems")
	}
}

// y/n dialogs render over the output pane — the dry-run plan that a
// rollback's "apply now?" question refers to must stay on screen instead of
// flashing away when the dialog opens.
func TestBoolDialogKeepsOutputPane(t *testing.T) {
	tt := &tui{h: 24, w: 80, inOutput: true,
		outTitle: "rollback --dry-run snap-1",
		outLines: []string{"1. stop+remove container demo-web", "2. restore volume demo-data"},
		dlg:      &dialog{kind: dlgBool, title: "apply the rollback now?"}}
	joined := ""
	for _, r := range tt.bodyView() {
		joined += r[0].(string) + "\n"
	}
	if !strings.Contains(joined, "apply the rollback now?") {
		t.Fatalf("dialog question missing:\n%s", joined)
	}
	if !strings.Contains(joined, "1. stop+remove container demo-web") {
		t.Fatalf("plan not visible under the dialog:\n%s", joined)
	}
}

// askScope mirrors guidedRollback: everything → --all; per-kind y/n + a
// comma-separated name list → the corresponding flags; nothing selected →
// nil, so no rollback runs.
func TestAskScope(t *testing.T) {
	tt := &tui{h: 24, w: 80}
	var got []string
	tt.askScope(func(scope []string) { got = scope })

	tt.dialogKey(0, keyEnter) // everything (default yes)
	if !equalStrings(got, []string{"--all"}) {
		t.Fatalf("scope = %v, want [--all]", got)
	}

	got = nil
	tt.askScope(func(scope []string) { got = scope })
	tt.dialogKey('n', keyNone) // not everything
	tt.dialogKey('y', keyNone) // containers?
	for _, r := range "demo-web, demo-worker" {
		tt.dialogKey(r, keyNone)
	}
	tt.dialogKey(0, keyEnter)  // submit names
	tt.dialogKey('n', keyNone) // volumes?
	tt.dialogKey('n', keyNone) // images?
	tt.dialogKey('n', keyNone) // networks?
	if !equalStrings(got, []string{"--containers", "demo-web,demo-worker"}) {
		t.Fatalf("scope = %v, want --containers demo-web,demo-worker", got)
	}

	got = []string{"sentinel"}
	tt.askScope(func(scope []string) { got = scope })
	for range 5 {
		tt.dialogKey('n', keyNone) // everything + all four kinds declined
	}
	if got != nil {
		t.Fatalf("scope = %v, want nil (nothing selected)", got)
	}
}
