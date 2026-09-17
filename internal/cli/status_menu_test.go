package cli

import (
	"strings"
	"testing"
)

func TestResolveStatusVolumeSelection(t *testing.T) {
	items := []listItem{
		{text: "alpha", value: "alpha"},
		{text: "beta", value: "beta"},
		{text: "2", value: "2"},
	}
	names, err := resolveStatusVolumeSelection(items, "2, alpha, 2")
	if err != nil {
		t.Fatal(err)
	}
	// Exact names win over numeric row selection; duplicates are collapsed.
	if !equalStrings(names, []string{"2", "alpha"}) {
		t.Fatalf("selection = %v, want [2 alpha]", names)
	}
	if _, err := resolveStatusVolumeSelection(items, "missing"); err == nil {
		t.Fatal("unknown selection should fail")
	}
}

func TestStatusDeepArgs(t *testing.T) {
	if got := statusDeepArgs(nil); !equalStrings(got, []string{"status", "--deep"}) {
		t.Fatalf("all-volume args = %v", got)
	}
	if got := statusDeepArgs([]string{"alpha", "beta"}); !equalStrings(got,
		[]string{"status", "--deep", "--volumes", "alpha,beta"}) {
		t.Fatalf("selected-volume args = %v", got)
	}
}

func TestStatusMenuAdvertisesVolumeSelection(t *testing.T) {
	action := menuActions[0]
	if action.name != "status" || action.guided == nil || !strings.Contains(action.args, "--volumes") {
		t.Fatalf("status menu action does not expose volume selection: %+v", action)
	}
}
