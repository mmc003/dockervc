package snapshot

import (
	"testing"

	"dockervc/internal/model"
	"dockervc/internal/progress"
)

func TestCaptureEstimateUsesLatestEntitySizes(t *testing.T) {
	c := &Capturer{baseline: &model.Manifest{
		Containers: []model.ContainerRecord{{Name: "web", Size: 11}},
		Volumes:    []model.VolumeRecord{{Name: "data", Size: 22}},
		Images:     []model.ImageRecord{{Digest: "sha256:image", Size: 33}},
		BindMounts: []model.BindMountRecord{{HostPath: "/host", Size: 44}},
	}}
	for _, tc := range []struct {
		kind, name string
		want       int64
	}{
		{"container", "web", 11},
		{"volume", "data", 22},
		{"image", "sha256:image", 33},
		{"bindmount", "/host", 44},
		{"volume", "new", 0},
	} {
		if got := c.estimateSize(tc.kind, tc.name); got != tc.want {
			t.Fatalf("estimateSize(%q, %q) = %d, want %d", tc.kind, tc.name, got, tc.want)
		}
	}
}

func TestCaptureProgressMarksBaselineAsEstimated(t *testing.T) {
	var events []progress.Event
	c := &Capturer{
		meter: progress.NewMeter(progress.ReporterFunc(func(event progress.Event) {
			events = append(events, event)
		}), "snapshot"),
	}
	c.progressEntity("snapshotting volume", "data", 1024, 0, 1)
	c.meter.AddBytes(512)
	c.finishProgressEntity("data", 1, 1, 768)

	var estimated, completed bool
	for _, event := range events {
		if event.Phase == "snapshotting volume" && event.TotalKind == progress.TotalEstimated && event.BytesTotal == 1024 {
			estimated = true
		}
		if event.Phase == "snapshotting volume" && event.TotalKind == progress.TotalExact &&
			event.BytesDone == 768 && event.BytesTotal == 768 {
			completed = true
		}
	}
	if !estimated || !completed {
		t.Fatalf("events = %+v", events)
	}
}
