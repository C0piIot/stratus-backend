// Package sqlite implements the metadata port on SQLite, through the pure-Go
// modernc.org/sqlite driver so the binary stays CGO-free and the image stays
// distroless.
package sqlite

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"iter"
	"net/url"
	"os"
	"path/filepath"
	"time"

	sqlitedriver "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

	"github.com/C0piIot/stratus-backend/internal/db"
	"github.com/C0piIot/stratus-backend/internal/db/sqlutil"
)

//go:embed migrations/*.sql
var migrations embed.FS

// pragmas are set by this package rather than taken from the DSN, because none
// of them is an operator preference:
//
//   - WAL, so a reader does not block the writer. Without it a thumbnail scan
//     stalls an upload.
//   - foreign_keys, which SQLite leaves off for backwards compatibility and
//     which every schema here assumes.
//   - busy_timeout, so a concurrent writer waits instead of failing instantly.
//   - synchronous=NORMAL, the pairing WAL is designed for.
var pragmas = []string{
	"journal_mode(WAL)",
	"foreign_keys(on)",
	"busy_timeout(5000)",
	"synchronous(normal)",
}

// Store is a db.Store backed by a SQLite file.
type Store struct {
	*repo
	db *sql.DB
}

var _ db.Store = (*Store)(nil)

// New opens the database at path, creating the file if it is not there.
func New(ctx context.Context, path string) (*Store, error) {
	if path == "" {
		return nil, errors.New("sqlite: path is required")
	}
	// Same courtesy as the disk blob backend: create the directory rather than
	// fail because it is one level deeper than the data dir.
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, fmt.Errorf("sqlite: create %s: %w", filepath.Dir(path), err)
	}

	// Built through url.URL so a path with a space or a question mark in it
	// still produces a DSN the driver can parse.
	dsn := url.URL{Scheme: "file", Path: path}
	query := url.Values{}
	for _, p := range pragmas {
		query.Add("_pragma", p)
	}
	dsn.RawQuery = query.Encode()

	sqlDB, err := sql.Open("sqlite", dsn.String())
	if err != nil {
		return nil, fmt.Errorf("sqlite: open %s: %w", path, err)
	}
	if err := sqlDB.PingContext(ctx); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("sqlite: open %s: %w", path, err)
	}
	return &Store{repo: &repo{q: sqlDB}, db: sqlDB}, nil
}

// Migrate implements db.Store.
func (s *Store) Migrate(ctx context.Context) error { return db.Migrate(ctx, s.db, migrations) }

// Ping implements db.Store.
func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

// Close implements db.Store.
func (s *Store) Close() error { return s.db.Close() }

// Tx implements db.Store.
func (s *Store) Tx(ctx context.Context, fn func(db.Repo) error) error {
	return sqlutil.InTx(ctx, s.db, func(q sqlutil.Querier) error { return fn(&repo{q: q}) })
}

type repo struct{ q sqlutil.Querier }

// isDirProbe is what sqlutil.CheckAffected asks when a statement changed
// nothing: the placeholders are why it cannot live in that package.
const isDirProbe = `SELECT is_dir FROM files WHERE owner_id = ? AND path = ?`

const fileColumns = `id, owner_id, path, blob_key, size, mtime, etag, mime_type, is_dir`

// PutFile implements db.Repo.
func (r *repo) PutFile(ctx context.Context, f db.File) (db.File, error) {
	if err := db.ValidatePath(f.Path); err != nil {
		return db.File{}, err
	}
	f = f.Normalize()

	const query = `INSERT INTO files (owner_id, path, parent_path, blob_key, size, mtime, etag, mime_type)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (owner_id, path) DO UPDATE SET
			blob_key = excluded.blob_key, size = excluded.size, mtime = excluded.mtime,
			etag = excluded.etag, mime_type = excluded.mime_type
		WHERE files.is_dir = 0
		RETURNING id`

	err := r.q.QueryRowContext(ctx, query,
		f.OwnerID, f.Path, db.ParentOf(f.Path), f.BlobKey, f.Size,
		f.MTime.UnixMilli(), f.ETag, f.MIMEType,
	).Scan(&f.ID)
	if errors.Is(err, sql.ErrNoRows) {
		// The upsert declined to update, which the WHERE above only does for a
		// directory: a file must not replace a collection.
		return db.File{}, fmt.Errorf("put %q: %w: it is a directory", f.Path, db.ErrConflict)
	}
	if err != nil {
		return db.File{}, fmt.Errorf("put %q: %w", f.Path, mapErr(err))
	}
	return f, nil
}

// CreateDir implements db.Repo.
func (r *repo) CreateDir(ctx context.Context, owner, path string) (db.File, error) {
	if err := db.ValidatePath(path); err != nil {
		return db.File{}, err
	}

	const query = `INSERT INTO files (owner_id, path, parent_path, blob_key, size, mtime, etag, mime_type, is_dir)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, 1)
		RETURNING id`

	dir := db.File{OwnerID: owner, Path: path, MTime: time.Now(), IsDir: true}.Normalize()
	err := r.q.QueryRowContext(ctx, query,
		dir.OwnerID, dir.Path, db.ParentOf(dir.Path), "", 0,
		dir.MTime.UnixMilli(), "", "",
	).Scan(&dir.ID)
	if err != nil {
		return db.File{}, fmt.Errorf("create directory %q: %w", path, mapErr(err))
	}
	return dir, nil
}

// FileByPath implements db.Repo.
func (r *repo) FileByPath(ctx context.Context, owner, path string) (db.File, error) {
	if err := db.ValidatePath(path); err != nil {
		return db.File{}, err
	}
	const query = `SELECT ` + fileColumns + ` FROM files WHERE owner_id = ? AND path = ?`

	f, err := scanFile(r.q.QueryRowContext(ctx, query, owner, path))
	if err != nil {
		return db.File{}, fmt.Errorf("get %q: %w", path, err)
	}
	return f, nil
}

// ListFiles implements db.Repo.
func (r *repo) ListFiles(ctx context.Context, owner, dir string) ([]db.File, error) {
	if err := db.ValidateDir(dir); err != nil {
		return nil, err
	}
	const query = `SELECT ` + fileColumns + ` FROM files
		WHERE owner_id = ? AND parent_path = ? ORDER BY path`

	out, err := sqlutil.Collect(ctx, r.q, scanFileRow, query, owner, dir)
	if err != nil {
		return nil, fmt.Errorf("list %q: %w", dir, mapErr(err))
	}
	return out, nil
}

const mediaColumns = `file_id, kind, indexed_at, version, error, taken_at, width, height, orientation, latitude, longitude, camera, duration_ms, codec, artist, album, title, track_no, disc_no, year, genre`

// PutMedia implements db.MediaIndex.
func (r *repo) PutMedia(ctx context.Context, m db.Media) error {
	var takenAt any
	if !m.TakenAt.IsZero() {
		takenAt = m.TakenAt.UTC().Truncate(db.TimePrecision).UnixMilli()
	}

	var lat, lon any
	if m.GPS != nil {
		lat, lon = m.GPS.Latitude, m.GPS.Longitude
	}

	const query = `INSERT INTO media (` + mediaColumns + `)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (file_id) DO UPDATE SET
			kind = excluded.kind, indexed_at = excluded.indexed_at, version = excluded.version,
			error = excluded.error, taken_at = excluded.taken_at, width = excluded.width,
			height = excluded.height, orientation = excluded.orientation,
			latitude = excluded.latitude, longitude = excluded.longitude, camera = excluded.camera,
			duration_ms = excluded.duration_ms, codec = excluded.codec, artist = excluded.artist,
			album = excluded.album, title = excluded.title, track_no = excluded.track_no,
			disc_no = excluded.disc_no, year = excluded.year, genre = excluded.genre`

	_, err := r.q.ExecContext(ctx, query,
		m.FileID, string(m.Kind), m.IndexedAt.UTC().UnixMilli(), m.Version, m.Error, takenAt,
		m.Width, m.Height, m.Orientation, lat, lon, m.Camera,
		m.DurationMS, m.Codec, m.Artist, m.Album, m.Title,
		m.TrackNo, m.DiscNo, m.Year, m.Genre,
	)
	if err != nil {
		return fmt.Errorf("put media for file %d: %w", m.FileID, mapErr(err))
	}
	return nil
}

// MediaByFile implements db.MediaIndex.
func (r *repo) MediaByFile(ctx context.Context, fileID int64) (db.Media, error) {
	const query = `SELECT ` + mediaColumns + ` FROM media WHERE file_id = ?`

	m, err := scanMedia(r.q.QueryRowContext(ctx, query, fileID))
	if err != nil {
		return db.Media{}, fmt.Errorf("get media for file %d: %w", fileID, err)
	}
	return m, nil
}

// PendingMedia implements db.MediaIndex.
func (r *repo) PendingMedia(ctx context.Context, version, limit int) ([]db.File, error) {
	// The queue is this LEFT JOIN. A row with an error counts as done, or a
	// file nothing can parse would come back on every pass forever.
	const query = `SELECT f.` + `id, f.owner_id, f.path, f.blob_key, f.size, f.mtime, f.etag, f.mime_type, f.is_dir
		FROM files f LEFT JOIN media m ON m.file_id = f.id
		WHERE f.is_dir = 0 AND (m.file_id IS NULL OR m.version < ?)
		ORDER BY f.id LIMIT ?`

	out, err := sqlutil.Collect(ctx, r.q, scanFileRow, query, version, limit)
	if err != nil {
		return nil, fmt.Errorf("list pending media: %w", mapErr(err))
	}
	return out, nil
}

func scanMedia(row *sql.Row) (db.Media, error) {
	var m db.Media
	var kind string
	var lat, lon sql.NullFloat64
	var indexedAt int64
	var takenAt sql.NullInt64

	err := row.Scan(&m.FileID, &kind, &indexedAt, &m.Version, &m.Error, &takenAt,
		&m.Width, &m.Height, &m.Orientation, &lat, &lon, &m.Camera,
		&m.DurationMS, &m.Codec, &m.Artist, &m.Album, &m.Title,
		&m.TrackNo, &m.DiscNo, &m.Year, &m.Genre)
	if err != nil {
		return db.Media{}, mapErr(err)
	}

	m.Kind = db.Kind(kind)
	m.IndexedAt = time.UnixMilli(indexedAt).UTC()
	if takenAt.Valid {
		m.TakenAt = time.UnixMilli(takenAt.Int64).UTC()
	}
	if lat.Valid && lon.Valid {
		m.GPS = &db.GPS{Latitude: lat.Float64, Longitude: lon.Float64}
	}
	return m, nil
}

// BlobKeys implements db.Repo.
func (r *repo) BlobKeys(ctx context.Context) iter.Seq2[string, error] {
	const query = `SELECT blob_key FROM files WHERE is_dir = 0`

	return sqlutil.Label(sqlutil.Seq(ctx, r.q, sqlutil.ScanOne[string], query), "list blob keys")
}

// MoveFile implements db.Repo.
func (r *repo) MoveFile(ctx context.Context, owner, from, to string) error {
	if err := db.ValidatePath(from); err != nil {
		return err
	}
	if err := db.ValidatePath(to); err != nil {
		return err
	}
	// The NOT EXISTS is what refuses to move a directory out from under its
	// contents: rewriting a whole subtree is a different operation, and this
	// one must not half-do it.
	const query = `UPDATE files SET path = ?, parent_path = ?
		WHERE owner_id = ? AND path = ?
		  AND NOT EXISTS (SELECT 1 FROM files child WHERE child.owner_id = ? AND child.parent_path = ?)`

	result, err := r.q.ExecContext(ctx, query, to, db.ParentOf(to), owner, from, owner, from)
	if err != nil {
		return fmt.Errorf("move %q to %q: %w", from, to, mapErr(err))
	}
	return sqlutil.CheckAffected(ctx, r.q, result, from, isDirProbe, owner, from)
}

// DeleteFile implements db.Repo.
func (r *repo) DeleteFile(ctx context.Context, owner, path string) error {
	if err := db.ValidatePath(path); err != nil {
		return err
	}
	const query = `DELETE FROM files
		WHERE owner_id = ? AND path = ?
		  AND NOT EXISTS (SELECT 1 FROM files child WHERE child.owner_id = ? AND child.parent_path = ?)`

	result, err := r.q.ExecContext(ctx, query, owner, path, owner, path)
	if err != nil {
		return fmt.Errorf("delete %q: %w", path, mapErr(err))
	}
	return sqlutil.CheckAffected(ctx, r.q, result, path, isDirProbe, owner, path)
}

// scanFileRow reads one row of fileColumns, for the queries that return many.
func scanFileRow(rows *sql.Rows) (db.File, error) {
	var f db.File
	var mtime int64
	if err := rows.Scan(&f.ID, &f.OwnerID, &f.Path, &f.BlobKey, &f.Size, &mtime, &f.ETag, &f.MIMEType, &f.IsDir); err != nil {
		return db.File{}, err
	}
	f.MTime = time.UnixMilli(mtime).UTC()
	return f, nil
}

func scanFile(row *sql.Row) (db.File, error) {
	var f db.File
	var mtime int64
	if err := row.Scan(&f.ID, &f.OwnerID, &f.Path, &f.BlobKey, &f.Size, &mtime, &f.ETag, &f.MIMEType, &f.IsDir); err != nil {
		return db.File{}, mapErr(err)
	}
	f.MTime = time.UnixMilli(mtime).UTC()
	return f, nil
}

// mapErr turns the driver's errors into the port's sentinels. The unique
// violation is the interesting one: it is how a move onto an occupied path is
// told apart from a broken database.
func mapErr(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return db.ErrNotFound
	}
	var serr *sqlitedriver.Error
	if errors.As(err, &serr) {
		switch serr.Code() {
		case sqlite3.SQLITE_CONSTRAINT_UNIQUE, sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY:
			return fmt.Errorf("%w: %w", db.ErrConflict, err)
		}
	}
	return err
}
