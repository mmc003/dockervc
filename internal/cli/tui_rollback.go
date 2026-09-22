// Menu-driven rollback scope selection for the full-screen TUI: after the
// snapshot picker, show what a rollback could restore (everything, or a
// per-kind checklist of the entities that changed or vanished since the
// snapshot) built from a real BuildPlan comparison — so the checklist can
// never disagree with the dry-run plan it precedes.
package cli

import (
	"context"
	"fmt"
	"strings"

	"dockervc/internal/dockerapi"
	"dockervc/internal/model"
	"dockervc/internal/rollback"
	"dockervc/internal/store"
)

// rollbackKinds is the checklist order — the same order askScope and the
// rollback flags use.
var rollbackKinds = []string{"containers", "volumes", "images", "networks"}

// askRollbackScope runs the scope selection for rolling back to snapshot id:
// everything (--all) or one kind's checklist, then the dry-run/apply prompts.
// When the live comparison isn't possible (engine down, store closed) it
// falls back to askScope's plain name prompts.
func (t *tui) askRollbackScope(id string) {
	m, live, errMsg := fetchRollbackState(id)
	if errMsg != "" {
		t.flash = errMsg + " — name prompts instead"
		t.askScope(func(scope []string) {
			t.rollbackWithScope(id, scope)
		})
		return
	}
	t.rollbackScopeMenu(id, rollbackEntityLists(m, live))
}

// rollbackScopeMenu shows the top-level choice: whole engine or one kind.
// Re-entered (not refetched) when the user backs out of a checklist.
func (t *tui) rollbackScopeMenu(id string, lists map[string][]listItem) {
	menu := []listItem{
		{text: "restore everything (--all)", note: "full restore", value: "--all"},
	}
	for _, kind := range rollbackKinds {
		note := fmt.Sprintf("%d to restore", len(lists[kind]))
		if len(lists[kind]) == 0 {
			note = "nothing to restore"
		}
		menu = append(menu, listItem{text: kind, note: note, value: kind})
	}
	t.askListOne("what should this rollback restore?", menu, func(val string, ok bool) {
		if !ok || val == "" {
			return
		}
		if val == "--all" {
			t.rollbackWithScope(id, []string{"--all"})
			return
		}
		items := lists[val]
		if len(items) == 0 {
			t.flash = "no " + val + " changed or missing vs this snapshot"
			t.rollbackScopeMenu(id, lists)
			return
		}
		t.rollbackChecklist(id, val, items, lists)
	})
}

// rollbackChecklist shows one kind's entities — gone ones pre-checked,
// existing ones listed but unchecked; the checked names become the
// rollback's --<kind> flag. Esc backs out to the scope menu, Enter with
// nothing checked re-asks (a rollback must name what it restores).
func (t *tui) rollbackChecklist(id, kind string, items []listItem, lists map[string][]listItem) {
	t.askListMany("restore which "+kind+"?", items, func(vals []string, ok bool) {
		if !ok {
			t.rollbackScopeMenu(id, lists) // Esc = back, not out
			return
		}
		if len(vals) == 0 {
			t.flash = "nothing selected — a rollback must name what it restores"
			t.rollbackChecklist(id, kind, items, lists)
			return
		}
		t.rollbackWithScope(id, []string{"--" + kind, strings.Join(vals, ",")})
	})
}

// rollbackWithScope asks how selected containers should be handled, then
// finishes with the dry-run/apply prompts. Non-container scopes go directly
// to the ordinary snapshot-restore flow.
func (t *tui) rollbackWithScope(id string, scope []string) {
	if scope == nil {
		return
	}
	if !rollbackScopeHasContainers(scope) {
		t.rollbackFinish(id, scope, false)
		return
	}
	items := []listItem{
		{
			text: "restore containers and dependencies to the snapshot",
			note: "ordinary rollback", value: "restore",
		},
		{
			text: "recreate using current images, volumes, and networks",
			note: "replace selected containers; keep dependency data", value: "current",
		},
		{
			text: "create only containers that are missing",
			note: "leave existing containers and dependencies unchanged", value: "missing",
		},
	}
	t.askListOne("what should happen to selected containers?", items, func(strategy string, ok bool) {
		if !ok {
			return
		}
		flag, valid := rollbackStrategyFlag(strategy)
		if !valid {
			t.flash = "unknown rollback strategy"
			return
		}
		selected := append([]string(nil), scope...)
		if flag != "" {
			selected = append(selected, flag)
		}
		t.rollbackFinish(id, selected, flag != "")
	})
}

func rollbackScopeHasContainers(scope []string) bool {
	for _, arg := range scope {
		if arg == "--all" || arg == "--containers" {
			return true
		}
	}
	return false
}

// rollbackFinish shows a preview and then offers to apply. Name-trusting
// strategies always preview because their most important behavior is what
// they deliberately leave untouched.
func (t *tui) rollbackFinish(id string, scope []string, requirePreview bool) {
	apply := func() {
		t.askBool("apply the rollback now?", false, func(yes bool, ok bool) {
			if !ok || !yes {
				return
			}
			argv := append([]string{"rollback", "--yes"}, scope...)
			t.execTUI(append(argv, id)...)
		})
	}
	if requirePreview {
		argv := append([]string{"rollback"}, scope...)
		t.doExec(append(argv, "--dry-run", id)...)
		apply()
		return
	}
	t.askBool("preview the restore plan first (dry run)?", true, func(dry bool, ok bool) {
		if !ok {
			return
		}
		if dry {
			argv := append([]string{"rollback"}, scope...)
			t.doExec(append(argv, "--dry-run", id)...)
		}
		apply()
	})
}

// fetchRollbackState loads the snapshot manifest and inventories the live
// engine for the entity lists. errMsg is user-facing, empty on success.
func fetchRollbackState(id string) (m *model.Manifest, live *rollback.LiveState, errMsg string) {
	s, err := store.Open(storePath)
	if err != nil {
		return nil, nil, "cannot open store: " + err.Error()
	}
	defer s.Close()
	m, err = s.GetSnapshot(id)
	if err != nil {
		return nil, nil, "cannot read snapshot: " + err.Error()
	}
	dcli, err := dockerapi.New()
	if err != nil {
		return nil, nil, "docker: " + err.Error()
	}
	ctx := context.Background()
	if err := dcli.Ping(ctx); err != nil {
		return nil, nil, "cannot reach docker engine: " + err.Error()
	}
	live, err = rollback.FetchLiveState(ctx, dcli)
	if err != nil {
		return nil, nil, "inspect live engine: " + err.Error()
	}
	return m, live, ""
}

// rollbackEntityLists turns a snapshot and live state into per-kind checklist
// items. Notes mirror BuildPlan's own reasoning so the checklist and the
// dry-run plan agree: containers are always recreated (rollback semantics),
// existing volumes are replaced, existing networks/images are reused.
func rollbackEntityLists(m *model.Manifest, live *rollback.LiveState) map[string][]listItem {
	lists := map[string][]listItem{}

	for i := range m.Containers {
		c := &m.Containers[i]
		note := "exists — will be replaced"
		gone := false
		if _, liveNow := live.ContainerIDs[c.Name]; !liveNow {
			note, gone = "deleted", true
		}
		if c.ImageObject == "" {
			note = "no captured filesystem — skipped"
			gone = false // selecting it restores nothing; don't pre-check
		}
		lists["containers"] = append(lists["containers"],
			listItem{text: c.Name, note: note, value: c.Name, on: gone})
	}

	for i := range m.Volumes {
		v := &m.Volumes[i]
		note := "exists — contents replaced"
		gone := false
		if !live.VolumeNames[v.Name] {
			note, gone = "deleted", true
		}
		lists["volumes"] = append(lists["volumes"],
			listItem{text: v.Name, note: note, value: v.Name, on: gone})
	}

	for i := range m.Images {
		im := &m.Images[i]
		if im.Digest == "" {
			continue // not individually selectable (no digest to scope by)
		}
		if live.ImageDigests[im.Digest] {
			missing := 0
			for _, ref := range im.Refs {
				if !live.ImageTags[ref] {
					missing++
				}
			}
			if missing == 0 {
				continue // already present and tagged — a pure no-op
			}
			lists["images"] = append(lists["images"], listItem{
				text:  imageListName(im),
				note:  fmt.Sprintf("present — re-applies %d missing tag(s)", missing),
				value: im.Digest,
				// present in the engine — listed for opting in, not pre-checked
			})
			continue
		}
		lists["images"] = append(lists["images"], listItem{
			text:  imageListName(im),
			note:  "deleted",
			value: im.Digest,
			on:    true,
		})
	}

	for i := range m.Networks {
		n := &m.Networks[i]
		if live.NetworkNames[n.Name] {
			continue // reused as-is; a rollback never recreates existing networks
		}
		lists["networks"] = append(lists["networks"],
			listItem{text: n.Name, note: "deleted", value: n.Name, on: true})
	}

	return lists
}

// imageListName picks a human-readable label for an image record: its first
// repo tag, else the digest, truncated so one long ref can't stretch the
// checklist column.
func imageListName(im *model.ImageRecord) string {
	name := im.Digest
	if len(im.Refs) > 0 && im.Refs[0] != "" {
		name = im.Refs[0]
	}
	if len(name) > 40 {
		name = name[:40] + "…"
	}
	return name
}
