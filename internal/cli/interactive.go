// Interactive menu: `dockervc cli` opens a guided shell around the same
// command tree, so the command surface doesn't have to be memorized. Every
// action funnels through the real cobra commands — the menu adds prompts,
// not logic.
package cli

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"dockervc/internal/snapshot"
	"dockervc/internal/store"
)

var stdinReader *bufio.Reader

// input returns the shared stdin reader. confirm() and the menu prompts must
// share one buffered reader, or a pasted answer can sit in the other half's
// buffer and never be seen.
func input() *bufio.Reader {
	if stdinReader == nil {
		stdinReader = bufio.NewReader(os.Stdin)
	}
	return stdinReader
}

var interactiveCmd = &cobra.Command{
	Use:     "cli",
	Aliases: []string{"interactive", "menu", "ui"},
	Short:   "Open the interactive menu (guided access to every command)",
	Args:    cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return openMenu()
	},
}

// action is one menu entry: the command, its argument shape (shown as a
// hint), a one-line description, and optionally a guided flow that collects
// arguments by prompting and returns the argv to execute (nil = cancelled).
type action struct {
	name   string
	args   string
	desc   string
	guided func(r *bufio.Reader) []string
}

var menuActions = []action{
	{"status", "[--deep --volumes names]", "what changed since the last snapshot?", guidedStatus},
	{"snapshot", "-m <msg> [--stop]", "capture the current engine state", guidedSnapshot},
	{"log", "", "list snapshots", nil},
	{"show", "<snap>", "inspect one snapshot", guidedShow},
	{"diff", "<snapA> [snapB]", "compare snapshots; --files <vol> lists changed files", guidedDiff},
	{"delete", "<snap>…", "drop one or more snapshots", guidedDelete},
	{"doctor", "[--deep] [--repair]", "store health check; offers repair when problems are found", guidedDoctor},
	{"prune", "", "reclaim storage from unreferenced objects", nil},
	{"config", "[get|set|unset]", "view or change store settings", guidedConfig},
	{"rollback", "<snap> [--all]", "restore engine state from a snapshot", guidedRollback},
	{"export", "<snap> [-o <file>]", "package a snapshot into a portable .dvca archive", guidedExport},
	{"import", "<archive.dvca>", "add an archive's snapshot to this store", guidedImport},
}

// RunInteractive runs the menu loop until quit/EOF.
func RunInteractive() error {
	r := input()
	fmt.Printf("dockervc %s — interactive menu\n", Version)
	fmt.Printf("store: %s\n", storeStatusLine())
	fmt.Println("type a number, a command (flags allowed), `help`, or `quit`.")
	for {
		fmt.Println()
		for i, a := range menuActions {
			args := a.args
			if args == "" {
				args = "(no args)"
			}
			fmt.Printf("  %2d) %-10s %-18s %s\n", i+1, a.name, args, a.desc)
		}
		fmt.Print("dockervc> ")
		line, err := r.ReadString('\n')
		if err != nil { // EOF (Ctrl-D) — quit
			fmt.Println()
			return nil
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		switch strings.ToLower(line) {
		case "q", "quit", "exit":
			return nil
		case "?", "help", "man":
			printMan()
			continue
		}
		if n, err := strconv.Atoi(line); err == nil {
			runMenuAction(n-1, r)
			continue
		}
		runArgs(splitQuoted(line)...)
	}
}

func runMenuAction(i int, r *bufio.Reader) {
	if i < 0 || i >= len(menuActions) {
		fmt.Printf("no action %d (pick 1–%d)\n", i+1, len(menuActions))
		return
	}
	a := menuActions[i]
	argv := []string{a.name}
	if a.guided != nil {
		argv = a.guided(r)
		if argv == nil {
			fmt.Println("cancelled.")
			return
		}
	}
	if a.name == "doctor" {
		// Run the check with output captured, then offer the repair right in
		// the menu when it found problems — no command typing needed.
		fmt.Printf("running %s …\n", strings.Join(argv, " "))
		out := captureOutput(func() { runArgs(argv...) })
		fmt.Print(out)
		if n, found := problemCountIn(strings.Split(out, "\n")); found &&
			promptBool(r, fmt.Sprintf("%d problem(s) found — run repair now?", n), false) {
			runArgs(append([]string{"doctor", "--repair"}, argv[1:]...)...)
		}
		return
	}
	runArgs(argv...)
}

// runArgs executes the real cobra command tree with the given arguments.
func runArgs(argv ...string) {
	if len(argv) > 0 && (argv[0] == "dockervc" || argv[0] == "./dockervc") {
		argv = argv[1:]
	}
	if len(argv) == 0 {
		return
	}
	resetCommandFlags()
	rootCmd.SetArgs(argv)
	// cobra prints errors itself; the loop continues regardless.
	_ = rootCmd.Execute()
	// cobra skips PersistentPostRun when RunE errors, which would leak the
	// store lock and fail every later command in this process. Close it here
	// unconditionally.
	if st != nil {
		st.Close()
		st = nil
	}
}

// resetCommandFlags clears flag values that linger between executions (they
// are package variables bound once at init).
func resetCommandFlags() {
	snapOpts = struct {
		message    string
		stop       bool
		only       []string
		anonymous  bool
		bindMounts bool
	}{}
	forceYes = false
	noProgress = false
	statusOpts = struct {
		deep    bool
		volumes []string
	}{}
	diffFiles = ""
	doctorRepair = false
	doctorDeep = false
	rollbackOpts = struct {
		all         bool
		containers  []string
		volumes     []string
		images      []string
		networks    []string
		dryRun      bool
		keepCurrent bool
	}{}
	exportOpts = struct {
		out    string
		latest bool
		split  string
	}{}
	importOpts = struct {
		apply bool
	}{}
}

// ── guided flows ─────────────────────────────────────────────────────────────

func guidedSnapshot(r *bufio.Reader) []string {
	msg := promptLine(r, "message", "interactive snapshot")
	argv := []string{"snapshot", "-m", msg}
	if promptBool(r, "stop containers first for app-consistent data?", false) {
		argv = append(argv, "--stop")
	}
	return argv
}

func guidedShow(r *bufio.Reader) []string {
	id := pickSnapshot(r, "show which snapshot?", false)
	if id == "" {
		return nil
	}
	return []string{"show", id}
}

func guidedDiff(r *bufio.Reader) []string {
	a := pickSnapshot(r, "compare FROM which snapshot?", false)
	if a == "" {
		return nil
	}
	b := pickSnapshot(r, "TO which snapshot? (blank = live engine)", true)
	if b == "" {
		return []string{"diff", a}
	}
	return []string{"diff", a, b}
}

// guidedDelete picks one or more snapshots; the command's own confirm names
// the whole batch, so no --yes is pre-baked here.
func guidedDelete(r *bufio.Reader) []string {
	ids := pickSnapshots(r, "delete which snapshot(s)?")
	if len(ids) == 0 {
		return nil
	}
	return append([]string{"delete"}, ids...)
}

// guidedRollback picks a snapshot, then asks per kind what to restore — a
// container selection carries its image, mounted volumes and networks along.
// Choosing every kind present collapses to --all (same selection, shorter
// argv). No --yes here: the command itself prints the plan and asks once more,
// which is the point of a rollback prompt.
func guidedRollback(r *bufio.Reader) []string {
	id := pickSnapshot(r, "roll back to which snapshot?", false)
	if id == "" {
		return nil
	}
	s, err := store.Open(storePath)
	if err != nil {
		fmt.Printf("cannot open store: %v\n", err)
		return nil
	}
	m, err := s.GetSnapshot(id)
	s.Close()
	if err != nil {
		fmt.Printf("cannot read snapshot %s: %v\n", id, err)
		return nil
	}
	wantC := len(m.Containers) > 0 && promptBool(r, fmt.Sprintf("restore all %d container(s)?", len(m.Containers)), false)
	wantV := len(m.Volumes) > 0 && promptBool(r, fmt.Sprintf("restore all %d volume(s)?", len(m.Volumes)), false)
	wantI := len(m.Images) > 0 && promptBool(r, fmt.Sprintf("restore all %d image(s)?", len(m.Images)), false)
	wantN := len(m.Networks) > 0 && promptBool(r, fmt.Sprintf("restore all %d network(s)?", len(m.Networks)), false)
	if !wantC && !wantV && !wantI && !wantN {
		fmt.Println("nothing selected — a rollback must name what it restores.")
		return nil
	}
	if (wantC || len(m.Containers) == 0) && (wantV || len(m.Volumes) == 0) &&
		(wantI || len(m.Images) == 0) && (wantN || len(m.Networks) == 0) {
		return []string{"rollback", id, "--all"}
	}
	argv := []string{"rollback", id}
	if wantC {
		names := make([]string, 0, len(m.Containers))
		for i := range m.Containers {
			names = append(names, m.Containers[i].Name)
		}
		argv = append(argv, "--containers", strings.Join(names, ","))
	}
	if wantV {
		names := make([]string, 0, len(m.Volumes))
		for i := range m.Volumes {
			names = append(names, m.Volumes[i].Name)
		}
		argv = append(argv, "--volumes", strings.Join(names, ","))
	}
	if wantI {
		names := make([]string, 0, len(m.Images))
		for i := range m.Images {
			if len(m.Images[i].Refs) > 0 {
				names = append(names, m.Images[i].Refs[0])
			} else {
				names = append(names, m.Images[i].Digest)
			}
		}
		argv = append(argv, "--images", strings.Join(names, ","))
	}
	if wantN {
		names := make([]string, 0, len(m.Networks))
		for i := range m.Networks {
			names = append(names, m.Networks[i].Name)
		}
		argv = append(argv, "--networks", strings.Join(names, ","))
	}
	return argv
}

// guidedExport picks a snapshot, then asks where to write the archive. The
// suggested default matches the bare command: the export.folder setting,
// else exports/<snapID>.dvca in the current directory.
func guidedExport(r *bufio.Reader) []string {
	id := pickSnapshot(r, "export which snapshot?", false)
	if id == "" {
		return nil
	}
	path := promptLine(r, "output path", defaultExportPath(id))
	return []string{"export", id, "-o", path}
}

// guidedImport asks for the archive, then whether to chain straight into a
// restore (--apply). The command's own single confirm covers the apply.
func guidedImport(r *bufio.Reader) []string {
	// The line menu can't intercept Tab, so surface the candidates instead:
	// archives from the common locations are listed above the prompt.
	items := findArchives()
	def := ""
	if len(items) == 1 {
		def = items[0].value // one archive around — Enter takes it
	}
	if len(items) > 0 {
		fmt.Println("archives found:")
		for _, it := range items {
			fmt.Printf("   %s  (%s)\n", it.value, it.note)
		}
	}
	path := promptLine(r, "archive path (.dvca)", def)
	if path == "" {
		fmt.Println("no archive path given.")
		return nil
	}
	if promptBool(r, "also roll the engine back to it after import? (--apply)", false) {
		return []string{"import", "--apply", path}
	}
	return []string{"import", path}
}

func guidedDoctor(r *bufio.Reader) []string {
	argv := []string{"doctor"}
	if promptBool(r, "also re-hash every object? (deep check, slower)", false) {
		argv = append(argv, "--deep")
	}
	return argv
}

// guidedConfig shows the export-folder state, then offers to change or reset
// it — the setting people actually reach for. Declining every prompt runs the
// bare command, which lists all settings like before.
func guidedConfig(r *bufio.Reader) []string {
	if cur := exportFolderSetting(); cur != "" {
		fmt.Printf("export folder: %s\n", displayDir(cur))
		if !promptBool(r, "change the export folder?", false) {
			if promptBool(r, "reset it to the default (exports/ in the current directory)?", false) {
				return []string{"config", "unset", "export.folder"}
			}
			return []string{"config"}
		}
		dir := promptLine(r, "new export folder (an existing directory)", cur)
		if dir == "" {
			fmt.Println("no folder given.")
			return nil
		}
		return []string{"config", "set", "export.folder", dir}
	}
	fmt.Println("export folder: not set (exports/ in the current directory)")
	if !promptBool(r, "set a default export folder?", false) {
		return []string{"config"}
	}
	dir := promptLine(r, "export folder (an existing directory)", "")
	if dir == "" {
		fmt.Println("no folder given.")
		return nil
	}
	return []string{"config", "set", "export.folder", dir}
}

// pickSnapshot lists snapshots newest-first and resolves a number or an
// ID/prefix. Empty input returns "" (cancel, or "live engine" when optional).
func pickSnapshot(r *bufio.Reader, label string, optional bool) string {
	rows := listSnapshots(label)
	if rows == nil {
		return ""
	}
	hint := "number or snapshot id"
	if optional {
		hint += " (blank = none)"
	}
	ans := promptLine(r, hint, "")
	if ans == "" {
		return ""
	}
	if n, err := strconv.Atoi(ans); err == nil {
		if n >= 1 && n <= len(rows) {
			return rows[len(rows)-n].ID
		}
		fmt.Printf("%d is out of range (1–%d)\n", n, len(rows))
		return ""
	}
	return ans
}

// pickSnapshots resolves one or more picks — numbers or ids, comma- or
// space-separated, duplicates collapsed. Empty input returns nil (cancel).
func pickSnapshots(r *bufio.Reader, label string) []string {
	rows := listSnapshots(label)
	if rows == nil {
		return nil
	}
	newest := make([]string, len(rows)) // display order: newest is row 1
	for i := range rows {
		newest[len(rows)-1-i] = rows[i].ID
	}
	ans := promptLine(r, "number(s) or snapshot id(s), comma-separated", "")
	if ans == "" {
		return nil
	}
	var ids []string
	seen := map[string]bool{}
	for _, tok := range strings.FieldsFunc(ans, func(c rune) bool {
		return c == ',' || c == ' ' || c == '\t'
	}) {
		if n, err := strconv.Atoi(tok); err == nil {
			if n < 1 || n > len(rows) {
				fmt.Printf("%d is out of range (1–%d)\n", n, len(rows))
				return nil
			}
			tok = newest[n-1]
		}
		if !seen[tok] {
			seen[tok] = true
			ids = append(ids, tok)
		}
	}
	return ids
}

// listSnapshots prints the newest-first pick list under label and returns the
// rows oldest-first; nil when the store is unreadable or empty.
func listSnapshots(label string) []store.SnapshotRow {
	s, err := store.Open(storePath)
	if err != nil {
		fmt.Printf("cannot open store: %v\n", err)
		return nil
	}
	rows, err := s.ListSnapshots()
	s.Close()
	if err != nil {
		fmt.Printf("cannot list snapshots: %v\n", err)
		return nil
	}
	if len(rows) == 0 {
		fmt.Println("no snapshots yet — take one first (menu action 2).")
		return nil
	}
	fmt.Printf("%s\n", label)
	for i := len(rows) - 1; i >= 0; i-- { // newest first, matching `log`
		row := rows[i]
		fmt.Printf("  %2d) %-26s %s  %s\n", len(rows)-i, row.ID,
			row.CreatedAt.Local().Format("2006-01-02 15:04"), row.Message)
	}
	return rows
}

// ── small prompt helpers ─────────────────────────────────────────────────────

func promptLine(r *bufio.Reader, label, def string) string {
	if def != "" {
		fmt.Printf("%s [%s]: ", label, def)
	} else {
		fmt.Printf("%s: ", label)
	}
	line, err := r.ReadString('\n')
	if err != nil && line == "" {
		return def
	}
	if line = strings.TrimSpace(line); line == "" {
		return def
	}
	return line
}

func promptBool(r *bufio.Reader, label string, def bool) bool {
	hint := "y/N"
	if def {
		hint = "Y/n"
	}
	fmt.Printf("%s [%s]: ", label, hint)
	line, _ := r.ReadString('\n')
	line = strings.ToLower(strings.TrimSpace(line))
	if line == "" {
		return def
	}
	return line == "y" || line == "yes"
}

// splitQuoted splits on whitespace, honoring single/double quotes so
// `snapshot -m "my message"` carries the message as one argument.
func splitQuoted(s string) []string {
	var out []string
	var cur strings.Builder
	inSingle, inDouble := false, false
	flush := func() {
		if cur.Len() > 0 {
			out = append(out, cur.String())
			cur.Reset()
		}
	}
	for _, c := range s {
		switch {
		case c == '\'' && !inDouble:
			inSingle = !inSingle
		case c == '"' && !inSingle:
			inDouble = !inDouble
		case (c == ' ' || c == '\t') && !inSingle && !inDouble:
			flush()
		default:
			cur.WriteRune(c)
		}
	}
	flush()
	return out
}

// ── header ───────────────────────────────────────────────────────────────────

func storeStatusLine() string {
	if _, err := os.Stat(filepath.Join(storePath, "dockervc.db")); err != nil {
		return fmt.Sprintf("%s (not initialized — run `dockervc init`)", storePath)
	}
	s, err := store.Open(storePath)
	if err != nil {
		return fmt.Sprintf("%s (unavailable: %v)", storePath, err)
	}
	defer s.Close()
	rows, err := s.ListSnapshots()
	if err != nil {
		return storePath
	}
	var total int64
	for _, row := range rows {
		total += row.Size
	}
	return fmt.Sprintf("%s — %d snapshot(s), %s", storePath, len(rows), snapshot.HumanBytes(total))
}

// ── man page ─────────────────────────────────────────────────────────────────

var manCmd = &cobra.Command{
	Use:   "man",
	Short: "Print the full command reference (all commands and flags)",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		printMan()
		return nil
	},
}

// printMan renders a man-page-style reference from the live command tree, so
// it can never drift from the actual commands.
func printMan() {
	fmt.Printf("DOCKERVC(1)\n\n")
	fmt.Printf("NAME\n    dockervc — %s\n", rootCmd.Short)
	fmt.Printf("\nSYNOPSIS\n    dockervc <command> [flags] [args]\n")
	fmt.Printf("    dockervc cli      # interactive menu\n")

	fmt.Printf("\nCOMMANDS\n")
	cmds := rootCmd.Commands()
	sort.Slice(cmds, func(i, j int) bool { return cmds[i].Name() < cmds[j].Name() })
	for _, c := range cmds {
		if c.Hidden || c.Deprecated != "" || c.Name() == "completion" || c.Name() == "help" {
			continue // hidden and deprecated commands don't belong in the reference
		}
		if len(c.Aliases) > 0 {
			fmt.Printf("  %s (alias: %s)\n", c.Use, strings.Join(c.Aliases, ", "))
		} else {
			fmt.Printf("  %s\n", c.Use)
		}
		fmt.Printf("      %s\n", c.Short)
		c.LocalFlags().VisitAll(func(f *pflag.Flag) {
			if f.Name == "help" {
				return
			}
			flag := "      --" + f.Name
			if f.Shorthand != "" {
				flag = "      -" + f.Shorthand + ", --" + f.Name
			}
			if f.Value.Type() != "bool" {
				flag += " <" + f.Name + ">"
			}
			fmt.Printf("        %-38s %s\n", flag, f.Usage)
		})
	}

	fmt.Printf("\nGLOBAL FLAGS\n")
	rootCmd.PersistentFlags().VisitAll(func(f *pflag.Flag) {
		if f.Name == "help" {
			return
		}
		fmt.Printf("  --%s <%s>\n      %s\n", f.Name, f.Name, f.Usage)
	})
	fmt.Printf("\nNOTES\n")
	fmt.Printf("    Snapshots are content-addressed and deduplicated: unchanged data is\n")
	fmt.Printf("    stored once and reused across snapshots. Delete removes the catalog\n")
	fmt.Printf("    entry; prune reclaims the storage of unreferenced objects.\n")
	fmt.Printf("    Store location, overrides and more: see README.md and DESIGN.md.\n")
}

func init() {
	rootCmd.AddCommand(interactiveCmd)
	rootCmd.AddCommand(manCmd)
}
