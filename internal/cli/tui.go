// Full-screen TUI for `dockervc cli` (VT-capable terminals). Renders into the
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
	"strconv"
	"strings"
	"unicode/utf8"

	"golang.org/x/term"

	"dockervc/internal/store"
)

// openMenu launches the best interactive front-end for the environment: the
// full-screen TUI on a terminal, the line-based loop otherwise (piped stdin).
var (
	enableVT          = enableVTImpl
	isTerminal        = term.IsTerminal
	runFullScreenMenu = runTUI
	runLineMenu       = RunInteractive
)

func openMenu() error {
	if isTerminal(int(os.Stdin.Fd())) && isTerminal(int(os.Stdout.Fd())) && enableVT() {
		return runFullScreenMenu()
	}
	return runLineMenu()
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
	dlgList                   // generic list: single-choice or checklist
)

// listItem is one row of a list dialog: a label, an optional status note
// ("deleted", "exists — contents replaced"…) and the token it contributes
// to the resulting command when picked. In checklists, rows flagged `on`
// start checked — the caller marks the entities already gone from the
// engine, so one Enter restores exactly what was lost.
type listItem struct {
	text  string
	note  string
	value string
	on    bool
}

type dialog struct {
	kind     dialogKind
	title    string
	def      string
	value    string
	boolVal  bool
	pickSel  int
	textDone func(t *tui, val string, ok bool)
	boolDone func(t *tui, val bool, ok bool)

	items    []listItem // dlgList rows
	multi    bool       // dlgList: checkboxes (space toggles) vs single choice
	filter   bool       // dlgList single-choice: typed text filters visible rows
	checked  []bool     // dlgList checkbox state, parallel to items
	listDone func(t *tui, vals []string, ok bool)

	path bool // dlgText: the value is a filesystem path (Tab completes it)
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

	inInput  bool // ':' raw command mode
	input    string
	comp     []store.SnapshotRow // live snapshot-id completion candidates
	pathComp []string            // live filesystem-path completion candidates

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
		case keyTab:
			if d.path {
				d.completePath()
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
	case dlgList:
		items := d.visibleListItems()
		n := len(items)
		switch k {
		case keyEsc:
			t.dlg = nil
			d.listDone(t, nil, false)
		case keyUp:
			if d.pickSel > 0 {
				d.pickSel--
			}
		case keyDown:
			if d.pickSel < n-1 {
				d.pickSel++
			}
		case keyEnter:
			if n == 0 {
				return
			}
			t.dlg = nil
			if d.multi {
				var vals []string
				for i, on := range d.checked {
					if on {
						vals = append(vals, d.items[i].value)
					}
				}
				d.listDone(t, vals, true)
			} else {
				if d.pickSel >= n {
					d.pickSel = n - 1
				}
				d.listDone(t, []string{items[d.pickSel].value}, true)
			}
		case keyBackspace:
			if d.filter {
				if runes := []rune(d.value); len(runes) > 0 {
					d.value = string(runes[:len(runes)-1])
					d.pickSel = 0
				}
			}
		case keyNone:
			if d.filter {
				d.value += string(r)
				d.pickSel = 0
				break
			}
			switch r {
			case 'j':
				if d.pickSel < n-1 {
					d.pickSel++
				}
			case 'k':
				if d.pickSel > 0 {
					d.pickSel--
				}
			case ' ', 'x', 'X':
				if d.multi && d.pickSel < len(d.checked) {
					d.checked[d.pickSel] = !d.checked[d.pickSel]
				}
			case 'a':
				if d.multi && len(d.checked) > 0 {
					anyOn := false
					for _, on := range d.checked {
						if on {
							anyOn = true
							break
						}
					}
					for i := range d.checked {
						d.checked[i] = !anyOn
					}
				}
			}
		}
	}
}

// ── flows ─────────────────────────────────────────────────────────────────────

func (t *tui) runMenuAction(i int) {
	if i < 0 || i >= len(menuActions) {
		return
	}
	switch menuActions[i].name {
	case "status":
		t.askStatus()
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
		t.askDeleteSnapshots()
	case "rollback":
		t.askPick("roll back to which snapshot?", func(id string, ok bool) {
			if !ok || id == "" {
				return
			}
			t.askRollbackScope(id)
		})
	case "export":
		t.askPick("export which snapshot?", func(id string, ok bool) {
			if !ok || id == "" {
				return
			}
			t.askExportScope(id)
		})
	case "files":
		t.askPick("browse which snapshot?", func(id string, ok bool) {
			if ok && id != "" {
				t.askLocalVolumeFiles(id)
			}
		})
	case "archive":
		t.askArchiveAction()
	case "import":
		t.askImportArchive()
	case "config":
		t.askConfig()
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

// askPath shows a text dialog for a filesystem path — Tab completes the
// value against the directory being typed in, and the matches are listed
// above the input line while typing.
func (t *tui) askPath(title, def string, done func(val string, ok bool)) {
	t.dlg = &dialog{kind: dlgText, title: title, def: def, path: true,
		textDone: func(t *tui, val string, ok bool) { done(val, ok) }}
}

// completePath extends a path dialog's value with the unique directory
// match, or the longest common prefix of the matches.
func (d *dialog) completePath() {
	if d.value == "" {
		return
	}
	ms := pathMatches(d.value)
	if len(ms) == 0 {
		return
	}
	repl := ms[0]
	if len(ms) > 1 {
		repl = lcp(ms)
	}
	if len(repl) > len(d.value) {
		d.value = repl
	}
}

func (t *tui) askBool(title string, def bool, done func(val bool, ok bool)) {
	t.dlg = &dialog{kind: dlgBool, title: title, boolVal: def,
		boolDone: func(t *tui, val bool, ok bool) { done(val, ok) }}
}

func (t *tui) askPick(title string, done func(val string, ok bool)) {
	t.dlg = &dialog{kind: dlgPick, title: title,
		textDone: func(t *tui, val string, ok bool) { done(val, ok) }}
}

// askListOne shows a single-choice list dialog — askPick for non-snapshot
// lists. Arrows/j/k move, Enter picks, Esc cancels.
func (t *tui) askListOne(title string, items []listItem, done func(val string, ok bool)) {
	t.dlg = &dialog{kind: dlgList, title: title, items: items,
		listDone: func(t *tui, vals []string, ok bool) {
			if ok && len(vals) > 0 {
				done(vals[0], true)
			} else {
				done("", false)
			}
		}}
}

// askFilterListOne is askListOne with snapshot-picker-style type-to-filter
// behavior. Arrow keys move through the filtered rows; Enter selects one.
func (t *tui) askFilterListOne(title string, items []listItem, done func(val string, ok bool)) {
	t.dlg = &dialog{kind: dlgList, title: title, items: items, filter: true,
		listDone: func(t *tui, vals []string, ok bool) {
			if ok && len(vals) > 0 {
				done(vals[0], true)
			} else {
				done("", false)
			}
		}}
}

func (d *dialog) visibleListItems() []listItem {
	if !d.filter || d.value == "" {
		return d.items
	}
	needle := strings.ToLower(d.value)
	items := make([]listItem, 0, len(d.items))
	for _, item := range d.items {
		haystack := strings.ToLower(item.text + "\n" + item.note)
		if strings.Contains(haystack, needle) {
			items = append(items, item)
		}
	}
	return items
}

// askListMany shows a checklist dialog. Boxes start checked only for rows
// the caller flagged `on` — entities gone from the engine — so the default
// Enter restores what was lost and leaves existing entities alone.
// space/x toggle, a flips all.
func (t *tui) askListMany(title string, items []listItem, done func(vals []string, ok bool)) {
	checked := make([]bool, len(items))
	for i := range checked {
		checked[i] = items[i].on
	}
	t.dlg = &dialog{kind: dlgList, title: title, items: items, multi: true, checked: checked,
		listDone: func(t *tui, vals []string, ok bool) { done(vals, ok) }}
}

// askScope collects what a rollback restores: everything (--all, the
// default) or per-kind name lists — the full-screen equivalent of the line
// menu's guidedRollback prompts. done receives the flag tokens, or nil when
// nothing was selected (a rollback must name what it restores).
func (t *tui) askScope(done func(scope []string)) {
	t.askBool("restore everything (--all)?", true, func(all bool, ok bool) {
		if !ok {
			return
		}
		if all {
			done([]string{"--all"})
			return
		}
		var flags []string
		var askKind func(kinds []string)
		askKind = func(kinds []string) {
			if len(kinds) == 0 {
				if len(flags) == 0 {
					t.flash = "nothing selected — a rollback must name what it restores"
					done(nil)
					return
				}
				done(flags)
				return
			}
			kind := kinds[0]
			t.askBool("restore any "+kind+"?", false, func(yes bool, ok bool) {
				if !ok {
					return
				}
				if !yes {
					askKind(kinds[1:])
					return
				}
				t.askText(kind+" names (comma-separated)", "", func(names string, ok bool) {
					if !ok {
						return
					}
					var list []string
					for _, n := range strings.Split(names, ",") {
						if n = strings.TrimSpace(n); n != "" {
							list = append(list, n)
						}
					}
					if len(list) > 0 {
						flags = append(flags, "--"+kind, strings.Join(list, ","))
					}
					askKind(kinds[1:])
				})
			})
		}
		askKind([]string{"containers", "volumes", "images", "networks"})
	})
}

// askDeleteSnapshots shows the multi-select deletion checklist. Nothing is
// pre-checked — deleting is destructive, so the default is "none" and the
// count is confirmed once before the batch runs.
func (t *tui) askDeleteSnapshots() {
	var items []listItem
	for _, r := range t.snaps {
		note := r.Message
		if len(note) > 40 {
			note = note[:40] + "…"
		}
		items = append(items, listItem{text: r.ID, note: note, value: r.ID})
	}
	t.askListMany("delete which snapshots?", items, func(ids []string, ok bool) {
		if !ok {
			return
		}
		if len(ids) == 0 {
			t.flash = "nothing selected — space/x checks snapshots to delete"
			t.askDeleteSnapshots()
			return
		}
		t.askBool(fmt.Sprintf("delete %d snapshot(s)?", len(ids)), false, func(yes bool, ok bool) {
			if !ok || !yes {
				return
			}
			t.execTUI(append([]string{"delete", "--yes"}, ids...)...)
		})
	})
}

// confirmDestructive intercepts delete/prune/rollback typed in raw ':' mode so
// their own [y/N] prompt (invisible inside the TUI) never blocks on hidden
// input. Menu flows pre-confirm and pass --yes themselves.
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
	destructive := name == "delete" || name == "prune" || name == "rollback" ||
		(name == "doctor" && containsArg(argv, "--repair")) || // repair deletes
		(name == "import" && containsArg(argv, "--apply")) // --apply chains the destructive rollback
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
	"show": {0}, "diff": {0, 1}, "delete": {0}, "rollback": {0}, "export": {0}, "files": {0},
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
// typed in ':' mode — snapshot ids when the token sits in a snapshot-argument
// slot, filesystem paths for import's archive and export's -o value.
func (t *tui) updateCompletions() {
	t.comp = nil
	t.pathComp = nil
	toks := strings.Fields(t.input)
	// `:dockervc diff …` completes like `:diff …` — skip a binary-name prefix.
	if len(toks) > 0 && (toks[0] == "dockervc" || toks[0] == "./dockervc") {
		toks = toks[1:]
	}
	if len(toks) < 2 { // empty after Enter or backspace-to-empty
		return
	}
	last := len(toks) - 1
	if strings.HasPrefix(toks[last], "-") {
		return
	}
	if slots, ok := snapArgSlots[toks[0]]; ok {
		isSlot := toks[0] == "delete" // multi-delete: every positional is a snapshot id
		for _, s := range slots {
			if last == s+1 {
				isSlot = true
			}
		}
		if isSlot {
			t.comp = matchSnaps(t.snaps, toks[last])
			return
		}
		// export's remaining positional — the -o value — is a path; fall
		// through to the path check below.
	}
	if pathToken(toks) {
		t.pathComp = pathMatches(toks[last])
	}
}

// tabComplete replaces the trailing token with the unique match, or extends
// it to the longest common prefix of the matches.
func (t *tui) tabComplete() {
	toks := strings.Fields(t.input)
	if len(toks) < 2 {
		return
	}
	old := toks[len(toks)-1]
	var repl string
	switch {
	case len(t.comp) == 1:
		repl = t.comp[0].ID
	case len(t.comp) > 1:
		ids := make([]string, len(t.comp))
		for i, r := range t.comp {
			ids[i] = r.ID
		}
		repl = lcp(ids)
	case len(t.pathComp) == 1:
		repl = t.pathComp[0]
	case len(t.pathComp) > 1:
		repl = lcp(t.pathComp)
	default:
		return
	}
	if repl == old {
		return // nothing to extend; the match list is already shown
	}
	t.input = t.input[:len(t.input)-len(old)] + repl
}

// expandSnapArgs resolves partial snapshot refs in argv to full ids. Returns
// an error message when a ref is ambiguous or unmatched.
func (t *tui) expandSnapArgs(argv []string) ([]string, string) {
	slots, ok := snapArgSlots[argv[0]]
	if !ok {
		return argv, ""
	}
	out := append([]string(nil), argv...)
	// Multi-delete takes any number of ids, so every positional slot counts;
	// other commands only expand their declared slots.
	positions := make([]int, 0, len(out)-1)
	if argv[0] == "delete" {
		for idx := 1; idx < len(out); idx++ {
			if !strings.HasPrefix(out[idx], "-") {
				positions = append(positions, idx)
			}
		}
	} else {
		for _, s := range slots {
			positions = append(positions, s+1) // argv[0] is the command name
		}
	}
	for _, idx := range positions {
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
// output pane. Destructive commands (delete, prune, rollback) are confirmed
// with a TUI dialog first and run with --yes — their own console [y/N] prompt
// would be invisible here and would deadlock on raw-mode input.
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
// (a header line plus ids or paths) for the ':' command line.
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
		return out
	}
	if len(t.pathComp) > 0 {
		out = append(out, [2]any{fmt.Sprintf(" %d path match(es) for %q — Tab completes:",
			len(t.pathComp), currentToken(t.input)), false})
		for i, p := range t.pathComp {
			if len(out) >= budget-1 {
				out = append(out, [2]any{fmt.Sprintf("   … %d more", len(t.pathComp)-i), false})
				break
			}
			out = append(out, [2]any{"   " + p, false})
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
	case dlgList:
		items := d.visibleListItems()
		out = append(out, [2]any{" " + d.title, false})
		if len(items) == 0 {
			message := "   (nothing to list)"
			if d.filter {
				message = "   (no rows match — type less, or Esc to cancel)"
			}
			out = append(out, [2]any{message, false})
		}
		// align the status notes within the rows
		w := 0
		for _, it := range items {
			if len(it.text) > w {
				w = len(it.text)
			}
		}
		if w > t.w-24 {
			w = t.w - 24
		}
		for i, it := range items {
			if len(out) >= t.mainH()-1 {
				out = append(out, [2]any{"   …", false})
				break
			}
			box := "  " // single-choice rows have no checkbox
			if d.multi {
				box = "[ ]"
				if d.checked[i] {
					box = "[x]"
				}
			}
			s := fmt.Sprintf("   %s %-*s", box, w, it.text)
			if it.note != "" {
				s += "  " + it.note
			}
			out = append(out, [2]any{s, i == d.pickSel})
		}
	default:
		out = append(out, [2]any{" " + d.title, false})
		// Path dialogs list the directory matches above the input line.
		reserve := 1
		if d.kind == dlgText && d.path && d.value != "" {
			ms := pathMatches(d.value)
			if len(ms) > 0 {
				out = append(out, [2]any{fmt.Sprintf(" %d path match(es) — Tab completes:", len(ms)), false})
				for i, m := range ms {
					if i >= 5 {
						out = append(out, [2]any{fmt.Sprintf("   … %d more", len(ms)-i), false})
						break
					}
					out = append(out, [2]any{"   " + m, false})
				}
				reserve = len(out) // rows the dialog takes from the output pane
			}
		}
		// y/n and text dialogs sit over the output pane so the result they
		// ask about (a dry-run plan, a doctor report) stays readable instead
		// of flashing away when the dialog opens.
		if t.inOutput {
			out = append(out, t.outputViewReserve(reserve)...)
		}
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
	case t.dlg != nil && t.dlg.kind == dlgList && t.dlg.multi:
		return " ↑↓/j/k move · space/x toggle · a all/none · Enter confirm · Esc cancel"
	case t.dlg != nil && t.dlg.kind == dlgList && t.dlg.filter:
		return " ↑↓ select · type to filter · Enter confirm · Esc cancel"
	case t.dlg != nil && t.dlg.kind == dlgList:
		return " ↑↓/j/k select · Enter confirm · Esc cancel"
	case t.dlg != nil && t.dlg.kind == dlgBool:
		return " y/n · Enter = default · Esc cancel"
	case t.dlg != nil && t.dlg.path:
		return " type path · Tab completes · Enter confirm · Esc cancel"
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
		case dlgList:
			if d.multi {
				n := 0
				for _, on := range d.checked {
					if on {
						n++
					}
				}
				return fmt.Sprintf(" %d of %d selected — Enter confirms", n, len(d.items))
			}
			if d.filter {
				return fmt.Sprintf(" filter: %s", d.value)
			}
			return " Enter picks the highlighted row"
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
