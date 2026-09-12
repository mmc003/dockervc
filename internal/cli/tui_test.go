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
