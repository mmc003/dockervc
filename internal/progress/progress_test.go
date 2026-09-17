package progress

import (
	"bytes"
	"io"
	"testing"
	"time"
)

func TestMeterLifecycle(t *testing.T) {
	now := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	var events []Event
	m := NewMeter(ReporterFunc(func(e Event) { events = append(events, e) }), "export", func() time.Time { return now })
	m.Phase("writing", "object-a", 100, TotalExact, 2)
	now = now.Add(time.Second)
	m.AddBytes(40)
	m.Item("object-b", 1, 2)
	now = now.Add(time.Second)
	m.SetBytes(100, 100, TotalExact)
	m.Finish(false)

	if len(events) != 6 {
		t.Fatalf("events = %d, want 6: %+v", len(events), events)
	}
	last := events[len(events)-1]
	if !last.Finished || last.Failed || last.BytesDone != 100 || last.ItemsDone != 1 {
		t.Fatalf("last event = %+v", last)
	}
	if got := last.At.Sub(last.StartedAt); got != 2*time.Second {
		t.Fatalf("elapsed = %s", got)
	}
}

func TestCountingReader(t *testing.T) {
	var counted int64
	r := &CountingReader{
		Reader:  bytes.NewBufferString("payload"),
		Advance: func(n int64) { counted += n },
	}
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "payload" || counted != int64(len(b)) {
		t.Fatalf("data=%q counted=%d", b, counted)
	}
}

func TestNilReporterIsSafe(t *testing.T) {
	m := NewMeter(nil, "noop")
	m.Phase("work", "", 10, TotalEstimated, 1)
	m.AddBytes(10)
	m.Finish(false)
}
