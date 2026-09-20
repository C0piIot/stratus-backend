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
	"strings"
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
//     It is half of that promise: see _txlock in New for the other half.
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
	query.Set("_txlock", "immediate")
	// Every transaction takes the write lock up front.
	//
	// Without this the driver opens one with a plain BEGIN, which is deferred:
	// a transaction that reads before it writes -- which is every write in
	// internal/files, since each one checks its parent first -- holds a read
	// snapshot and then has to upgrade. SQLite refuses that upgrade with
	// SQLITE_BUSY **immediately**, and busy_timeout does not apply, because
	// waiting cannot help: the snapshot it read is already stale.
	//
	// The symptom was two clients writing at once getting 500s -- measured at
	// 22 failures in 60 concurrent writes, which is a phone backing up a camera
	// roll. With the lock taken at BEGIN there is nothing to upgrade, so the
	// second writer waits out busy_timeout like it was always supposed to.
	query.Set("_txlock", "immediate")
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
	// Directories first and then by name, which is the order files_owner_parent
	// is built in. Ordering by path alone sent SQLite to the unique index on
	// (owner_id, path) instead -- it matched the sort, so the planner took a
	// scan of every row this owner has over sorting the handful in one folder,
	// and a directory of a hundred cost 58 ms on a library of a hundred
	// thousand rather than 0.3 (#160). It is also the order ListFilesPage
	// already returns, so the two no longer disagree.
	const query = `SELECT ` + fileColumns + ` FROM files
		WHERE owner_id = ? AND parent_path = ? ORDER BY is_dir DESC, path`

	out, err := sqlutil.Collect(ctx, r.q, scanFileRow, query, owner, dir)
	if err != nil {
		return nil, fmt.Errorf("list %q: %w", dir, mapErr(err))
	}
	return out, nil
}

// ListFilesPage implements db.Repo.
func (r *repo) ListFilesPage(ctx context.Context, owner, dir string, after db.Cursor, limit int) ([]db.File, error) {
	if err := db.ValidateDir(dir); err != nil {
		return nil, err
	}
	if err := db.ValidateLimit(limit); err != nil {
		return nil, err
	}

	// Two statements rather than one with a predicate pasted into it: the
	// queries in this package are consts a reader can grep for, and the first
	// page differs by one clause.
	//
	// files_owner_parent is (owner_id, parent_path, is_dir DESC, path), which
	// is this ORDER BY exactly, so both of these are a seek to where the last
	// page stopped rather than a sort of the directory.
	const (
		fromStart = `SELECT ` + fileColumns + ` FROM files
			WHERE owner_id = ? AND parent_path = ?
			ORDER BY is_dir DESC, path LIMIT ?`
		fromCursor = `SELECT ` + fileColumns + ` FROM files
			WHERE owner_id = ? AND parent_path = ?
				AND (is_dir < ? OR (is_dir = ? AND path > ?))
			ORDER BY is_dir DESC, path LIMIT ?`
	)

	query, args := fromStart, []any{owner, dir, limit}
	if !after.AtStart() {
		query = fromCursor
		args = []any{owner, dir, after.IsDir, after.IsDir, after.Path, limit}
	}

	out, err := sqlutil.Collect(ctx, r.q, scanFileRow, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list %q after %q: %w", dir, after.Path, mapErr(err))
	}
	return out, nil
}

const mediaColumns = `file_id, kind, indexed_at, version, etag, error, retry_at, taken_at, width, height, orientation, latitude, longitude, camera, duration_ms, codec, artist, album, title, track_no, disc_no, year, genre, album_artist`

// mediaWriteColumns is the read list plus the three folded columns a search
// matches on. They are written and filtered but never read back: they are how
// the row is stored, not part of what a db.Media is, so scanMedia does not know
// about them.
const mediaWriteColumns = mediaColumns + `, search_song, search_album, search_album_artist`

// PutMedia implements db.MediaIndex.
func (r *repo) PutMedia(ctx context.Context, m db.Media) error {
	m = m.Normalize()
	folded := m.Fold()

	var takenAt any
	if !m.TakenAt.IsZero() {
		takenAt = m.TakenAt.UnixMilli()
	}

	var retryAt any
	if !m.RetryAt.IsZero() {
		retryAt = m.RetryAt.UnixMilli()
	}

	var lat, lon any
	if m.GPS != nil {
		lat, lon = m.GPS.Latitude, m.GPS.Longitude
	}

	const query = `INSERT INTO media (` + mediaWriteColumns + `)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (file_id) DO UPDATE SET
			kind = excluded.kind, indexed_at = excluded.indexed_at, version = excluded.version,
			etag = excluded.etag, error = excluded.error, retry_at = excluded.retry_at,
			taken_at = excluded.taken_at, width = excluded.width,
			height = excluded.height, orientation = excluded.orientation,
			latitude = excluded.latitude, longitude = excluded.longitude, camera = excluded.camera,
			duration_ms = excluded.duration_ms, codec = excluded.codec, artist = excluded.artist,
			album = excluded.album, title = excluded.title, track_no = excluded.track_no,
			disc_no = excluded.disc_no, year = excluded.year, genre = excluded.genre,
			album_artist = excluded.album_artist, search_song = excluded.search_song,
			search_album = excluded.search_album,
			search_album_artist = excluded.search_album_artist`

	_, err := r.q.ExecContext(ctx, query,
		m.FileID, string(m.Kind), m.IndexedAt.UnixMilli(), m.Version, m.ETag, m.Error, retryAt, takenAt,
		m.Width, m.Height, m.Orientation, lat, lon, m.Camera,
		m.DurationMS, m.Codec, m.Artist, m.Album, m.Title,
		m.TrackNo, m.DiscNo, m.Year, m.Genre,
		m.AlbumArtist, folded.Song, folded.Album, folded.AlbumArtist,
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
func (r *repo) PendingMedia(ctx context.Context, version int, now time.Time, limit int) ([]db.File, error) {
	// The queue is this LEFT JOIN. A row with an error counts as done, or a
	// file nothing can parse would come back on every pass forever -- but only
	// while it describes the bytes that are there: an overwrite keeps the row
	// and its id, so the etag comparison is what puts it back in the queue.
	//
	// And only while the error was a verdict. A failure on the way to the bytes
	// leaves a time on the row instead, and the last clause is what brings the
	// file back when it arrives (#157).
	const query = `SELECT f.` + `id, f.owner_id, f.path, f.blob_key, f.size, f.mtime, f.etag, f.mime_type, f.is_dir
		FROM files f LEFT JOIN media m ON m.file_id = f.id
		WHERE f.is_dir = 0 AND (m.file_id IS NULL OR m.version < ? OR m.etag <> f.etag
			OR (m.retry_at IS NOT NULL AND m.retry_at <= ?))
		ORDER BY f.id LIMIT ?`

	out, err := sqlutil.Collect(ctx, r.q, scanFileRow, query, version, now.UnixMilli(), limit)
	if err != nil {
		return nil, fmt.Errorf("list pending media: %w", mapErr(err))
	}
	return out, nil
}

// MediaCounts implements db.MediaIndex.
//
// COUNT over a CASE rather than a FILTER clause or three queries: the filtered
// aggregate is not portable, and the three conditions have to see the same rows
// at the same instant or the numbers would not add up. It walks every file row,
// which is what counting an absence costs.
func (r *repo) MediaCounts(ctx context.Context, version int) (db.MediaCounts, error) {
	const query = `SELECT COUNT(*),
			COUNT(CASE WHEN m.version >= ? AND m.etag = f.etag AND m.error = '' THEN 1 END),
			COUNT(CASE WHEN m.version >= ? AND m.etag = f.etag AND m.error <> '' AND m.retry_at IS NULL THEN 1 END)
		FROM files f LEFT JOIN media m ON m.file_id = f.id
		WHERE f.is_dir = 0`

	var c db.MediaCounts
	err := r.q.QueryRowContext(ctx, query, version, version).Scan(&c.Files, &c.Indexed, &c.Failed)
	if err != nil {
		return db.MediaCounts{}, fmt.Errorf("count media: %w", mapErr(err))
	}
	return c, nil
}

// MediaStates implements db.MediaIndex.
//
// The only statement in this package built rather than declared, because the
// placeholder list is as long as what the caller is rendering. It stays here
// rather than in sqlutil for the reason that package states about itself: a
// placeholder is dialect, and this one is bounded by a page.
func (r *repo) MediaStates(ctx context.Context, fileIDs []int64) (map[int64]db.MediaState, error) {
	if len(fileIDs) == 0 {
		return map[int64]db.MediaState{}, nil
	}

	// A row waiting to be tried again reports as not failed: the listing marks
	// it as nothing has looked at it yet, which is what is true.
	query := `SELECT file_id, version, etag, error <> '' AND retry_at IS NULL FROM media WHERE file_id IN (?` +
		strings.Repeat(", ?", len(fileIDs)-1) + `)`
	args := make([]any, len(fileIDs))
	for i, id := range fileIDs {
		args[i] = id
	}

	rows, err := sqlutil.Collect(ctx, r.q, scanMediaState, query, args...)
	if err != nil {
		return nil, fmt.Errorf("read media states: %w", mapErr(err))
	}
	out := make(map[int64]db.MediaState, len(rows))
	for _, row := range rows {
		out[row.id] = row.state
	}
	return out, nil
}

// mediaStateRow is one row of MediaStates, keyed on the way out.
type mediaStateRow struct {
	id    int64
	state db.MediaState
}

func scanMediaState(rows *sql.Rows) (mediaStateRow, error) {
	var row mediaStateRow
	err := rows.Scan(&row.id, &row.state.Version, &row.state.ETag, &row.state.Failed)
	return row, err
}

func scanMedia(row *sql.Row) (db.Media, error) {
	var m db.Media
	var kind string
	var lat, lon sql.NullFloat64
	var indexedAt int64
	var takenAt, retryAt sql.NullInt64

	err := row.Scan(&m.FileID, &kind, &indexedAt, &m.Version, &m.ETag, &m.Error, &retryAt, &takenAt,
		&m.Width, &m.Height, &m.Orientation, &lat, &lon, &m.Camera,
		&m.DurationMS, &m.Codec, &m.Artist, &m.Album, &m.Title,
		&m.TrackNo, &m.DiscNo, &m.Year, &m.Genre, &m.AlbumArtist)
	if err != nil {
		return db.Media{}, mapErr(err)
	}

	m.Kind = db.Kind(kind)
	m.IndexedAt = time.UnixMilli(indexedAt).UTC()
	if takenAt.Valid {
		m.TakenAt = time.UnixMilli(takenAt.Int64).UTC()
	}
	if retryAt.Valid {
		m.RetryAt = time.UnixMilli(retryAt.Int64).UTC()
	}
	if lat.Valid && lon.Valid {
		m.GPS = &db.GPS{Latitude: lat.Float64, Longitude: lon.Float64}
	}
	return m, nil
}

// joinedFileColumns and joinedMediaColumns are the same lists qualified for a
// join, where both tables carry columns the other also has.
var (
	joinedFileColumns  = "f." + strings.ReplaceAll(fileColumns, ", ", ", f.")
	joinedMediaColumns = "m." + strings.ReplaceAll(mediaColumns, ", ", ", m.")
)

// Artists implements db.Repo.
func (r *repo) Artists(ctx context.Context, owner string) ([]db.Artist, error) {
	const query = `SELECT m.album_artist, COUNT(DISTINCT m.album)
		FROM media m JOIN files f ON f.id = m.file_id
		WHERE f.owner_id = ? AND m.kind = ? AND m.album_artist <> '' AND m.album <> ''
		GROUP BY m.album_artist
		ORDER BY m.album_artist`

	out, err := sqlutil.Collect(ctx, r.q, scanArtist, query, owner, string(db.KindAudio))
	if err != nil {
		return nil, fmt.Errorf("list artists: %w", mapErr(err))
	}
	return out, nil
}

// albumSelect and albumGroup are the aggregate every album listing shares.
// Written once because two listings that counted an album's songs differently
// would show a user two numbers for one album, and nobody would find out why.
const (
	albumSelect = `SELECT m.album_artist, m.album, COUNT(*), COALESCE(SUM(m.duration_ms), 0),
			MAX(m.year), MAX(m.genre), MIN(f.mtime)
		FROM media m JOIN files f ON f.id = m.file_id`
	albumGroup = ` GROUP BY m.album_artist, m.album`
)

// Albums implements db.Repo.
func (r *repo) Albums(ctx context.Context, owner, artist string) ([]db.Album, error) {
	// The artist is matched or ignored by the same clause, so one query serves
	// both a listing of the whole library and of one artist.
	const query = albumSelect + `
		WHERE f.owner_id = ? AND m.kind = ? AND m.album <> ''
		  AND (? = '' OR m.album_artist = ?)` + albumGroup + `
		ORDER BY m.album_artist, m.album`

	out, err := sqlutil.Collect(ctx, r.q, scanAlbum, query, owner, string(db.KindAudio), artist, artist)
	if err != nil {
		return nil, fmt.Errorf("list albums: %w", mapErr(err))
	}
	return out, nil
}

// Tracks implements db.Repo.
func (r *repo) Tracks(ctx context.Context, owner, artist, album string) ([]db.Track, error) {
	query := `SELECT ` + joinedFileColumns + `, ` + joinedMediaColumns + `
		FROM media m JOIN files f ON f.id = m.file_id
		WHERE f.owner_id = ? AND m.kind = ? AND m.album_artist = ? AND m.album = ?
		ORDER BY m.disc_no, m.track_no, f.path`

	out, err := sqlutil.Collect(ctx, r.q, scanTrack, query, owner, string(db.KindAudio), artist, album)
	if err != nil {
		return nil, fmt.Errorf("list tracks of %q: %w", album, mapErr(err))
	}
	return out, nil
}

// TracksIn implements db.Repo.
func (r *repo) TracksIn(ctx context.Context, owner, dir string) ([]db.Track, error) {
	query := `SELECT ` + joinedFileColumns + `, ` + joinedMediaColumns + `
		FROM media m JOIN files f ON f.id = m.file_id
		WHERE f.owner_id = ? AND f.parent_path = ? AND m.kind = ?
		ORDER BY f.path`

	out, err := sqlutil.Collect(ctx, r.q, scanTrack, query, owner, dir, string(db.KindAudio))
	if err != nil {
		return nil, fmt.Errorf("list tracks in %q: %w", dir, mapErr(err))
	}
	return out, nil
}

// TrackByFile implements db.Repo.
func (r *repo) TrackByFile(ctx context.Context, owner string, fileID int64) (db.Track, error) {
	query := `SELECT ` + joinedFileColumns + `, ` + joinedMediaColumns + `
		FROM media m JOIN files f ON f.id = m.file_id
		WHERE f.owner_id = ? AND f.id = ?`

	out, err := sqlutil.Collect(ctx, r.q, scanTrack, query, owner, fileID)
	if err != nil {
		return db.Track{}, fmt.Errorf("get track %d: %w", fileID, mapErr(err))
	}
	if len(out) == 0 {
		return db.Track{}, fmt.Errorf("get track %d: %w", fileID, db.ErrNotFound)
	}
	return out[0], nil
}

// AlbumList implements db.Repo.
//
// The genre and the year are filtered in HAVING rather than in WHERE, and that
// is not a style choice: an album's year is MAX(m.year) over its tracks and its
// song count is COUNT(*) over them, so a WHERE that dropped the tracks not
// matching would leave the album in the answer with the wrong numbers beside it.
func (r *repo) AlbumList(ctx context.Context, owner string, f db.AlbumFilter) ([]db.Album, error) {
	order, err := albumOrder(f.Order)
	if err != nil {
		return nil, err
	}

	query := albumSelect + `
		WHERE f.owner_id = ? AND m.kind = ? AND m.album <> ''` + albumGroup + `
		HAVING (? = '' OR SUM(CASE WHEN m.genre = ? THEN 1 ELSE 0 END) > 0)
		   AND (? = 0 OR MAX(m.year) >= ?)
		   AND (? = 0 OR MAX(m.year) <= ?)
		ORDER BY ` + order + `
		LIMIT ? OFFSET ?`

	out, err := sqlutil.Collect(ctx, r.q, scanAlbum, query,
		owner, string(db.KindAudio),
		f.Genre, f.Genre, f.FromYear, f.FromYear, f.ToYear, f.ToYear,
		f.Page.Limit, f.Page.Offset)
	if err != nil {
		return nil, fmt.Errorf("list albums: %w", mapErr(err))
	}
	return out, nil
}

// albumOrder is the ORDER BY for each way a client asks to see a library. Every
// one of them ends in the same tie-break, so a page boundary lands in the same
// place twice -- except the random one, which is a different set by definition.
func albumOrder(o db.AlbumOrder) (string, error) {
	const tie = `, m.album_artist, m.album`
	switch o {
	case db.AlbumsByName:
		return `m.album, m.album_artist`, nil
	case db.AlbumsByArtist:
		return `m.album_artist, m.album`, nil
	case db.AlbumsByAdded:
		return `MIN(f.mtime) DESC` + tie, nil
	case db.AlbumsByYear:
		return `MAX(m.year)` + tie, nil
	case db.AlbumsByYearDesc:
		return `MAX(m.year) DESC` + tie, nil
	case db.AlbumsRandom:
		return `random()`, nil
	default:
		return "", fmt.Errorf("list albums: unknown order %q", o)
	}
}

// TrackList implements db.Repo. The year is the track's own here, unlike
// AlbumList: nothing is aggregated, so there is nothing to distort.
func (r *repo) TrackList(ctx context.Context, owner string, f db.TrackFilter) ([]db.Track, error) {
	order, err := trackOrder(f.Order)
	if err != nil {
		return nil, err
	}

	query := `SELECT ` + joinedFileColumns + `, ` + joinedMediaColumns + `
		FROM media m JOIN files f ON f.id = m.file_id
		WHERE f.owner_id = ? AND m.kind = ?
		  AND (? = '' OR m.genre = ?)
		  AND (? = 0 OR m.year >= ?)
		  AND (? = 0 OR m.year <= ?)
		ORDER BY ` + order + `
		LIMIT ? OFFSET ?`

	out, err := sqlutil.Collect(ctx, r.q, scanTrack, query,
		owner, string(db.KindAudio),
		f.Genre, f.Genre, f.FromYear, f.FromYear, f.ToYear, f.ToYear,
		f.Page.Limit, f.Page.Offset)
	if err != nil {
		return nil, fmt.Errorf("list tracks: %w", mapErr(err))
	}
	return out, nil
}

func trackOrder(o db.TrackOrder) (string, error) {
	switch o {
	case db.TracksByPath:
		return `f.path`, nil
	case db.TracksRandom:
		return `random()`, nil
	default:
		return "", fmt.Errorf("list tracks: unknown order %q", o)
	}
}

// Genres implements db.Repo.
//
// The album count is over the same album key the listings group by, so a genre
// and an album listing cannot disagree about what an album is. The separator is
// a control character because a tag can contain any printable one, and two
// albums must not collide because of where the join happened to fall.
func (r *repo) Genres(ctx context.Context, owner string) ([]db.Genre, error) {
	const query = `SELECT m.genre, COUNT(*),
			COUNT(DISTINCT CASE WHEN m.album <> ''
				THEN m.album_artist || char(31) || m.album END)
		FROM media m JOIN files f ON f.id = m.file_id
		WHERE f.owner_id = ? AND m.kind = ? AND m.genre <> ''
		GROUP BY m.genre
		ORDER BY m.genre`

	out, err := sqlutil.Collect(ctx, r.q, scanGenre, query, owner, string(db.KindAudio))
	if err != nil {
		return nil, fmt.Errorf("list genres: %w", mapErr(err))
	}
	return out, nil
}

// Search implements db.Repo.
//
// Three queries, and every one of them matches a column this process folded
// rather than calling lower() here: SQLite folds ASCII and PostgreSQL folds
// Unicode, so the engine deciding would make the two drivers answer differently
// for an accented capital.
//
// The folded columns are constant within an album, so the album and artist
// filters live in WHERE and not HAVING -- unlike the genre and the year, which
// are aggregates.
func (r *repo) Search(ctx context.Context, owner string, f db.SearchFilter) (db.SearchResult, error) {
	term := sqlutil.Contains(db.FoldQuery(f.Text))
	kind := string(db.KindAudio)

	const artists = `SELECT m.album_artist, COUNT(DISTINCT m.album)
		FROM media m JOIN files f ON f.id = m.file_id
		WHERE f.owner_id = ? AND m.kind = ? AND m.album_artist <> '' AND m.album <> ''
		  AND m.search_album_artist LIKE ? ESCAPE '` + sqlutil.LikeEscape + `'
		GROUP BY m.album_artist
		ORDER BY m.album_artist
		LIMIT ? OFFSET ?`

	const albums = albumSelect + `
		WHERE f.owner_id = ? AND m.kind = ? AND m.album <> ''
		  AND m.search_album LIKE ? ESCAPE '` + sqlutil.LikeEscape + `'` + albumGroup + `
		ORDER BY m.album_artist, m.album
		LIMIT ? OFFSET ?`

	tracks := `SELECT ` + joinedFileColumns + `, ` + joinedMediaColumns + `
		FROM media m JOIN files f ON f.id = m.file_id
		WHERE f.owner_id = ? AND m.kind = ?
		  AND (m.search_song LIKE ? ESCAPE '` + sqlutil.LikeEscape + `'
		    OR m.search_album LIKE ? ESCAPE '` + sqlutil.LikeEscape + `'
		    OR m.search_album_artist LIKE ? ESCAPE '` + sqlutil.LikeEscape + `')
		ORDER BY m.album_artist, m.album, m.disc_no, m.track_no, f.path
		LIMIT ? OFFSET ?`

	var out db.SearchResult
	var err error
	if out.Artists, err = sqlutil.Collect(ctx, r.q, scanArtist, artists,
		owner, kind, term, f.Artists.Limit, f.Artists.Offset); err != nil {
		return db.SearchResult{}, fmt.Errorf("search artists: %w", mapErr(err))
	}
	if out.Albums, err = sqlutil.Collect(ctx, r.q, scanAlbum, albums,
		owner, kind, term, f.Albums.Limit, f.Albums.Offset); err != nil {
		return db.SearchResult{}, fmt.Errorf("search albums: %w", mapErr(err))
	}
	if out.Tracks, err = sqlutil.Collect(ctx, r.q, scanTrack, tracks,
		owner, kind, term, term, term, f.Tracks.Limit, f.Tracks.Offset); err != nil {
		return db.SearchResult{}, fmt.Errorf("search tracks: %w", mapErr(err))
	}
	return out, nil
}

func scanGenre(rows *sql.Rows) (db.Genre, error) {
	var g db.Genre
	err := rows.Scan(&g.Name, &g.SongCount, &g.AlbumCount)
	return g, err
}

func scanArtist(rows *sql.Rows) (db.Artist, error) {
	var a db.Artist
	err := rows.Scan(&a.Name, &a.AlbumCount)
	return a, err
}

func scanAlbum(rows *sql.Rows) (db.Album, error) {
	var a db.Album
	var created int64
	if err := rows.Scan(&a.Artist, &a.Name, &a.SongCount, &a.DurationMS,
		&a.Year, &a.Genre, &created); err != nil {
		return db.Album{}, err
	}
	a.Created = time.UnixMilli(created).UTC()
	return a, nil
}

// scanTrack reads a file row and a media row from one joined result. The two
// halves are scanned the same way the single-table readers do it, because the
// conversions are a property of the columns and not of the query.
func scanTrack(rows *sql.Rows) (db.Track, error) {
	var t db.Track
	var mtime, indexedAt int64
	var kind string
	var lat, lon sql.NullFloat64
	var takenAt, retryAt sql.NullInt64

	err := rows.Scan(
		&t.File.ID, &t.File.OwnerID, &t.File.Path, &t.File.BlobKey, &t.File.Size,
		&mtime, &t.File.ETag, &t.File.MIMEType, &t.File.IsDir,
		&t.Media.FileID, &kind, &indexedAt, &t.Media.Version, &t.Media.ETag, &t.Media.Error, &retryAt, &takenAt,
		&t.Media.Width, &t.Media.Height, &t.Media.Orientation, &lat, &lon, &t.Media.Camera,
		&t.Media.DurationMS, &t.Media.Codec, &t.Media.Artist, &t.Media.Album, &t.Media.Title,
		&t.Media.TrackNo, &t.Media.DiscNo, &t.Media.Year, &t.Media.Genre, &t.Media.AlbumArtist)
	if err != nil {
		return db.Track{}, err
	}

	t.File.MTime = time.UnixMilli(mtime).UTC()
	t.Media.Kind = db.Kind(kind)
	t.Media.IndexedAt = time.UnixMilli(indexedAt).UTC()
	if takenAt.Valid {
		t.Media.TakenAt = time.UnixMilli(takenAt.Int64).UTC()
	}
	if retryAt.Valid {
		t.Media.RetryAt = time.UnixMilli(retryAt.Int64).UTC()
	}
	if lat.Valid && lon.Valid {
		t.Media.GPS = &db.GPS{Latitude: lat.Float64, Longitude: lon.Float64}
	}
	return t, nil
}

// BlobKeys implements db.Repo.
func (r *repo) BlobKeys(ctx context.Context) iter.Seq2[string, error] {
	const query = `SELECT blob_key FROM files WHERE is_dir = 0`

	return sqlutil.Label(sqlutil.Seq(ctx, r.q, sqlutil.ScanOne[string], query), "list blob keys")
}

// MoveFile implements db.Repo.
// SubtreeSize implements db.Files.
func (r *repo) SubtreeSize(ctx context.Context, owner, dir string) (int64, error) {
	if err := db.ValidateDir(dir); err != nil {
		return 0, err
	}

	// Two statements rather than one with a predicate pasted in, the way
	// ListFilesPage is two: the queries in this package are consts a reader can
	// grep for.
	const (
		whole = `SELECT COALESCE(SUM(size), 0) FROM files WHERE owner_id = ? AND is_dir = 0`
		under = `SELECT COALESCE(SUM(size), 0) FROM files
			WHERE owner_id = ? AND is_dir = 0 AND path >= ? AND path < ?`
	)

	query, args := whole, []any{owner}
	if from, to, all := db.SubtreeRange(dir); !all {
		query, args = under, []any{owner, from, to}
	}

	var total int64
	if err := r.q.QueryRowContext(ctx, query, args...).Scan(&total); err != nil {
		return 0, fmt.Errorf("size of %q: %w", dir, mapErr(err))
	}
	return total, nil
}

func (r *repo) MoveFile(ctx context.Context, owner, from, to string) error {
	if err := db.ValidateMove(from, to); err != nil {
		return err
	}

	const moveOne = `UPDATE files SET path = ?, parent_path = ?
		WHERE owner_id = ? AND path = ?`

	// Everything under it, in one statement: a rewrite that took a row at a
	// time would leave the tree in a state nothing could read if it stopped
	// halfway. substr is 1-based, so the +1 is the character after the old
	// prefix, and parent_path is rewritten from parent_path rather than from
	// path so the two assignments cannot depend on each other's order.
	const moveTree = `UPDATE files
		SET path = ? || substr(path, ?), parent_path = ? || substr(parent_path, ?)
		WHERE owner_id = ? AND path LIKE ? ESCAPE '` + sqlutil.LikeEscape + `'`

	result, err := r.q.ExecContext(ctx, moveOne, to, db.ParentOf(to), owner, from)
	if err != nil {
		return fmt.Errorf("move %q to %q: %w", from, to, mapErr(err))
	}
	if err := sqlutil.CheckAffected(ctx, r.q, result, from, isDirProbe, owner, from); err != nil {
		return err
	}

	tail := len(from) + 1
	if _, err := r.q.ExecContext(ctx, moveTree, to, tail, to, tail, owner, sqlutil.Under(from)); err != nil {
		return fmt.Errorf("move the contents of %q to %q: %w", from, to, mapErr(err))
	}
	return nil
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
		case sqlite3.SQLITE_CONSTRAINT_FOREIGNKEY:
			// A row whose parent is gone, which is a write racing a delete.
			// What the caller needs to hear is "it is not there", not the
			// shape of a constraint.
			return fmt.Errorf("%w: %w", db.ErrNotFound, err)
		}
	}
	return err
}

const uploadColumns = `id, owner_id, path, size, received, blob_key, store_id, digest, mime_type, expires_at`

// PutUpload implements db.Uploads.
func (r *repo) PutUpload(ctx context.Context, u db.Upload) error {
	if err := db.ValidatePath(u.Path); err != nil {
		return err
	}

	// Only what an append changes is updated: everything else about an upload
	// is decided when it is created and a later request has no business
	// rewriting where the bytes are going.
	const query = `INSERT INTO uploads (` + uploadColumns + `)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET
			received = excluded.received, digest = excluded.digest,
			expires_at = excluded.expires_at`

	_, err := r.q.ExecContext(ctx, query,
		u.ID, u.OwnerID, u.Path, u.Size, u.Received, u.BlobKey, u.StoreID,
		u.Digest, u.MIMEType, u.ExpiresAt.UnixMilli(),
	)
	if err != nil {
		return fmt.Errorf("put upload %q: %w", u.ID, mapErr(err))
	}
	return nil
}

// UploadByID implements db.Uploads.
func (r *repo) UploadByID(ctx context.Context, owner, id string) (db.Upload, error) {
	const query = `SELECT ` + uploadColumns + ` FROM uploads WHERE id = ? AND owner_id = ?`

	u, err := scanUpload(r.q.QueryRowContext(ctx, query, id, owner))
	if err != nil {
		return db.Upload{}, fmt.Errorf("get upload %q: %w", id, err)
	}
	return u, nil
}

// DeleteUpload implements db.Uploads.
func (r *repo) DeleteUpload(ctx context.Context, owner, id string) error {
	const query = `DELETE FROM uploads WHERE id = ? AND owner_id = ?`

	if _, err := r.q.ExecContext(ctx, query, id, owner); err != nil {
		return fmt.Errorf("delete upload %q: %w", id, mapErr(err))
	}
	return nil
}

// ExpiredUploads implements db.Uploads.
func (r *repo) ExpiredUploads(ctx context.Context, now time.Time) iter.Seq2[db.Upload, error] {
	const query = `SELECT ` + uploadColumns + ` FROM uploads WHERE expires_at <= ? ORDER BY expires_at`

	return sqlutil.Label(sqlutil.Seq(ctx, r.q, scanUploadRow, query, now.UnixMilli()), "list expired uploads")
}

func scanUpload(row *sql.Row) (db.Upload, error) {
	var u db.Upload
	var expires int64
	if err := row.Scan(&u.ID, &u.OwnerID, &u.Path, &u.Size, &u.Received,
		&u.BlobKey, &u.StoreID, &u.Digest, &u.MIMEType, &expires); err != nil {
		return db.Upload{}, mapErr(err)
	}
	u.ExpiresAt = time.UnixMilli(expires).UTC()
	return u, nil
}

func scanUploadRow(rows *sql.Rows) (db.Upload, error) {
	var u db.Upload
	var expires int64
	if err := rows.Scan(&u.ID, &u.OwnerID, &u.Path, &u.Size, &u.Received,
		&u.BlobKey, &u.StoreID, &u.Digest, &u.MIMEType, &expires); err != nil {
		return db.Upload{}, err
	}
	u.ExpiresAt = time.UnixMilli(expires).UTC()
	return u, nil
}
