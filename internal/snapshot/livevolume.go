package snapshot

import (
	"context"
	"fmt"
	"io"

	"dockervc/internal/model"
	"dockervc/internal/progress"
)

// VolumeTarOpener starts a read-only tar stream for one live Docker volume.
// dockerapi.Client.VolumeTarStream satisfies this shape.
type VolumeTarOpener func(context.Context, string) (io.ReadCloser, error)

// IndexLiveVolume reads and hashes a live volume without persisting its tar or
// index. The VolumeIndexer tee ensures the source is consumed through EOF so a
// helper-stream failure is observed even though archive/tar itself may stop at
// the tar end marker before reading the underlying stream's final error.
func IndexLiveVolume(ctx context.Context, open VolumeTarOpener, name string) (*FileIndex, error) {
	return IndexLiveVolumeTracked(ctx, open, name, nil)
}

// IndexLiveVolumeTracked is IndexLiveVolume with a callback for raw tar bytes
// consumed from the helper stream.
func IndexLiveVolumeTracked(ctx context.Context, open VolumeTarOpener, name string,
	advance func(int64)) (*FileIndex, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	stream, err := open(ctx, name)
	if err != nil {
		return nil, err
	}

	indexer := NewVolumeIndexer(stream)
	counted := &progress.CountingReader{Reader: indexer.Reader(), Advance: advance}
	_, copyErr := io.Copy(io.Discard, counted)
	closeErr := stream.Close()
	idx, indexErr := indexer.Finish()
	if copyErr != nil {
		return nil, fmt.Errorf("read live volume %s: %w", name, copyErr)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("close live volume %s stream: %w", name, closeErr)
	}
	if indexErr != nil {
		return nil, fmt.Errorf("index live volume %s: %w", name, indexErr)
	}
	return idx, nil
}

// DiffLiveVolume compares a snapshot's stored file index with the current
// contents of the same Docker volume. A nil change and nil indexes means the
// snapshot predates file indexing and cannot be compared at file level.
func DiffLiveVolume(ctx context.Context, openTar VolumeTarOpener, openBlob BlobOpener,
	rec model.VolumeRecord) (change *FileChange, snapshotIndex, liveIndex *FileIndex, err error) {
	return DiffLiveVolumeTracked(ctx, openTar, openBlob, rec, nil)
}

// DiffLiveVolumeTracked is DiffLiveVolume with live progress reporting.
func DiffLiveVolumeTracked(ctx context.Context, openTar VolumeTarOpener, openBlob BlobOpener,
	rec model.VolumeRecord, reporter progress.Reporter) (
	change *FileChange, snapshotIndex, liveIndex *FileIndex, retErr error) {

	snapshotIndex, retErr = LoadFileIndex(openBlob, rec)
	if retErr != nil || snapshotIndex == nil {
		return nil, snapshotIndex, nil, retErr
	}
	meter := progress.NewMeter(reporter, "deep status")
	estimate := snapshotIndex.EstimatedTarBytes()
	kind := progress.TotalUnknown
	if estimate > 0 {
		kind = progress.TotalEstimated
	}
	meter.Phase("scanning volume", rec.Name, estimate, kind, 1)
	defer func() { meter.Finish(retErr != nil) }()
	liveIndex, retErr = IndexLiveVolumeTracked(ctx, openTar, rec.Name, meter.AddBytes)
	if retErr != nil {
		return nil, snapshotIndex, nil, retErr
	}
	meter.Item(rec.Name, 1, 1)
	return DiffIndexes(rec.Name, snapshotIndex, liveIndex), snapshotIndex, liveIndex, nil
}
