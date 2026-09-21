package cli

import (
	"testing"

	"dockervc/internal/snapshot"
)

func TestVolumePathItemsBuildHierarchy(t *testing.T) {
	idx := &snapshot.FileIndex{Files: map[string]snapshot.FileMeta{
		"etc/app.conf":        {Type: "file", Size: 12},
		"etc/nginx/site.conf": {Type: "file", Size: 20},
		"readme.txt":          {Type: "file", Size: 7},
		"current":             {Type: "symlink", Link: "releases/v2"},
	}}
	root := volumePathItems(idx, "", false)
	assertListValue(t, root, "dir:etc")
	assertListValue(t, root, "select:readme.txt")
	assertListValue(t, root, "select:current")
	assertListValue(t, root, "manual")

	etc := volumePathItems(idx, "etc", false)
	assertListValue(t, etc, "select:etc")
	assertListValue(t, etc, "up")
	assertListValue(t, etc, "select:etc/app.conf")
	assertListValue(t, etc, "dir:etc/nginx")

	raw := volumePathItems(idx, "", true)
	assertListValue(t, raw, "select:readme.txt")
	assertNoListValue(t, raw, "select:current")
	assertNoListValue(t, raw, "select:")
}

func TestFilterListDialogFiltersAndSelects(t *testing.T) {
	var selected string
	tt := &tui{}
	tt.askFilterListOne("pick", []listItem{
		{text: "alpha.txt", note: "file", value: "select:alpha.txt"},
		{text: "config/", note: "folder", value: "dir:config"},
	}, func(value string, ok bool) {
		if ok {
			selected = value
		}
	})
	tt.dialogKey('c', keyNone)
	if got := tt.dlg.visibleListItems(); len(got) != 1 || got[0].value != "dir:config" {
		t.Fatalf("filtered rows = %+v", got)
	}
	tt.dialogKey(0, keyEnter)
	if selected != "dir:config" {
		t.Fatalf("selected %q", selected)
	}
}

func assertListValue(t *testing.T, items []listItem, want string) {
	t.Helper()
	for _, item := range items {
		if item.value == want {
			return
		}
	}
	t.Fatalf("missing list value %q in %+v", want, items)
}

func assertNoListValue(t *testing.T, items []listItem, unwanted string) {
	t.Helper()
	for _, item := range items {
		if item.value == unwanted {
			t.Fatalf("unexpected list value %q in %+v", unwanted, items)
		}
	}
}
