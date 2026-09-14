// Filesystem path Tab-completion for the TUI: the ':' command line (import's
// archive argument, export's -o/--output value) and the export/import path
// dialogs. A token completes against the directory holding its final path
// component; directories come back with a trailing separator so completion
// can keep walking into them, like a shell.
package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"dockervc/internal/snapshot"
	"dockervc/internal/store"
)

// pathMatches returns the directory entries that complete tok, as full
// replacements for the token (directories carry a trailing "/"). Empty when
// nothing matches or the directory can't be read.
func pathMatches(tok string) []string {
	dir, prefix := splitPathToken(tok)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		suffix := ""
		if e.IsDir() {
			// the OS separator, so what lands in the input line is consistent
			suffix = string(filepath.Separator)
		}
		out = append(out, filepath.Join(dir, name)+suffix)
	}
	sort.Strings(out)
	return out
}

// pathSeps is the byte set that ends a path component for completion: "/"
// everywhere, plus "\" on Windows, where it is the native separator (on Unix
// a backslash is a legal filename character and must not split anything).
func pathSeps() string {
	if runtime.GOOS == "windows" {
		return `/\`
	}
	return "/"
}

// splitPathToken splits a completion token into its directory and the last
// path component being typed ("a/b/c" → "a/b", "c"; "c" → ".", "c"). A
// leading "~" expands to the home directory so ~/… completes like a shell.
func splitPathToken(tok string) (dir, prefix string) {
	return splitPathSep(tok, pathSeps())
}

// splitPathSep is splitPathToken with the separator set injected, so the
// backslash behavior is unit-testable on every platform.
func splitPathSep(tok, seps string) (dir, prefix string) {
	tok = expandHome(tok)
	windows := strings.Contains(seps, `\`)
	i := strings.LastIndexAny(tok, seps)
	switch {
	case i == -1:
		// "C:" alone means "current directory of drive C" to Windows APIs;
		// a completer means the drive root, so list that.
		if windows && isDrive(tok) {
			return tok + `\`, ""
		}
		return ".", tok
	case i == 0:
		return tok[:1], tok[1:] // "/" (or "\") root
	default:
		dir := tok[:i]
		if windows && isDrive(dir) { // "C:\Users" → directory "C:\"
			dir += `\`
		}
		return dir, tok[i+1:]
	}
}

// isDrive reports whether s is a bare drive reference like "C:".
func isDrive(s string) bool {
	return len(s) == 2 && s[1] == ':' &&
		(s[0] >= 'a' && s[0] <= 'z' || s[0] >= 'A' && s[0] <= 'Z')
}

// lcp is the longest common prefix of the strings.
func lcp(ss []string) string {
	if len(ss) == 0 {
		return ""
	}
	p := ss[0]
	for _, s := range ss[1:] {
		for !strings.HasPrefix(s, p) && len(p) > 0 {
			p = p[:len(p)-1]
		}
	}
	return p
}

// pathToken reports whether the last token of toks (command name first) is a
// filesystem path argument: import's archive, export's -o/--output value, or
// the directory of `config set export.folder`.
func pathToken(toks []string) bool {
	if len(toks) < 2 {
		return false
	}
	last := len(toks) - 1
	switch toks[0] {
	case "import":
		return last == 1
	case "export":
		return last >= 2 && (toks[last-1] == "-o" || toks[last-1] == "--output")
	case "config":
		return last == 3 && toks[1] == "set" && toks[2] == "export.folder"
	}
	return false
}

// ── export folder & common locations ──────────────────────────────────────────

// exportFolderSetting is the user's configured export folder
// (`dockervc config set export.folder <dir>`), "" when unset or the store
// can't be read. It reuses the command's open store when one is held —
// re-opening would trip the store lock mid-command.
func exportFolderSetting() string {
	var v string
	var ok bool
	var err error
	if st != nil {
		v, ok, err = st.ConfigGet("export.folder")
	} else {
		s, oerr := store.Open(storePath)
		if oerr != nil {
			return ""
		}
		defer s.Close()
		v, ok, err = s.ConfigGet("export.folder")
	}
	if err != nil || !ok || v == "" {
		return ""
	}
	return expandHome(v)
}

// defaultExportPath is where exports land by default: the export.folder
// setting when set, else an exports/ folder in the current directory — the
// same default the bare command uses, created on demand.
func defaultExportPath(id string) string {
	if dir := exportFolderSetting(); dir != "" {
		return filepath.Join(dir, id+".dvca")
	}
	return filepath.Join("exports", id+".dvca")
}

// downloadsDir is the user's Downloads folder, or "" when it can't be found.
func downloadsDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	dl := filepath.Join(home, "Downloads")
	if fi, err := os.Stat(dl); err == nil && fi.IsDir() {
		return dl
	}
	return ""
}

// displayDir shortens a directory for prompts: "." for the current directory,
// "~/…" under the home directory, otherwise as-is.
func displayDir(dir string) string {
	if wd, err := os.Getwd(); err == nil && dir == wd {
		return "."
	}
	if home, err := os.UserHomeDir(); err == nil {
		if dir == home {
			return "~"
		}
		if strings.HasPrefix(dir, home+string(filepath.Separator)) {
			return "~" + dir[len(home):]
		}
	}
	return dir
}

// findArchives lists .dvca files from the places an archive usually lands —
// the configured export folder, the current directory and its exports/
// folder, and ~/Downloads — newest first, with size and location as the row
// note. Names appearing in more than one place count once (earlier wins).
func findArchives() []listItem {
	var locs []string
	if dir := exportFolderSetting(); dir != "" {
		locs = append(locs, dir)
	}
	if wd, err := os.Getwd(); err == nil {
		locs = append(locs, wd, filepath.Join(wd, "exports"))
	}
	if dl := downloadsDir(); dl != "" {
		locs = append(locs, dl)
	}
	type found struct {
		item listItem
		mod  time.Time
	}
	var out []found
	seen := map[string]bool{}
	for _, dir := range locs {
		ents, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range ents {
			name := e.Name()
			if e.IsDir() || !strings.EqualFold(filepath.Ext(name), ".dvca") || seen[name] {
				continue
			}
			seen[name] = true
			fi, err := e.Info()
			if err != nil {
				continue
			}
			out = append(out, found{
				item: listItem{
					text:  name,
					note:  fmt.Sprintf("%s · %s", snapshot.HumanBytes(fi.Size()), displayDir(dir)),
					value: filepath.Join(dir, name),
				},
				mod: fi.ModTime(),
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].mod.After(out[j].mod) })
	items := make([]listItem, len(out))
	for i, f := range out {
		items[i] = f.item
	}
	return items
}

// askImportArchive picks the archive to import: .dvca files found in the
// common locations are offered as a list first (with "enter a path manually…"
// as the way out); with none found it goes straight to the path dialog.
func (t *tui) askImportArchive() {
	items := findArchives()
	if len(items) == 0 {
		t.askImportPath("")
		return
	}
	items = append(items, listItem{text: "enter a path manually…", value: ""})
	t.askListOne("import which archive?", items, func(val string, ok bool) {
		if !ok {
			return
		}
		t.askImportPath(val)
	})
}

// askImportPath collects the archive path (pre-filled when picked from the
// list) and the --apply question, then runs the real import command.
func (t *tui) askImportPath(path string) {
	t.askPath("archive path (.dvca)", path, func(path string, ok bool) {
		if !ok || path == "" {
			return
		}
		t.askBool("also roll the engine back to it after import? (--apply)", false, func(apply bool, ok bool) {
			if !ok {
				return
			}
			argv := []string{"import"}
			if apply {
				argv = append(argv, "--apply")
			}
			t.execTUI(append(argv, path)...)
		})
	})
}

// askConfig opens the settings flow: list every setting, set the export
// folder (Tab completes directories), or clear it back to the default. Like
// every menu flow it ends in the real config command.
func (t *tui) askConfig() {
	cur := exportFolderSetting()
	note := "not set — exports land in exports/ here"
	if cur != "" {
		note = "currently " + displayDir(cur)
	}
	items := []listItem{
		{text: "show all settings", note: "every key and its value", value: "show"},
		{text: "set export folder…", note: note, value: "set"},
	}
	if cur != "" {
		items = append(items, listItem{
			text:  "reset export folder",
			note:  "back to exports/ in the current directory",
			value: "unset",
		})
	}
	t.askListOne("settings — what would you like to do?", items, func(val string, ok bool) {
		if !ok {
			return
		}
		switch val {
		case "show":
			t.doExec("config") // read-only listing, like the doctor flow
		case "set":
			t.askPath("export folder (an existing directory)", cur, func(dir string, ok bool) {
				if !ok || dir == "" {
					return
				}
				t.execTUI("config", "set", "export.folder", dir)
			})
		case "unset":
			t.execTUI("config", "unset", "export.folder")
		}
	})
}
