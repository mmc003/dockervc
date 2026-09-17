package snapshot

import (
	"context"
	"fmt"
	"io"

	"dockervc/internal/model"
)

// VolumeTarOpener starts a read-only tar stream for one live Docker volume.
// dockerapi.Client.VolumeTarStream satisfies this shape.
type VolumeTarOpener func(context.Context, string) (io.ReadCloser, error)

// IndexLiveVolume reads and hashes a live volume without persisting its tar or
// index. The VolumeIndexer tee ensures the source is consumed through EOF so a
// helper-stream failure is observed even though archive/tar itself may stop at
// the tar end marker before reading the underlying stream's final error.
func IndexLiveVolume(ctx context.Context, open VolumeTarOpener, name string) (*FileIndex, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	stream, err := open(ctx, name)
	if err != nil {
		return nil, err
	}

	indexer := NewVolumeIndexer(stream)
	_, copyErr := io.Copy(io.Discard, indexer.Reader())
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

	snapshotIndex, err = LoadFileIndex(openBlob, rec)
	if err != nil || snapshotIndex == nil {
		return nil, snapshotIndex, nil, err
	}
	liveIndex, err = IndexLiveVolume(ctx, openTar, rec.Name)
	if err != nil {
		return nil, snapshotIndex, nil, err
	}
	return DiffIndexes(rec.Name, snapshotIndex, liveIndex), snapshotIndex, liveIndex, nil
}
