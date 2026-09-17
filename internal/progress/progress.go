// Package progress defines operation-neutral progress events and a small
// meter. Core packages emit events; frontends decide how to render them.
package progress

import (
	"io"
	"sync"
	"time"
)

// TotalKind describes how trustworthy BytesTotal is.
type TotalKind uint8

const (
	TotalUnknown TotalKind = iota
	TotalEstimated
	TotalExact
)

// Event is one immutable progress observation.
type Event struct {
	Operation string
	Phase     string
	Item      string

	ItemsDone  int
	ItemsTotal int
	BytesDone  int64
	BytesTotal int64
	TotalKind  TotalKind

	StartedAt      time.Time
	PhaseStartedAt time.Time
	At             time.Time
	Finished       bool
	Failed         bool
}

// Reporter consumes progress events. Implementations must not return errors:
// display failures must never fail the underlying backup or restore.
type Reporter interface {
	Report(Event)
}

// ReporterFunc adapts a function to Reporter.
type ReporterFunc func(Event)

func (f ReporterFunc) Report(e Event) {
	if f != nil {
		f(e)
	}
}

type nopReporter struct{}

func (nopReporter) Report(Event) {}

// Nop returns a reporter that discards every event.
func Nop() Reporter { return nopReporter{} }

// Meter owns the mutable counters for one operation. Its methods are safe to
// call from multiple goroutines, though current dockervc streams are serial.
type Meter struct {
	mu       sync.Mutex
	reporter Reporter
	now      func() time.Time
	event    Event
}

// NewMeter starts an operation and immediately emits its initial event.
// A nil reporter is treated as Nop. The optional clock exists for tests.
func NewMeter(reporter Reporter, operation string, clock ...func() time.Time) *Meter {
	if reporter == nil {
		reporter = Nop()
	}
	now := time.Now
	if len(clock) > 0 && clock[0] != nil {
		now = clock[0]
	}
	t := now()
	m := &Meter{
		reporter: reporter,
		now:      now,
		event: Event{
			Operation:      operation,
			StartedAt:      t,
			PhaseStartedAt: t,
			At:             t,
		},
	}
	m.reporter.Report(m.event)
	return m
}

// Phase starts a new phase and resets phase-local byte counters.
func (m *Meter) Phase(name, item string, bytesTotal int64, kind TotalKind, itemsTotal int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t := m.now()
	m.event.Phase = name
	m.event.Item = item
	m.event.ItemsDone = 0
	m.event.ItemsTotal = itemsTotal
	m.event.BytesDone = 0
	m.event.BytesTotal = bytesTotal
	m.event.TotalKind = kind
	m.event.PhaseStartedAt = t
	m.event.At = t
	m.event.Finished = false
	m.event.Failed = false
	m.reporter.Report(m.event)
}

// Item changes the current item and item counts without resetting the phase.
func (m *Meter) Item(name string, done, total int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.event.Item = name
	m.event.ItemsDone = done
	m.event.ItemsTotal = total
	m.emitLocked()
}

// AddBytes advances the phase-local byte counter.
func (m *Meter) AddBytes(n int64) {
	if n <= 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.event.BytesDone += n
	m.emitLocked()
}

// SetBytes replaces byte progress and optionally updates the total.
func (m *Meter) SetBytes(done, total int64, kind TotalKind) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.event.BytesDone = done
	m.event.BytesTotal = total
	m.event.TotalKind = kind
	m.emitLocked()
}

// Finish emits the terminal event for the operation.
func (m *Meter) Finish(failed bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.event.Finished = true
	m.event.Failed = failed
	m.emitLocked()
}

func (m *Meter) emitLocked() {
	m.event.At = m.now()
	m.reporter.Report(m.event)
}

// CountingReader reports bytes after successful reads.
type CountingReader struct {
	Reader  io.Reader
	Advance func(int64)
}

func (r *CountingReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	if n > 0 && r.Advance != nil {
		r.Advance(int64(n))
	}
	return n, err
}

// CountingWriter reports bytes after successful writes.
type CountingWriter struct {
	Writer  io.Writer
	Advance func(int64)
}

func (w *CountingWriter) Write(p []byte) (int, error) {
	n, err := w.Writer.Write(p)
	if n > 0 && w.Advance != nil {
		w.Advance(int64(n))
	}
	return n, err
}
