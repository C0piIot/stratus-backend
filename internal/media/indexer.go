package media

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/C0piIot/stratus-backend/internal/db"
	"github.com/C0piIot/stratus-backend/internal/files"
)

// BatchSize is how many files one pass looks at. Small enough that a first run
// over a large library reports progress and can be interrupted, large enough
// that the query is not the cost.
const BatchSize = 64

// Indexer fills in the metadata of files that have none.
type Indexer struct {
	files   *files.Service
	meta    db.MediaIndex
	tmpDir  string
	ffprobe string
}

// LookupFFprobe finds ffprobe, which is a hard requirement rather than an
// optional extra: without it there is no duration for a track and no dimensions
// for a video, and half a media library is worse than an honest refusal to
// start.
func LookupFFprobe() (string, error) {
	path, err := exec.LookPath("ffprobe")
	if err != nil {
		return "", fmt.Errorf("ffprobe is required for media indexing: %w", err)
	}
	return path, nil
}

// NewIndexer wires an indexer. tmpDir is where blobs are spooled for ffprobe,
// and ffprobe is its path, from LookupFFprobe.
func NewIndexer(f *files.Service, meta db.MediaIndex, tmpDir, ffprobe string) *Indexer {
	return &Indexer{files: f, meta: meta, tmpDir: tmpDir, ffprobe: ffprobe}
}

// IndexBatch extracts metadata for up to BatchSize files and returns how many
// it wrote. A full batch means there is probably more to do.
func (i *Indexer) IndexBatch(ctx context.Context) (int, error) {
	pending, err := i.meta.PendingMedia(ctx, Version, BatchSize)
	if err != nil {
		return 0, fmt.Errorf("find files to index: %w", err)
	}

	for n, f := range pending {
		if err := ctx.Err(); err != nil {
			return n, err
		}
		if err := i.meta.PutMedia(ctx, i.index(ctx, f)); err != nil {
			return n, fmt.Errorf("store the metadata of %q: %w", f.Path, err)
		}
	}
	return len(pending), nil
}

// index never fails: a file it cannot read gets a row saying why, because the
// alternative is reading it again on every pass for the rest of time.
func (i *Indexer) index(ctx context.Context, f db.File) db.Media {
	m, err := i.extract(ctx, f)
	m.FileID = f.ID
	m.IndexedAt = time.Now()
	m.Version = Version
	if err != nil {
		m.Kind = kindOf(f)
		m.Error = err.Error()
	}
	return m
}

func (i *Indexer) extract(ctx context.Context, f db.File) (db.Media, error) {
	kind := kindOf(f)
	switch kind {
	case db.KindImage:
		body, err := i.files.OpenFile(ctx, f)
		if err != nil {
			return db.Media{}, err
		}
		defer func() { _ = body.Close() }()
		return extractImage(body)

	case db.KindAudio, db.KindVideo:
		path, cleanup, err := i.spool(ctx, f)
		if err != nil {
			return db.Media{}, err
		}
		defer cleanup()

		report, err := runProbe(ctx, i.ffprobe, path)
		if err != nil {
			return db.Media{}, err
		}
		return report.mediaFrom(kind), nil

	default:
		return db.Media{Kind: db.KindOther}, nil
	}
}

// spool copies a blob to a local file, because ffprobe seeks and the storage
// port streams.
//
// For a large video on S3 this downloads the whole thing once. That is the
// honest cost of the abstraction, and the obvious fix -- handing ffprobe a path
// when the backend already has one -- is a change to the port rather than a
// change here.
func (i *Indexer) spool(ctx context.Context, f db.File) (string, func(), error) {
	body, err := i.files.OpenFile(ctx, f)
	if err != nil {
		return "", nil, err
	}
	defer func() { _ = body.Close() }()

	tmp, err := os.CreateTemp(i.tmpDir, "probe-*")
	if err != nil {
		return "", nil, fmt.Errorf("spool %q: %w", f.Path, err)
	}
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
	}

	if _, err := io.Copy(tmp, body); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("spool %q: %w", f.Path, err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("spool %q: %w", f.Path, err)
	}
	return tmp.Name(), func() { _ = os.Remove(tmp.Name()) }, nil
}

// TempDir is where spooled blobs go, under the data directory so that a
// multi-gigabyte video does not land on whatever /tmp happens to be.
func TempDir(dataDir string) (string, error) {
	dir := filepath.Join(dataDir, ".index")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", fmt.Errorf("create %s: %w", dir, err)
	}
	// Anything still in there is from a process that is no longer running, the
	// same argument the blob store makes about its own reserved directory.
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", dir, err)
	}
	for _, e := range entries {
		if err := os.Remove(filepath.Join(dir, e.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("remove %s: %w", e.Name(), err)
		}
	}
	return dir, nil
}
