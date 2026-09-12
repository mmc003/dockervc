package cli

import (
	"strings"
	"testing"

	"dockervc/internal/model"
	"dockervc/internal/rollback"
)

func TestListDialogMultiSelectKeys(t *testing.T) {
	tt := &tui{w: 80, h: 24}
	var got []string
	var okFlag bool
	tt.askListMany("restore which volumes?", []listItem{
		{text: "demo-data", note: "deleted", value: "demo-data", on: true},
		{text: "other", value: "other"},
	}, func(vals []string, ok bool) { got, okFlag = vals, ok })

	// boxes start as their `on` flag: gone entities checked, existing not
	tt.dialogKey(0, keyEnter) // confirm defaults
	if !okFlag {
		t.Fatal("Enter should confirm")
	}
	if len(got) != 1 || got[0] != "demo-data" {
		t.Fatalf("wanted only the pre-checked [demo-data], got %v", got)
	}

	// space toggles an unchecked row on
	tt.askListMany("pick", []listItem{
		{text: "a", value: "a"},
		{text: "b", value: "b", on: true},
	}, func(vals []string, ok bool) { got, okFlag = vals, ok })
	tt.dialogKey(0, keyEnter)
	if len(got) != 1 || got[0] != "b" {
		t.Fatalf("wanted [b], got %v", got)
	}

	// 'a' flips every box the other way: all-on becomes all-off
	tt.askListMany("pick", []listItem{{value: "a"}, {value: "b", on: true}},
		func(vals []string, ok bool) { got, okFlag = vals, ok })
	tt.dialogKey('a', keyNone)
	tt.dialogKey(0, keyEnter)
	if !okFlag || len(got) != 0 {
		t.Fatalf("'a' should clear all boxes, got %v ok=%v", got, okFlag)
	}

	// Esc cancels without a selection
	tt.askListMany("pick", []listItem{{value: "a"}},
		func(vals []string, ok bool) { got, okFlag = vals, ok })
	tt.dialogKey(0, keyEsc)
	if okFlag || got != nil {
		t.Fatalf("Esc should cancel, got %v ok=%v", got, okFlag)
	}
}

func TestListDialogSingleSelectRenders(t *testing.T) {
	tt := &tui{w: 80, h: 24}
	tt.askListOne("what should this rollback restore?", []listItem{
		{text: "restore everything (--all)", note: "full restore", value: "--all"},
		{text: "volumes", note: "1 to restore", value: "volumes"},
	}, func(string, bool) {})
	tt.dialogKey(0, keyDown) // cursor → volumes

	joined := ""
	for _, r := range tt.bodyView() {
		joined += r[0].(string) + "\n"
	}
	for _, want := range []string{
		"what should this rollback restore?",
		"restore everything (--all)",
		"volumes",
		"1 to restore",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("list view missing %q in:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "[x]") {
		t.Error("single-choice rows should not render checkboxes")
	}
}

func TestRollbackEntityLists(t *testing.T) {
	m := &model.Manifest{
		Containers: []model.ContainerRecord{
			{Name: "demo-web", ImageObject: "hash1"}, // live → replaced
			{Name: "gone-svc", ImageObject: "hash2"}, // absent → deleted
			{Name: "weird", ImageObject: ""},         // not recreatable
		},
		Volumes: []model.VolumeRecord{
			{Name: "demo-data"}, // live → replaced
			{Name: "new-vol"},   // absent → deleted
		},
		Images: []model.ImageRecord{
			{Digest: "sha256:111", Refs: []string{"nginx:latest"}},            // absent
			{Digest: "sha256:222", Refs: []string{"busybox:latest", "x:tag"}}, // present, 1 tag missing
			{Digest: "sha256:333", Refs: []string{"full:latest"}},             // present and tagged
			{Digest: ""}, // not selectable
		},
		Networks: []model.NetworkRecord{
			{Name: "demo-net"},   // absent
			{Name: "still-here"}, // live → reused, never listed
		},
	}
	live := &rollback.LiveState{
		ContainerIDs: map[string]string{"demo-web": "cid"},
		VolumeNames:  map[string]bool{"demo-data": true},
		NetworkNames: map[string]bool{"still-here": true},
		ImageDigests: map[string]bool{"sha256:222": true, "sha256:333": true},
		ImageTags:    map[string]bool{"busybox:latest": true, "full:latest": true},
	}

	lists := rollbackEntityLists(m, live)

	type row struct {
		text, note string
		on         bool
	}
	want := map[string][]row{
		"containers": {
			{"demo-web", "exists — will be replaced", false},
			{"gone-svc", "deleted", true},
			{"weird", "no captured filesystem — skipped", false},
		},
		"volumes": {
			{"demo-data", "exists — contents replaced", false},
			{"new-vol", "deleted", true},
		},
		"images": {
			{"nginx:latest", "deleted", true},
			{"busybox:latest", "present — re-applies 1 missing tag(s)", false},
		},
		"networks": {
			{"demo-net", "deleted", true},
		},
	}
	for kind, rows := range want {
		got := lists[kind]
		if len(got) != len(rows) {
			t.Fatalf("%s: got %d items %v, want %d", kind, len(got), got, len(rows))
		}
		for i, w := range rows {
			if got[i].text != w.text || got[i].note != w.note || got[i].on != w.on {
				t.Errorf("%s[%d] = %q/%q/on=%v, want %q/%q/on=%v",
					kind, i, got[i].text, got[i].note, got[i].on, w.text, w.note, w.on)
			}
		}
	}
	if lists["images"][0].value != "sha256:111" || lists["images"][1].value != "sha256:222" {
		t.Errorf("image rows must carry digests as scope values, got %v", lists["images"])
	}
}

func TestRollbackScopeMenuFlow(t *testing.T) {
	tt := &tui{w: 80, h: 24}
	lists := map[string][]listItem{
		"volumes": {{text: "demo-data", note: "deleted", value: "demo-data", on: true}},
	}
	tt.rollbackScopeMenu("snap-1", lists)

	if tt.dlg == nil || tt.dlg.kind != dlgList || tt.dlg.multi {
		t.Fatal("scope menu should be a single-choice list")
	}
	// Esc from the checklist should come back here, not abort
	title := tt.dlg.title

	tt.dialogKey(0, keyDown) // → containers (empty in lists)
	tt.dialogKey(0, keyEnter)
	if tt.flash == "" || !strings.Contains(tt.flash, "containers") {
		t.Errorf("selecting an empty kind should flash a note, got %q", tt.flash)
	}
	if tt.dlg == nil || tt.dlg.multi || tt.dlg.title != title {
		t.Fatalf("empty kind should re-open the scope menu, got %+v", tt.dlg)
	}

	tt.dialogKey(0, keyDown) // → containers
	tt.dialogKey(0, keyDown) // → volumes
	tt.dialogKey(0, keyEnter)
	if tt.dlg == nil || !tt.dlg.multi || !strings.Contains(tt.dlg.title, "volumes") {
		t.Fatalf("expected the volumes checklist, got %+v", tt.dlg)
	}

	tt.dialogKey(0, keyEsc) // back to the menu
	if tt.dlg == nil || tt.dlg.multi || tt.dlg.title != title {
		t.Fatalf("Esc should return to the scope menu, got %+v", tt.dlg)
	}

	// again into volumes, confirm all-checked → dry-run prompt, no execution
	tt.dialogKey(0, keyDown)
	tt.dialogKey(0, keyDown)
	tt.dialogKey(0, keyEnter)
	tt.dialogKey(0, keyEnter)
	if tt.dlg == nil || tt.dlg.kind != dlgBool || !strings.Contains(tt.dlg.title, "dry run") {
		t.Fatalf("checklist confirm should reach the dry-run prompt, got %+v", tt.dlg)
	}
}
