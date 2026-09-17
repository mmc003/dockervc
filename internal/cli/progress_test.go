package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"dockervc/internal/progress"
)

func progressEvent(kind progress.TotalKind, at time.Time, done, total int64) progress.Event {
	start := at.Add(-11 * time.Second)
	return progress.Event{
		Operation:      "export",
		Phase:          "writing",
		Item:           "object 1/2",
		ItemsDone:      1,
		ItemsTotal:     2,
		BytesDone:      done,
		BytesTotal:     total,
		TotalKind:      kind,
		StartedAt:      start,
		PhaseStartedAt: start,
		At:             at,
	}
}

func TestProgressRendererExactAndEstimatedETA(t *testing.T) {
	for _, tc := range []struct {
		name string
		kind progress.TotalKind
		want string
	}{
		{"exact", progress.TotalExact, " · ETA "},
		{"estimated", progress.TotalEstimated, " · ~ETA "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			r := newProgressRenderer(&out, false)
			t0 := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
			first := progressEvent(tc.kind, t0, 0, 1000)
			second := progressEvent(tc.kind, t0.Add(11*time.Second), 500, 1000)
			second.StartedAt = first.StartedAt
			second.PhaseStartedAt = first.PhaseStartedAt
			r.Report(first)
			r.Report(second)
			got := out.String()
			if !strings.Contains(got, tc.want) {
				t.Fatalf("output missing %q:\n%s", tc.want, got)
			}
			if strings.Contains(got, "\x1b[") || strings.Contains(got, "\r") {
				t.Fatalf("redirected output contains terminal controls: %q", got)
			}
		})
	}
}

func TestProgressRendererUnknownTotalOmitsETA(t *testing.T) {
	var out bytes.Buffer
	r := newProgressRenderer(&out, false)
	t0 := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	first := progressEvent(progress.TotalUnknown, t0, 0, 0)
	second := progressEvent(progress.TotalUnknown, t0.Add(11*time.Second), 500, 0)
	second.StartedAt = first.StartedAt
	second.PhaseStartedAt = first.PhaseStartedAt
	r.Report(first)
	r.Report(second)
	if strings.Contains(out.String(), "ETA") {
		t.Fatalf("unknown total displayed ETA: %s", out.String())
	}
}

func TestTTYProgressTerminatesFinishedLine(t *testing.T) {
	var out bytes.Buffer
	r := newProgressRenderer(&out, true)
	t0 := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	e := progressEvent(progress.TotalExact, t0, 1000, 1000)
	e.Finished = true
	r.Report(e)
	got := out.String()
	if !strings.Contains(got, "\r\x1b[2K") || !strings.HasSuffix(got, "\n") || !strings.Contains(got, "done") {
		t.Fatalf("TTY completion = %q", got)
	}
}

func TestFormatProgressDuration(t *testing.T) {
	for _, tc := range []struct {
		in   time.Duration
		want string
	}{
		{0, "00:00"},
		{65 * time.Second, "01:05"},
		{time.Hour + 2*time.Minute + 3*time.Second, "1:02:03"},
	} {
		if got := formatProgressDuration(tc.in); got != tc.want {
			t.Fatalf("formatProgressDuration(%s) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestCommandProgressCanBeDisabled(t *testing.T) {
	old := noProgress
	noProgress = true
	t.Cleanup(func() { noProgress = old })
	var out bytes.Buffer
	meter := progress.NewMeter(commandProgress(&out), "test")
	meter.Phase("work", "item", 10, progress.TotalExact, 1)
	meter.AddBytes(10)
	meter.Finish(false)
	if out.Len() != 0 {
		t.Fatalf("--no-progress reporter wrote %q", out.String())
	}
}
