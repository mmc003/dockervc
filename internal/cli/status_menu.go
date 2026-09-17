package cli

import (
	"bufio"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"dockervc/internal/store"
)

// guidedStatus exposes deep volume selection in the line-based interactive
// menu. A blank selection preserves the existing all-volume deep scan.
func guidedStatus(r *bufio.Reader) []string {
	if !promptBool(r, "scan live volume contents? (deep, slower)", false) {
		return []string{"status"}
	}
	items, err := latestStatusVolumeItems()
	if err != nil {
		fmt.Printf("cannot list snapshot volumes: %v\n", err)
		return nil
	}
	if len(items) == 0 {
		return []string{"status", "--deep"}
	}

	fmt.Println("volumes in the latest snapshot:")
	for i, item := range items {
		fmt.Printf("  %2d) %-30s %s\n", i+1, item.text, item.note)
	}
	answer := promptLine(r, "volume number(s) or name(s), comma-separated (blank = all)", "")
	if answer == "" {
		return []string{"status", "--deep"}
	}
	names, err := resolveStatusVolumeSelection(items, answer)
	if err != nil {
		fmt.Println(err)
		return nil
	}
	return statusDeepArgs(names)
}

// latestStatusVolumeItems loads the latest manifest and builds the rows shared
// by both interactive front-ends.
func latestStatusVolumeItems() ([]listItem, error) {
	s, err := store.Open(storePath)
	if err != nil {
		return nil, err
	}
	defer s.Close()
	m, err := s.LatestSnapshot()
	if err != nil || m == nil {
		return nil, err
	}
	items := make([]listItem, 0, len(m.Volumes))
	for _, volume := range m.Volumes {
		note := fmt.Sprintf("%d files", volume.Files)
		if volume.IndexObject == "" {
			note = "file index unavailable"
		}
		items = append(items, listItem{text: volume.Name, note: note, value: volume.Name})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].value < items[j].value })
	return items, nil
}

func resolveStatusVolumeSelection(items []listItem, answer string) ([]string, error) {
	byName := make(map[string]bool, len(items))
	for _, item := range items {
		byName[item.value] = true
	}
	var names []string
	seen := map[string]bool{}
	for _, token := range strings.FieldsFunc(answer, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t'
	}) {
		name := token
		if !byName[name] {
			n, err := strconv.Atoi(token)
			if err != nil || n < 1 || n > len(items) {
				return nil, fmt.Errorf("unknown volume selection %q", token)
			}
			name = items[n-1].value
		}
		if !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("no volumes selected")
	}
	return names, nil
}

func statusDeepArgs(names []string) []string {
	argv := []string{"status", "--deep"}
	if len(names) > 0 {
		argv = append(argv, "--volumes", strings.Join(names, ","))
	}
	return argv
}

// askStatus is the full-screen counterpart of guidedStatus.
func (t *tui) askStatus() {
	t.askBool("scan live volume contents? (deep, slower)", false, func(deep bool, ok bool) {
		if !ok {
			return
		}
		if !deep {
			t.execTUI("status")
			return
		}
		items, err := latestStatusVolumeItems()
		if err != nil {
			t.flash = "cannot list snapshot volumes: " + err.Error()
			return
		}
		if len(items) == 0 {
			t.execTUI("status", "--deep")
			return
		}
		t.askStatusVolumes(items)
	})
}

func (t *tui) askStatusVolumes(items []listItem) {
	t.askListMany("deep-scan which volumes? (a = all)", items, func(names []string, ok bool) {
		if !ok {
			return
		}
		if len(names) == 0 {
			t.flash = "select at least one volume (Space toggles, a selects all)"
			t.askStatusVolumes(items)
			return
		}
		t.execTUI(statusDeepArgs(names)...)
	})
}
