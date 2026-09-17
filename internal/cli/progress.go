package cli

import (
	"fmt"
	"io"
	"math"
	"os"
	"strings"
	"time"

	"dockervc/internal/progress"
	"dockervc/internal/snapshot"
)

type progressRenderer struct {
	out io.Writer
	tty bool

	phaseKey   string
	lastAt     time.Time
	lastBytes  int64
	sampleAt   time.Time
	sampleByte int64
	rate       float64
	lastPrint  time.Time
	activeLine bool
}

func newProgressRenderer(out io.Writer, tty bool) progress.Reporter {
	return &progressRenderer{out: out, tty: tty}
}

func commandProgress(out io.Writer) progress.Reporter {
	if noProgress {
		return progress.Nop()
	}
	tty := false
	if f, ok := out.(*os.File); ok {
		if fi, err := f.Stat(); err == nil {
			tty = fi.Mode()&os.ModeCharDevice != 0
		}
	}
	return newProgressRenderer(out, tty)
}

func (r *progressRenderer) Report(e progress.Event) {
	if r.out == nil {
		return
	}
	if e.Phase == "" && !e.Finished {
		return
	}
	key := e.Operation + "\x00" + e.Phase + "\x00" + e.PhaseStartedAt.String()
	phaseChanged := key != r.phaseKey
	if phaseChanged {
		if r.tty && r.activeLine {
			fmt.Fprintln(r.out)
		}
		r.phaseKey = key
		r.lastAt = e.At
		r.lastBytes = e.BytesDone
		r.sampleAt = e.At
		r.sampleByte = e.BytesDone
		r.rate = 0
		r.activeLine = false
	}
	r.updateRate(e)

	shouldPrint := phaseChanged || e.Finished
	if r.tty {
		shouldPrint = shouldPrint || r.lastPrint.IsZero() || e.At.Sub(r.lastPrint) >= 250*time.Millisecond
	} else {
		shouldPrint = shouldPrint || r.lastPrint.IsZero() || e.At.Sub(r.lastPrint) >= 10*time.Second
	}
	if !shouldPrint {
		return
	}

	line := r.format(e)
	if r.tty {
		fmt.Fprintf(r.out, "\r\x1b[2K%s", line)
		r.activeLine = true
		if e.Finished {
			fmt.Fprintln(r.out)
			r.activeLine = false
		}
	} else {
		fmt.Fprintln(r.out, line)
	}
	r.lastPrint = e.At
}

func (r *progressRenderer) updateRate(e progress.Event) {
	if e.BytesDone < r.lastBytes {
		r.sampleAt, r.sampleByte, r.rate = e.At, e.BytesDone, 0
	}
	r.lastAt, r.lastBytes = e.At, e.BytesDone
	dt := e.At.Sub(r.sampleAt)
	if dt < 250*time.Millisecond || e.BytesDone <= r.sampleByte {
		return
	}
	instant := float64(e.BytesDone-r.sampleByte) / dt.Seconds()
	if r.rate == 0 {
		r.rate = instant
	} else {
		const alpha = 0.25
		r.rate = alpha*instant + (1-alpha)*r.rate
	}
	r.sampleAt, r.sampleByte = e.At, e.BytesDone
}

func (r *progressRenderer) format(e progress.Event) string {
	label := strings.TrimSpace(strings.Join([]string{e.Phase, e.Item}, " "))
	if label == "" {
		label = e.Operation
	}
	var parts []string
	if e.ItemsTotal > 0 {
		parts = append(parts, fmt.Sprintf("%d/%d", e.ItemsDone, e.ItemsTotal))
	}
	if e.BytesDone > 0 || e.BytesTotal > 0 {
		bytes := snapshot.HumanBytes(e.BytesDone)
		if e.BytesTotal > 0 {
			bytes += " / " + snapshot.HumanBytes(e.BytesTotal)
		}
		parts = append(parts, bytes)
	}
	if e.BytesTotal > 0 {
		pct := math.Min(100, float64(e.BytesDone)*100/float64(e.BytesTotal))
		parts = append(parts, fmt.Sprintf("%.0f%%", pct))
	}
	if r.rate > 0 {
		parts = append(parts, snapshot.HumanBytes(int64(r.rate))+"/s")
	}
	elapsed := e.At.Sub(e.StartedAt)
	if elapsed < 0 {
		elapsed = 0
	}
	parts = append(parts, "elapsed "+formatProgressDuration(elapsed))
	if e.BytesTotal > e.BytesDone && e.TotalKind != progress.TotalUnknown {
		if e.At.Sub(e.PhaseStartedAt) < 2*time.Second || r.rate <= 0 {
			parts = append(parts, "ETA calculating…")
		} else {
			eta := time.Duration(float64(e.BytesTotal-e.BytesDone)/r.rate) * time.Second
			prefix := "ETA "
			if e.TotalKind == progress.TotalEstimated {
				prefix = "~ETA "
			}
			parts = append(parts, prefix+formatProgressDuration(eta))
		}
	}
	if e.Finished {
		if e.Failed {
			parts = append(parts, "failed")
		} else {
			parts = append(parts, "done")
		}
	}
	return label + ": " + strings.Join(parts, " · ")
}

func formatProgressDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	d = d.Round(time.Second)
	h := int(d / time.Hour)
	m := int(d/time.Minute) % 60
	s := int(d/time.Second) % 60
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, m, s)
	}
	return fmt.Sprintf("%02d:%02d", m, s)
}
