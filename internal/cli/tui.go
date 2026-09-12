// Full-screen TUI for `dockervc cli` (unix terminals). Renders into the
// alternate screen buffer with an arrow-key menu, a scrollable output pane
// for command results, dialog prompts for guided flows, and Tab-completion
// for snapshot ids. Every action still executes through the real cobra
// command tree via runArgs — the TUI is presentation plus prompts, not logic.
package cli

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"runtime"
	"strconv"
	"strings"
	"unicode/utf8"

	"golang.org/x/term"

	"dockervc/internal/store"
)

// openMenu launches the best interactive front-end for the environment: the
// full-screen TUI on a unix terminal, the line-based loop otherwise (piped
// stdin, Windows).
func openMenu() error {
	if runtime.GOOS != "windows" &&
		term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd())) {
		return runTUI()
	}
	return RunInteractive()
}

// ── key events ────────────────────────────────────────────────────────────────

type key int

const (
	keyNone key = iota
	keyUp
	keyDown
	keyLeft
	keyRight
	keyEnter
	keyTab
	keyEsc
	keyBackspace
	keyCtrlC
	keyEOF
)

func readKey(br *bufio.Reader) (rune, key) {
	b, err := br.ReadByte()
	if err != nil {
		return 0, keyEOF
	}
	switch b {
	case 0x03:
		return 0, keyCtrlC
	case 0x0d, 0x0a:
		return 0, keyEnter
	case 0x09:
		return 0, keyTab
	case 0x7f, 0x08:
		return 0, keyBackspace
	case 0x1b:
		if br.Buffered() == 0 {
			return 0, keyEsc
		}
		b2, _ := br.ReadByte()
		if b2 != '[' && b2 != 'O' {
			return 0, keyEsc
		}
		b3, err := br.ReadByte()
		if err != nil {
			return 0, keyEsc
		}
		switch b3 {
		case 'A':
			return 0, keyUp
		case 'B':
			return 0, keyDown
		case 'C':
			return 0, keyRight
		case 'D':
			return 0, keyLeft
		}
		return 0, keyEsc
	}
	if b < utf8.RuneSelf {
		return rune(b), keyNone
	}
	// multi-byte utf-8
	n := 0
	switch {
	case b&0xE0 == 0xC0:
		n = 1
	case b&0xF0 == 0xE0:
		n = 2
	case b&0xF8 == 0xF0:
		n = 3
	}
	buf := []byte{b}
	for i := 0; i < n; i++ {
		nb, err := br.ReadByte()
		if err != nil {
			break
		}
		buf = append(buf, nb)
	}
	r, _ := utf8.DecodeRune(buf)
	return r, keyNone
}

// ── dialogs ───────────────────────────────────────────────────────────────────

type dialogKind int

const (
	dlgText dialogKind = iota // free-form input
	dlgBool                   // y/n
	dlgPick                   // snapshot picker: type to filter, arrows+Enter
)

type dialog struct {
	kind     dialogKind
	title    string
	def      string
	value    string
	boolVal  bool
	pickSel  int
	textDone func(t *tui, val string, ok bool)
	boolDone func(t *tui, val bool, ok bool)
}

// ── the TUI ───────────────────────────────────────────────────────────────────

type tui struct {
	br   *bufio.Reader
	out  *bufio.Writer
	w, h int
	done bool

	sel int // menu selection

	inOutput bool // showing captured command output
	outTitle string
	outLines []string
	scroll   int

	inInput bool // ':' raw command mode
	input   string
	comp    []store.SnapshotRow // live completion candidates

	dlg   *dialog
	flash string

	escArmed bool // one Esc pressed in the menu — a second quits

	snaps     []store.SnapshotRow // newest first
	storeLine string              // cached header text (store path + counts)
}

// tuiActive is true while the full-screen TUI runs. confirm() checks it so a
// console prompt can never block on raw-mode stdin (where its text is also
// invisible — output is captured into the output pane).
var tuiActive bool

func runTUI() error {
	fd := int(os.Stdin.Fd())
	old, err := term.MakeRaw(fd)
	if err != nil {
		return RunInteractive()
	}
	defer term.Restore(fd, old)
	tuiActive = true

	t := &tui{br: bufio.NewReader(os.Stdin), out: bufio.NewWriter(os.Stdout)}
	t.out.WriteString("\x1b[?1049h\x1b[?25l") // alt screen, hide cursor
	defer func() {
		t.out.WriteString("\x1b[?25h\x1b[?1049l") // restore
		t.out.Flush()
	}()
	t.refresh()

	for {
		t.render()
		r, k := readKey(t.br)
		t.flash = ""
		if k == keyCtrlC || k == keyEOF || t.done {
			return nil
		}
		if k != keyEsc {
			t.escArmed = false // any other key dismisses the quit prompt
		}
		switch {
		case t.dlg != nil:
			t.dialogKey(r, k)
		case t.inInput:
			t.inputKey(r, k)
		case t.inOutput:
			t.outputKey(r, k)
		default:
			t.menuKey(r, k)
		}
		if t.done {
			return nil
		}
	}
}

// ── key handlers ──────────────────────────────────────────────────────────────

func (t *tui) menuKey(r rune, k key) {
	switch k {
	case keyUp:
		t.sel = (t.sel + len(menuActions) - 1) % len(menuActions)
	case keyDown:
		t.sel = (t.sel + 1) % len(menuActions)
	case keyEnter:
		t.runMenuAction(t.sel)
	case keyEsc:
		if t.escArmed {
			t.done = true
		} else {
			t.escArmed = true
			t.flash = "hit Esc again to quit (Ctrl-C also quits)"
		}
	case keyNone:
		switch r {
		case 'j':
			t.sel = (t.sel + 1) % len(menuActions)
		case 'k':
			t.sel = (t.sel + len(menuActions) - 1) % len(menuActions)
		case ':':
			t.inInput, t.input, t.comp = true, "", nil
		case '?':
			t.execTUI("man")
		case 'q':
			t.done = true
		default:
			if r >= '1' && r <= '9' {
				if n := int(r-'0') - 1; n < len(menuActions) {
					t.sel = n
				}
			}
		}
	}
}

func (t *tui) outputKey(r rune, k key) {
	vis := t.visLines()
	maxScroll := len(t.outLines) - vis
	if maxScroll < 0 {
		maxScroll = 0
	}
	switch k {
	case keyUp:
		if t.scroll > 0 {
			t.scroll--
		}
	case keyDown:
		if t.scroll < maxScroll {
			t.scroll++
		}
	case keyEsc, keyEnter:
		t.inOutput = false
	case keyNone:
		switch r {
		case 'j':
			if t.scroll < maxScroll {
				t.scroll++
			}
		case 'k':
			if t.scroll > 0 {
				t.scroll--
			}
		case 'g':
			t.scroll = 0
		case 'G':
			t.scroll = maxScroll
		case ':':
			// Enter command mode keeping the output visible — ids shown by
			// `log`/`show` stay on screen while the next command is typed.
			t.inInput, t.input, t.comp = true, "", nil
		case 'q':
			t.inOutput = false
		}
	}
}

func (t *tui) inputKey(r rune, k key) {
	switch k {
	case keyEsc:
		t.inInput, t.input, t.comp = false, "", nil
	case keyEnter:
		argv := splitQuoted(t.input)
		t.inInput, t.input, t.comp = false, "", nil
		if len(argv) == 0 {
			return
		}
		t.execTUI(argv...)
	case keyBackspace:
		if runes := []rune(t.input); len(runes) > 0 {
			t.input = string(runes[:len(runes)-1])
		}
	case keyTab:
		t.tabComplete()
	case keyNone:
		t.input += string(r)
	}
	t.updateCompletions()
}

func (t *tui) dialogKey(r rune, k key) {
	d := t.dlg
	switch d.kind {
	case dlgText:
		switch k {
		case keyEsc:
			t.dlg = nil
			d.textDone(t, "", false)
		case keyEnter:
			v := d.value
			if v == "" {
				v = d.def
			}
			t.dlg = nil
			d.textDone(t, v, true)
		case keyBackspace:
			if runes := []rune(d.value); len(runes) > 0 {
				d.value = string(runes[:len(runes)-1])
			}
		case keyNone:
			d.value += string(r)
		}
	case dlgBool:
		switch k {
		case keyEsc:
			t.dlg = nil
			d.boolDone(t, false, false)
		case keyEnter:
			t.dlg = nil
			d.boolDone(t, d.boolVal, true)
		case keyNone:
			switch r {
			case 'y', 'Y':
				t.dlg = nil
				d.boolDone(t, true, true)
			case 'n', 'N':
				t.dlg = nil
				d.boolDone(t, false, true)
			}
		}
	case dlgPick:
		rows := t.pickRows()
		switch k {
		case keyEsc:
			t.dlg = nil
			d.textDone(t, "", false)
		case keyUp:
			if d.pickSel > 0 {
				d.pickSel--
			}
		case keyDown:
			if d.pickSel < len(rows)-1 {
				d.pickSel++
			}
		case keyTab, keyEnter:
			if len(rows) > 0 {
				if d.pickSel >= len(rows) {
					d.pickSel = len(rows) - 1
				}
				id := rows[d.pickSel].ID
				t.dlg = nil
				d.textDone(t, id, true)
			}
		case keyBackspace:
			if runes := []rune(d.value); len(runes) > 0 {
				d.value = string(runes[:len(runes)-1])
				d.pickSel = 0
			}
		case keyNone:
			d.value += string(r)
			d.pickSel = 0
		}
	}
}

// ── flows ─────────────────────────────────────────────────────────────────────

func (t *tui) runMenuAction(i int) {
	if i < 0 || i >= len(menuActions) {
		return
	}
	switch menuActions[i].name {
	case "snapshot":
		t.askText("snapshot message", "interactive snapshot", func(val string, ok bool) {
			if !ok {
				return
			}
			t.askBool("stop containers first (app-consistent)?", false, func(stop bool, ok bool) {
				if !ok {
					return
				}
				argv := []string{"snapshot", "-m", val}
				if stop {
					argv = append(argv, "--stop")
				}
				t.execTUI(argv...)
			})
		})
	case "show":
		t.askPick("show which snapshot?", func(id string, ok bool) {
			if ok && id != "" {
				t.execTUI("show", id)
			}
		})
	case "diff":
		t.askPick("diff FROM which snapshot?", func(a string, ok bool) {
			if !ok || a == "" {
				return
			}
			t.askPick("diff TO which snapshot? (Esc = live engine)", func(b string, ok bool) {
				if ok && b != "" {
					t.execTUI("diff", a, b)
				} else {
					// Esc on the second picker means "compare against live"
					t.execTUI("diff", a)
				}
			})
		})
	case "delete":
		t.askPick("delete which snapshot?", func(id string, ok bool) {
			if !ok || id == "" {
				return
			}
			t.askBool("delete this snapshot?", false, func(yes bool, ok bool) {
				if ok && yes {
					t.execTUI("delete", "--yes", id)
				}
			})
		})
	case "doctor":
		t.askBool("deep check — re-hash every object? (slower)", false, func(deep bool, ok bool) {
			if !ok {
				return
			}
			check := []string{"doctor"}
			if deep {
				check = append(check, "--deep")
			}
			t.doExec(check...) // read-only check: no destructive gate needed
			n, found := problemCountIn(t.outLines)
			if !found {
				return
			}
			// Menu path for `doctor --repair`: ask in plain words (the raw
			// "run doctor --repair --deep ?" read like the deep-check
			// question again), then run with --yes baked in like the delete
			// flow. The report stays visible under the dialog.
			t.askBool(fmt.Sprintf("%d problem(s) found — repair now?", n), false, func(yes bool, ok bool) {
				if !ok || !yes {
					return
				}
				t.doExec(append([]string{"doctor", "--repair", "--yes"}, check[1:]...)...)
			})
		})
	default:
		t.execTUI(menuActions[i].name)
	}
}

func (t *tui) askText(title, def string, done func(val string, ok bool)) {
	t.dlg = &dialog{kind: dlgText, title: title, def: def,
		textDone: func(t *tui, val string, ok bool) { done(val, ok) }}
}

func (t *tui) askBool(title string, def bool, done func(val bool, ok bool)) {
	t.dlg = &dialog{kind: dlgBool, title: title, boolVal: def,
		boolDone: func(t *tui, val bool, ok bool) { done(val, ok) }}
}

func (t *tui) askPick(title string, done func(val string, ok bool)) {
	t.dlg = &dialog{kind: dlgPick, title: title,
		textDone: func(t *tui, val string, ok bool) { done(val, ok) }}
}

// confirmDestructive intercepts delete/prune typed in raw ':' mode so their
// own [y/N] prompt (invisible inside the TUI) never blocks on hidden input.
// Menu flows pre-confirm and pass --yes themselves.
func (t *tui) confirmDestructive(argv []string, run func([]string)) {
	// `:dockervc doctor --repair` must behave exactly like `:doctor --repair`:
	// without stripping, the name check below silently misses the command and
	// its console prompt would freeze the TUI.
	if len(argv) > 0 && (argv[0] == "dockervc" || argv[0] == "./dockervc") {
		argv = argv[1:]
	}
	if len(argv) == 0 {
		return
	}
	name := argv[0]
	destructive := name == "delete" || name == "prune" ||
		(name == "doctor" && containsArg(argv, "--repair")) // repair deletes
	if !destructive {
		run(argv)
		return
	}
	for _, a := range argv[1:] {
		if a == "-y" || a == "--yes" {
			run(argv)
			return
		}
	}
	t.askBool("run "+strings.Join(argv, " ")+" ?", false, func(yes bool, ok bool) {
		if ok && yes {
			run(append([]string{name, "--yes"}, argv[1:]...))
		}
	})
}

// containsArg reports whether the exact flag appears in argv (position 1+).
func containsArg(argv []string, flag string) bool {
	for _, a := range argv[1:] {
		if a == flag {
			return true
		}
	}
	return false
}

// problemCountIn extracts the count from a captured doctor run's
// "N problem(s) found" error line; ok=false when the run was healthy. The
// "repair finished, N problem(s) remain" line deliberately does not match, so
// a completed (partially declined) repair isn't re-offered.
func problemCountIn(lines []string) (n int, ok bool) {
	for _, l := range lines {
		i := strings.Index(l, "problem(s) found")
		if i < 0 {
			continue
		}
		seg := strings.TrimSpace(strings.TrimPrefix(l[:i], "Error:"))
		if v, err := strconv.Atoi(seg); err == nil {
			return v, true
		}
	}
	return 0, false
}

// outputHadProblems reports whether a captured doctor run found problems.
func outputHadProblems(lines []string) bool {
	_, ok := problemCountIn(lines)
	return ok
}

// ── snapshot completion ───────────────────────────────────────────────────────

// snapArgSlots maps commands to the positional-argument indexes that hold
// snapshot ids (used for hints, completion and expansion).
var snapArgSlots = map[string][]int{
	"show": {0}, "diff": {0, 1}, "delete": {0}, "rollback": {0}, "export": {0},
}

// refresh reloads the snapshot list and cached store header line.
func (t *tui) refresh() {
	t.storeLine = storeStatusLine()
	s, err := store.Open(storePath)
	if err != nil {
		t.snaps = nil
		return
	}
	rows, err := s.ListSnapshots()
	s.Close()
	if err != nil {
		t.snaps = nil
		return
	}
	// newest first, like `log`
	for i, j := 0, len(rows)-1; i < j; i, j = i+1, j-1 {
		rows[i], rows[j] = rows[j], rows[i]
	}
	t.snaps = rows
}

// matchSnaps returns rows whose id prefix-matches q first, then id/message
// substring matches.
func matchSnaps(rows []store.SnapshotRow, q string) []store.SnapshotRow {
	q = strings.ToLower(strings.TrimSpace(q))
	if q == "" {
		return rows
	}
	var pref, sub []store.SnapshotRow
	for _, r := range rows {
		id := strings.ToLower(r.ID)
		if strings.HasPrefix(id, q) {
			pref = append(pref, r)
		} else if strings.Contains(id, q) || strings.Contains(strings.ToLower(r.Message), q) {
			sub = append(sub, r)
		}
	}
	return append(pref, sub...)
}

// updateCompletions refreshes the live candidate list for the token being
// typed in ':' mode, when that token sits in a snapshot-argument slot.
func (t *tui) updateCompletions() {
	t.comp = nil
	toks := strings.Fields(t.input)
	// `:dockervc diff …` completes like `:diff …` — skip a binary-name prefix.
	if len(toks) > 0 && (toks[0] == "dockervc" || toks[0] == "./dockervc") {
		toks = toks[1:]
	}
	if len(toks) == 0 { // empty after Enter or backspace-to-empty
		return
	}
	slots, takesSnap := snapArgSlots[toks[0]]
	if !takesSnap || len(toks) < 2 {
		return
	}
	last := len(toks) - 1
	isSlot := false
	for _, s := range slots {
		if last == s+1 {
			isSlot = true
		}
	}
	if !isSlot || strings.HasPrefix(toks[last], "-") {
		return
	}
	t.comp = matchSnaps(t.snaps, toks[last])
}

// tabComplete replaces the trailing token with the unique match, or extends
// it to the longest common prefix of the matches.
func (t *tui) tabComplete() {
	toks := strings.Fields(t.input)
	if len(toks) < 2 || len(t.comp) == 0 {
		return
	}
	old := toks[len(toks)-1]
	var repl string
	if len(t.comp) == 1 {
		repl = t.comp[0].ID
	} else {
		repl = lcpIDs(t.comp)
		if repl == old {
			return // nothing to extend; the match list is already shown
		}
	}
	t.input = t.input[:len(t.input)-len(old)] + repl
}

func lcpIDs(rows []store.SnapshotRow) string {
	if len(rows) == 0 {
		return ""
	}
	p := rows[0].ID
	for _, r := range rows[1:] {
		for !strings.HasPrefix(r.ID, p) && len(p) > 0 {
			p = p[:len(p)-1]
		}
	}
	return p
}

// expandSnapArgs resolves partial snapshot refs in argv to full ids. Returns
// an error message when a ref is ambiguous or unmatched.
func (t *tui) expandSnapArgs(argv []string) ([]string, string) {
	slots, ok := snapArgSlots[argv[0]]
	if !ok {
		return argv, ""
	}
	out := append([]string(nil), argv...)
	for _, s := range slots {
		idx := s + 1 // argv[0] is the command name
		if idx >= len(out) {
			break // that argument was not provided
		}
		tok := out[idx]
		if strings.HasPrefix(tok, "-") {
			continue
		}
		exact := false
		for _, r := range t.snaps {
			if r.ID == tok {
				exact = true
				break
			}
		}
		if exact {
			continue
		}
		m := matchSnaps(t.snaps, tok)
		switch {
		case len(m) == 1:
			out[idx] = m[0].ID
		case len(m) > 1:
			return nil, fmt.Sprintf("%q matches %d snapshots — be more specific (Tab lists them)", tok, len(m))
		default:
			return nil, fmt.Sprintf("no snapshot matches %q", tok)
		}
	}
	return out, ""
}

// ── command execution ─────────────────────────────────────────────────────────

// execTUI runs argv through the real command tree, capturing output for the
// output pane. Destructive commands (delete, prune) are confirmed with a TUI
// dialog first and run with --yes — their own console [y/N] prompt would be
// invisible here and would deadlock on raw-mode input.
func (t *tui) execTUI(argv ...string) {
	t.confirmDestructive(argv, func(argv []string) { t.doExec(argv...) })
}

func (t *tui) doExec(argv ...string) {
	argv, errMsg := t.expandSnapArgs(argv)
	if errMsg != "" {
		t.flash = errMsg
		return
	}
	// Long captures (snapshot: docker commit/save of whole images) otherwise
	// look like a freeze — draw one "running" frame first.
	t.flash = "running: " + strings.Join(argv, " ") + " …"
	t.render()
	out := captureOutput(func() { runArgs(argv...) })
	t.flash = ""
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) == 1 && lines[0] == "" {
		lines = nil
	}
	t.outTitle = strings.Join(argv, " ")
	t.outLines = lines
	t.scroll = 0
	t.inOutput = true
	t.refresh()
}

// captureOutput runs f with os.Stdout/os.Stderr redirected into a pipe and
// returns everything written.
func captureOutput(f func()) string {
	r, w, err := os.Pipe()
	if err != nil {
		f()
		return ""
	}
	oldOut, oldErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = w, w
	done := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	func() {
		defer func() {
			os.Stdout, os.Stderr = oldOut, oldErr
			w.Close()
		}()
		f()
	}()
	return <-done
}

// ── rendering ─────────────────────────────────────────────────────────────────

// mainH is the line budget for the main area; the remaining 6 lines are
// header(2), separator, separator, hint bar, input line.
func (t *tui) mainH() int {
	if n := t.h - 6; n >= 3 {
		return n
	}
	return 3
}

// visLines is the number of output-body lines visible in the output pane
// (minus its title and counter lines).
func (t *tui) visLines() int { return t.mainH() - 2 }

func (t *tui) render() {
	t.w, t.h = 80, 24 // sane fallback when size detection fails
	if w, h, err := term.GetSize(int(os.Stdout.Fd())); err == nil && w > 0 && h > 0 {
		t.w, t.h = w, h
	}
	if t.w < 40 {
		t.w = 40
	}

	var sb strings.Builder
	sb.WriteString("\x1b[H") // home
	row := func(s string, inverse bool) {
		if inverse {
			sb.WriteString("\x1b[7m")
		}
		sb.WriteString(truncateRunes(s, t.w))
		sb.WriteString("\x1b[0m\x1b[K\r\n")
	}

	row(fmt.Sprintf(" dockervc %s │ %s", Version, t.storeLine), false)
	row(strings.Repeat("─", t.w), false)

	var body [][2]any
	body = t.bodyView()
	for i := 0; i < t.mainH(); i++ {
		if i < len(body) {
			row(body[i][0].(string), body[i][1].(bool))
		} else {
			row("", false)
		}
	}

	row(strings.Repeat("─", t.w), false)
	row(t.hintBar(), false)
	sb.WriteString("\x1b[0m\x1b[K") // input line: no trailing newline
	sb.WriteString(truncateRunes(t.inputLine(), t.w-1) + "█")

	t.out.WriteString(sb.String())
	t.out.Flush()
}

// bodyView picks the main pane: dialogs win, then ':' command mode (over the
// output pane when one is showing, so snapshot ids stay readable), then
// output, then the menu.
func (t *tui) bodyView() [][2]any {
	switch {
	case t.dlg != nil:
		return t.dialogView()
	case t.inInput && t.inOutput:
		// A few completion candidates on top, the existing output below.
		cand := t.candidateLines(4)
		return append(cand, t.outputViewReserve(len(cand))...)
	case t.inInput:
		return t.candidateLines(t.mainH())
	case t.inOutput:
		return t.outputViewReserve(0)
	default:
		return t.menuView()
	}
}

// menuView lists commands with their argument shapes. Rows are (text, inverse).
func (t *tui) menuView() [][2]any {
	var out [][2]any
	for i, a := range menuActions {
		if len(out) >= t.mainH() {
			break
		}
		args := a.args
		if args == "" {
			args = "(no args)"
		}
		s := fmt.Sprintf(" %s %2d  %-10s %-16s %s", selMarker(i == t.sel),
			i+1, a.name, args, a.desc)
		out = append(out, [2]any{s, i == t.sel})
	}
	return out
}

// outputViewReserve renders the output pane, leaving `reserved` body lines
// free for content stacked above it (the ':' command mode candidates).
func (t *tui) outputViewReserve(reserved int) [][2]any {
	out := [][2]any{{fmt.Sprintf(" output of: %s", t.outTitle), false}}
	vis := t.visLines() - reserved
	if vis < 1 {
		vis = 1
	}
	start := clamp(t.scroll, 0, len(t.outLines)-vis)
	end := start + vis
	if end > len(t.outLines) {
		end = len(t.outLines)
	}
	for _, l := range t.outLines[start:end] {
		out = append(out, [2]any{" " + l, false})
	}
	for len(out) < t.mainH()-1-reserved {
		out = append(out, [2]any{"", false})
	}
	counter := fmt.Sprintf(" %d line(s)", len(t.outLines))
	if len(t.outLines) > vis {
		counter = fmt.Sprintf(" lines %d–%d of %d", start+1, end, len(t.outLines))
	}
	return append(out, [2]any{counter, false})
}

// candidateLines renders up to `budget` rows of live completion candidates
// (a header line plus ids) for the ':' command line.
func (t *tui) candidateLines(budget int) [][2]any {
	var out [][2]any
	if len(t.comp) > 0 {
		out = append(out, [2]any{fmt.Sprintf(" %d match(es) for %q — Tab completes:",
			len(t.comp), currentToken(t.input)), false})
		for i, r := range t.comp {
			if len(out) >= budget-1 {
				out = append(out, [2]any{fmt.Sprintf("   … %d more", len(t.comp)-i), false})
				break
			}
			out = append(out, [2]any{fmt.Sprintf("   %s  %s", r.ID, r.Message), false})
		}
	}
	return out
}

func (t *tui) dialogView() [][2]any {
	d := t.dlg
	var out [][2]any
	switch d.kind {
	case dlgPick:
		rows := t.pickRows()
		out = append(out, [2]any{" " + d.title, false})
		if len(rows) == 0 {
			out = append(out, [2]any{"   (no snapshots match — type less, or Esc to cancel)", false})
		}
		for i, r := range rows {
			if len(out) >= t.mainH()-1 {
				out = append(out, [2]any{"   …", false})
				break
			}
			s := fmt.Sprintf("   %s  %s", r.ID, r.CreatedAt.Local().Format("2006-01-02 15:04"))
			if msg := truncateRunes(r.Message, t.w-58); msg != "" {
				s += "  " + msg
			}
			out = append(out, [2]any{s, i == d.pickSel})
		}
	default:
		out = append(out, [2]any{" " + d.title, false})
	}
	return out
}

func (t *tui) pickRows() []store.SnapshotRow {
	if t.dlg == nil {
		return nil
	}
	return matchSnaps(t.snaps, t.dlg.value)
}

func (t *tui) hintBar() string {
	if t.flash != "" {
		return " ✗ " + t.flash
	}
	switch {
	case t.dlg != nil && t.dlg.kind == dlgPick:
		return " ↑↓ select · type to filter · Tab/Enter confirm · Esc cancel"
	case t.dlg != nil && t.dlg.kind == dlgBool:
		return " y/n · Enter = default · Esc cancel"
	case t.dlg != nil:
		return " type value · Enter confirm · Esc cancel"
	case t.inInput && t.inOutput:
		return " Tab completes snapshot ids · Enter run · Esc back to output"
	case t.inInput:
		return " Tab completes snapshot ids · Enter run · Esc cancel"
	case t.inOutput:
		return " ↑↓/j/k scroll · g/G top/bottom · : command · q/Esc menu"
	default:
		return " ↑↓/j/k select · Enter run · 1-9 jump · : command · ? man · q quit"
	}
}

func (t *tui) inputLine() string {
	switch {
	case t.dlg != nil:
		d := t.dlg
		switch d.kind {
		case dlgBool:
			return fmt.Sprintf(" %s [y/N]", d.title)
		case dlgPick:
			return fmt.Sprintf(" filter: %s", d.value)
		default:
			def := ""
			if d.def != "" {
				def = fmt.Sprintf(" [%s]", d.def)
			}
			return fmt.Sprintf(" %s%s: %s", d.title, def, d.value)
		}
	case t.inInput:
		return fmt.Sprintf(" : %s", t.input)
	default:
		return ""
	}
}

func selMarker(on bool) string {
	if on {
		return "▸"
	}
	return " "
}

func truncateRunes(s string, w int) string {
	if w <= 1 {
		return ""
	}
	r := []rune(s)
	if len(r) <= w {
		return s
	}
	return string(r[:w-1]) + "…"
}

func currentToken(input string) string {
	f := strings.Fields(input)
	if len(f) == 0 {
		return ""
	}
	return f[len(f)-1]
}

func clamp(v, lo, hi int) int {
	if hi < lo {
		hi = lo // never return below lo (short output → negative max scroll)
	}
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
