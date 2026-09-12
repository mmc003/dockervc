package cli

import (
	"bufio"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dockervc/internal/model"
	"dockervc/internal/store"
)

// seedStore creates a temp store with count bare snapshots snap-aaa… and
// points the package storePath at it.
func seedStore(t *testing.T, count int) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("DOCKERVC_HOME", dir)
	storePath = filepath.Join(dir, "store")
	s, err := store.Init(storePath)
	if err != nil {
		t.Fatalf("init store: %v", err)
	}
	for i := 0; i < count; i++ {
		m := &model.Manifest{
			ID:        "snap-" + string(rune('a'+i)) + string(rune('a'+i)) + string(rune('a'+i)),
			CreatedAt: time.Now(),
			Message:   "msg " + string(rune('a'+i)),
		}
		if err := s.InsertSnapshot(m); err != nil {
			t.Fatalf("insert snapshot: %v", err)
		}
	}
	s.Close()
}

func TestAskDeleteSnapshots(t *testing.T) {
	seedStore(t, 3)
	tt := &tui{w: 80, h: 24, out: bufio.NewWriter(io.Discard)}
	tt.refresh()
	if len(tt.snaps) != 3 {
		t.Fatalf("want 3 snapshots, got %d", len(tt.snaps))
	}

	tt.askDeleteSnapshots()
	d := tt.dlg
	if d == nil || !d.multi || len(d.items) != 3 {
		t.Fatalf("expected a 3-row checklist, got %+v", d)
	}
	for i, on := range d.checked {
		if on {
			t.Fatalf("row %d pre-checked — deletion must default to none", i)
		}
	}

	// check two rows, confirm the count dialog, answer y — this runs the
	// real delete command against the temp store.
	tt.dialogKey(' ', keyNone) // row 0
	tt.dialogKey(0, keyDown)
	tt.dialogKey(0, keyDown)
	tt.dialogKey(' ', keyNone) // row 2
	tt.dialogKey(0, keyEnter)
	if tt.dlg == nil || tt.dlg.kind != dlgBool {
		t.Fatalf("expected the count confirmation, got %+v", tt.dlg)
	}
	if want := "delete 2 snapshot(s)?"; tt.dlg.title != want {
		t.Fatalf("title = %q, want %q", tt.dlg.title, want)
	}
	tt.dialogKey('y', keyNone)

	s, err := store.Open(storePath)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	rows, err := s.ListSnapshots()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("want 1 snapshot left, got %d", len(rows))
	}
}

func TestDeleteMultipleArgs(t *testing.T) {
	seedStore(t, 3)
	// a full id, its prefix (resolves to the same snapshot → collapsed) and a
	// second id delete exactly two snapshots
	runArgs("delete", "--yes", "snap-aaa", "snap-a", "snap-ccc")
	s, err := store.Open(storePath)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	rows, _ := s.ListSnapshots()
	if len(rows) != 1 || rows[0].ID != "snap-bbb" {
		t.Fatalf("want only snap-bbb left, got %v", rows)
	}

	// a bad name must fail before anything is deleted
	runArgs("delete", "--yes", "snap-bbb", "snap-nope")
	rows, _ = s.ListSnapshots()
	if len(rows) != 1 {
		t.Fatalf("bad name in batch must delete nothing, got %d left", len(rows))
	}
}

func TestExpandSnapArgsMultiDelete(t *testing.T) {
	tt := &tui{snaps: []store.SnapshotRow{
		{ID: "snap-1111-aaaa", Message: "one"},
		{ID: "snap-2222-bbbb", Message: "two"},
	}}
	out, errMsg := tt.expandSnapArgs([]string{"delete", "1111", "2222"})
	if errMsg != "" {
		t.Fatalf("unexpected error: %s", errMsg)
	}
	want := "delete snap-1111-aaaa snap-2222-bbbb"
	if got := joinArgs(out); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}

	// non-delete commands still expand only their declared slots
	out, errMsg = tt.expandSnapArgs([]string{"show", "1111"})
	if errMsg != "" || len(out) != 2 || out[1] != "snap-1111-aaaa" {
		t.Fatalf("show expansion broke: %v %q", out, errMsg)
	}
}

func joinArgs(argv []string) string {
	got := argv[0]
	for _, a := range argv[1:] {
		got += " " + a
	}
	return got
}

func TestGuidedDeleteMultiPick(t *testing.T) {
	seedStore(t, 3) // rows oldest-first: aaa, bbb, ccc → menu numbers 3, 2, 1
	cases := []struct {
		in   string
		want []string
	}{
		{"1,3\n", []string{"delete", "snap-ccc", "snap-aaa"}},
		{"1 3 3\n", []string{"delete", "snap-ccc", "snap-aaa"}}, // space-sep + dup collapse
		{"snap-a snap-b\n", []string{"delete", "snap-a", "snap-b"}},
		{"2\n", []string{"delete", "snap-bbb"}},
	}
	for _, c := range cases {
		argv := guidedDelete(bufio.NewReader(strings.NewReader(c.in)))
		if len(argv) != len(c.want) {
			t.Fatalf("input %q: argv = %v, want %v", c.in, argv, c.want)
		}
		for i := range c.want {
			if argv[i] != c.want[i] {
				t.Fatalf("input %q: argv = %v, want %v", c.in, argv, c.want)
			}
		}
	}
	// blank input and out-of-range numbers cancel (nil → "cancelled.")
	for _, in := range []string{"\n", "9\n"} {
		if argv := guidedDelete(bufio.NewReader(strings.NewReader(in))); argv != nil {
			t.Fatalf("input %q must cancel, got %v", in, argv)
		}
	}
}
