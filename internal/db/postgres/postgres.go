// Package postgres implements the metadata port on PostgreSQL, through pgx.
//
// It exists as much to keep internal/db honest as to be deployed: a port with
// one implementation only records that implementation's habits. Writing this
// alongside the SQLite driver is what turns internal/db/dbtest into a contract.
package postgres

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"iter"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" driver

	"github.com/C0piIot/stratus-backend/internal/db"
	"github.com/C0piIot/stratus-backend/internal/db/sqlutil"
)

//go:embed migrations/*.sql
var migrations embed.FS

// uniqueViolation is SQLSTATE 23505.
const (
	uniqueViolation     = "23505"
	foreignKeyViolation = "23503"
)

// Store is a db.Store backed by a PostgreSQL database.
type Store struct {
	*repo
	db *sql.DB
}

var _ db.Store = (*Store)(nil)

// New connects to the database described by dsn, which is a libpq URL.
func New(ctx context.Context, dsn string) (*Store, error) {
	if dsn == "" {
		return nil, errors.New("postgres: dsn is required")
	}
	sqlDB, err := sql.Open("pgx", dsn)
	if err != nil {
		// The DSN carries a password, so it is never in the message.
		return nil, fmt.Errorf("postgres: open: %w", err)
	}
	if err := sqlDB.PingContext(ctx); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("postgres: connect: %w", err)
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
const isDirProbe = `SELECT is_dir FROM files WHERE owner_id = $1 AND path = $2`

const fileColumns = `id, owner_id, path, blob_key, size, mtime, etag, mime_type, is_dir`

// PutFile implements db.Repo.
func (r *repo) PutFile(ctx context.Context, f db.File) (db.File, error) {
	if err := db.ValidatePath(f.Path); err != nil {
		return db.File{}, err
	}
	f = f.Normalize()

	const query = `INSERT INTO files (owner_id, path, parent_path, blob_key, size, mtime, etag, mime_type)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (owner_id, path) DO UPDATE SET
			blob_key = excluded.blob_key, size = excluded.size, mtime = excluded.mtime,
			etag = excluded.etag, mime_type = excluded.mime_type
		WHERE files.is_dir = FALSE
		RETURNING id`

	err := r.q.QueryRowContext(ctx, query,
		f.OwnerID, f.Path, db.ParentOf(f.Path), f.BlobKey, f.Size,
		f.MTime, f.ETag, f.MIMEType,
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
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, TRUE)
		RETURNING id`

	dir := db.File{OwnerID: owner, Path: path, MTime: time.Now(), IsDir: true}.Normalize()
	err := r.q.QueryRowContext(ctx, query,
		dir.OwnerID, dir.Path, db.ParentOf(dir.Path), "", 0,
		dir.MTime, "", "",
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
	const query = `SELECT ` + fileColumns + ` FROM files WHERE owner_id = $1 AND path = $2`

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
		WHERE owner_id = $1 AND parent_path = $2 ORDER BY is_dir DESC, path`

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
			WHERE owner_id = $1 AND parent_path = $2
			ORDER BY is_dir DESC, path LIMIT $3`
		fromCursor = `SELECT ` + fileColumns + ` FROM files
			WHERE owner_id = $1 AND parent_path = $2
				AND (is_dir < $3 OR (is_dir = $3 AND path > $4))
			ORDER BY is_dir DESC, path LIMIT $5`
	)

	query, args := fromStart, []any{owner, dir, limit}
	if !after.AtStart() {
		query = fromCursor
		args = []any{owner, dir, after.IsDir, after.Path, limit}
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
		takenAt = m.TakenAt
	}

	var retryAt any
	if !m.RetryAt.IsZero() {
		retryAt = m.RetryAt
	}

	var lat, lon any
	if m.GPS != nil {
		lat, lon = m.GPS.Latitude, m.GPS.Longitude
	}

	const query = `INSERT INTO media (` + mediaWriteColumns + `)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19,
			$20, $21, $22, $23, $24, $25, $26, $27)
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
		m.FileID, string(m.Kind), m.IndexedAt, m.Version, m.ETag, m.Error, retryAt, takenAt,
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
	const query = `SELECT ` + mediaColumns + ` FROM media WHERE file_id = $1`

	m, err := scanMedia(r.q.QueryRowContext(ctx, query, fileID))
	if err != nil {
		return db.Media{}, fmt.Errorf("get media for file %d: %w", fileID, err)
	}
	return m, nil
}

// PendingMedia implements db.MediaIndex.
func (r *repo) PendingMedia(ctx context.Context, version int, now time.Time, limit int) ([]db.File, error) {
	// The queue is this LEFT JOIN. A row with an error counts as done, or a
	// file nothing can parse would come back on every pass forever -- and only
	// while the error was a verdict: a failure on the way to the bytes leaves a
	// time on the row instead, and the last clause brings the file back when it
	// arrives (#157).
	const query = `SELECT f.` + `id, f.owner_id, f.path, f.blob_key, f.size, f.mtime, f.etag, f.mime_type, f.is_dir
		FROM files f LEFT JOIN media m ON m.file_id = f.id
		WHERE f.is_dir = FALSE AND (m.file_id IS NULL OR m.version < $1 OR m.etag <> f.etag
			OR (m.retry_at IS NOT NULL AND m.retry_at <= $2))
		ORDER BY f.id LIMIT $3`

	out, err := sqlutil.Collect(ctx, r.q, scanFileRow, query, version, now, limit)
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
			COUNT(CASE WHEN m.version >= $1 AND m.etag = f.etag AND m.error = '' THEN 1 END),
			COUNT(CASE WHEN m.version >= $1 AND m.etag = f.etag AND m.error <> '' AND m.retry_at IS NULL THEN 1 END)
		FROM files f LEFT JOIN media m ON m.file_id = f.id
		WHERE f.is_dir = FALSE`

	var c db.MediaCounts
	err := r.q.QueryRowContext(ctx, query, version).Scan(&c.Files, &c.Indexed, &c.Failed)
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

	var places strings.Builder
	args := make([]any, len(fileIDs))
	for i, id := range fileIDs {
		if i > 0 {
			places.WriteString(", ")
		}
		places.WriteString("$" + strconv.Itoa(i+1))
		args[i] = id
	}
	// A row waiting to be tried again reports as not failed: the listing marks
	// it as nothing has looked at it yet, which is what is true.
	query := `SELECT file_id, version, etag, error <> '' AND retry_at IS NULL FROM media WHERE file_id IN (` +
		places.String() + `)`

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
	var indexedAt time.Time
	var takenAt, retryAt sql.NullTime

	err := row.Scan(&m.FileID, &kind, &indexedAt, &m.Version, &m.ETag, &m.Error, &retryAt, &takenAt,
		&m.Width, &m.Height, &m.Orientation, &lat, &lon, &m.Camera,
		&m.DurationMS, &m.Codec, &m.Artist, &m.Album, &m.Title,
		&m.TrackNo, &m.DiscNo, &m.Year, &m.Genre, &m.AlbumArtist)
	if err != nil {
		return db.Media{}, mapErr(err)
	}

	m.Kind = db.Kind(kind)
	m.IndexedAt = indexedAt.UTC()
	if takenAt.Valid {
		m.TakenAt = takenAt.Time.UTC()
	}
	if retryAt.Valid {
		m.RetryAt = retryAt.Time.UTC()
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
		WHERE f.owner_id = $1 AND m.kind = $2 AND m.album_artist <> '' AND m.album <> ''
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
		WHERE f.owner_id = $1 AND m.kind = $2 AND m.album <> ''
		  AND ($3 = '' OR m.album_artist = $3)` + albumGroup + `
		ORDER BY m.album_artist, m.album`

	out, err := sqlutil.Collect(ctx, r.q, scanAlbum, query, owner, string(db.KindAudio), artist)
	if err != nil {
		return nil, fmt.Errorf("list albums: %w", mapErr(err))
	}
	return out, nil
}

// Tracks implements db.Repo.
func (r *repo) Tracks(ctx context.Context, owner, artist, album string) ([]db.Track, error) {
	query := `SELECT ` + joinedFileColumns + `, ` + joinedMediaColumns + `
		FROM media m JOIN files f ON f.id = m.file_id
		WHERE f.owner_id = $1 AND m.kind = $2 AND m.album_artist = $3 AND m.album = $4
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
		WHERE f.owner_id = $1 AND f.parent_path = $2 AND m.kind = $3
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
		WHERE f.owner_id = $1 AND f.id = $2`

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
	o, err := albumOrder(f.Order)
	if err != nil {
		return nil, err
	}

	query := albumSelect + o.join + `
		WHERE f.owner_id = $1 AND m.kind = $2 AND m.album <> ''` + albumGroup + `
		HAVING ($3 = '' OR SUM(CASE WHEN m.genre = $3 THEN 1 ELSE 0 END) > 0)
		   AND ($4 = 0 OR MAX(m.year) >= $4)
		   AND ($5 = 0 OR MAX(m.year) <= $5)` + o.having + `
		ORDER BY ` + o.by + `
		LIMIT $6 OFFSET $7`

	out, err := sqlutil.Collect(ctx, r.q, scanAlbum, query,
		owner, string(db.KindAudio), f.Genre, f.FromYear, f.ToYear,
		f.Page.Limit, f.Page.Offset)
	if err != nil {
		return nil, fmt.Errorf("list albums: %w", mapErr(err))
	}
	return out, nil
}

// albumListing is what one order adds to the album aggregate: a join for the
// orders that read annotations, a condition on the group for the ones that are
// also filters, and the ORDER BY.
type albumListing struct{ join, having, by string }

// albumOrder is the listing for each way a client asks to see a library. Every
// one of them ends in the same tie-break, so a page boundary lands in the same
// place twice -- except the random one, which is a different set by definition.
func albumOrder(o db.AlbumOrder) (albumListing, error) {
	const tie = `, m.album_artist, m.album`
	switch o {
	case db.AlbumsByName:
		return albumListing{by: `m.album, m.album_artist`}, nil
	case db.AlbumsByArtist:
		return albumListing{by: `m.album_artist, m.album`}, nil
	case db.AlbumsByAdded:
		return albumListing{by: `MIN(f.mtime) DESC` + tie}, nil
	case db.AlbumsByYear:
		return albumListing{by: `MAX(m.year)` + tie}, nil
	case db.AlbumsByYearDesc:
		return albumListing{by: `MAX(m.year) DESC` + tie}, nil
	case db.AlbumsRandom:
		return albumListing{by: `random()`}, nil
	case db.AlbumsStarred:
		return albumListing{join: albumAnnotated + ` AND a.starred_at IS NOT NULL`, by: `MAX(a.starred_at) DESC` + tie}, nil
	case db.AlbumsHighest:
		return albumListing{join: albumAnnotated + ` AND a.rating > 0`, by: `MAX(a.rating) DESC` + tie}, nil
	case db.AlbumsFrequent:
		return albumListing{join: albumPlayed, having: ` AND SUM(t.play_count) > 0`,
			by: `SUM(t.play_count) DESC` + tie}, nil
	case db.AlbumsRecent:
		return albumListing{join: albumPlayed, having: ` AND MAX(t.played_at) IS NOT NULL`,
			by: `MAX(t.played_at) DESC` + tie}, nil
	default:
		return albumListing{}, fmt.Errorf("list albums: unknown order %q", o)
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
		WHERE f.owner_id = $1 AND m.kind = $2
		  AND ($3 = '' OR m.genre = $3)
		  AND ($4 = 0 OR m.year >= $4)
		  AND ($5 = 0 OR m.year <= $5)
		ORDER BY ` + order + `
		LIMIT $6 OFFSET $7`

	out, err := sqlutil.Collect(ctx, r.q, scanTrack, query,
		owner, string(db.KindAudio), f.Genre, f.FromYear, f.ToYear,
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
				THEN m.album_artist || chr(31) || m.album END)
		FROM media m JOIN files f ON f.id = m.file_id
		WHERE f.owner_id = $1 AND m.kind = $2 AND m.genre <> ''
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
		WHERE f.owner_id = $1 AND m.kind = $2 AND m.album_artist <> '' AND m.album <> ''
		  AND m.search_album_artist LIKE $3 ESCAPE '` + sqlutil.LikeEscape + `'
		GROUP BY m.album_artist
		ORDER BY m.album_artist
		LIMIT $4 OFFSET $5`

	const albums = albumSelect + `
		WHERE f.owner_id = $1 AND m.kind = $2 AND m.album <> ''
		  AND m.search_album LIKE $3 ESCAPE '` + sqlutil.LikeEscape + `'` + albumGroup + `
		ORDER BY m.album_artist, m.album
		LIMIT $4 OFFSET $5`

	tracks := `SELECT ` + joinedFileColumns + `, ` + joinedMediaColumns + `
		FROM media m JOIN files f ON f.id = m.file_id
		WHERE f.owner_id = $1 AND m.kind = $2
		  AND (m.search_song LIKE $3 ESCAPE '` + sqlutil.LikeEscape + `'
		    OR m.search_album LIKE $3 ESCAPE '` + sqlutil.LikeEscape + `'
		    OR m.search_album_artist LIKE $3 ESCAPE '` + sqlutil.LikeEscape + `')
		ORDER BY m.album_artist, m.album, m.disc_no, m.track_no, f.path
		LIMIT $4 OFFSET $5`

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
		owner, kind, term, f.Tracks.Limit, f.Tracks.Offset); err != nil {
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
	var created time.Time
	if err := rows.Scan(&a.Artist, &a.Name, &a.SongCount, &a.DurationMS,
		&a.Year, &a.Genre, &created); err != nil {
		return db.Album{}, err
	}
	a.Created = created.UTC()
	return a, nil
}

// scanTrack reads a file row and a media row from one joined result. The two
// halves are scanned the same way the single-table readers do it, because the
// conversions are a property of the columns and not of the query.
func scanTrack(rows *sql.Rows) (db.Track, error) {
	var t db.Track
	var kind string
	var lat, lon sql.NullFloat64
	var takenAt, retryAt sql.NullTime

	err := rows.Scan(
		&t.File.ID, &t.File.OwnerID, &t.File.Path, &t.File.BlobKey, &t.File.Size,
		&t.File.MTime, &t.File.ETag, &t.File.MIMEType, &t.File.IsDir,
		&t.Media.FileID, &kind, &t.Media.IndexedAt, &t.Media.Version, &t.Media.ETag, &t.Media.Error, &retryAt, &takenAt,
		&t.Media.Width, &t.Media.Height, &t.Media.Orientation, &lat, &lon, &t.Media.Camera,
		&t.Media.DurationMS, &t.Media.Codec, &t.Media.Artist, &t.Media.Album, &t.Media.Title,
		&t.Media.TrackNo, &t.Media.DiscNo, &t.Media.Year, &t.Media.Genre, &t.Media.AlbumArtist)
	if err != nil {
		return db.Track{}, err
	}

	t.File.MTime = t.File.MTime.UTC()
	t.Media.Kind = db.Kind(kind)
	t.Media.IndexedAt = t.Media.IndexedAt.UTC()
	if takenAt.Valid {
		t.Media.TakenAt = takenAt.Time.UTC()
	}
	if retryAt.Valid {
		t.Media.RetryAt = retryAt.Time.UTC()
	}
	if lat.Valid && lon.Valid {
		t.Media.GPS = &db.GPS{Latitude: lat.Float64, Longitude: lon.Float64}
	}
	return t, nil
}

// BlobKeys implements db.Repo.
func (r *repo) BlobKeys(ctx context.Context) iter.Seq2[string, error] {
	const query = `SELECT blob_key FROM files WHERE is_dir = FALSE`

	return sqlutil.Label(sqlutil.Seq(ctx, r.q, sqlutil.ScanOne[string], query), "list blob keys")
}

// MoveFile implements db.Repo.
// SubtreeSize implements db.Files.
func (r *repo) SubtreeSize(ctx context.Context, owner, dir string) (int64, error) {
	if err := db.ValidateDir(dir); err != nil {
		return 0, err
	}

	const (
		whole = `SELECT COALESCE(SUM(size), 0) FROM files WHERE owner_id = $1 AND is_dir = FALSE`
		under = `SELECT COALESCE(SUM(size), 0) FROM files
			WHERE owner_id = $1 AND is_dir = FALSE AND path >= $2 AND path < $3`
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

	const moveOne = `UPDATE files SET path = $1, parent_path = $2
		WHERE owner_id = $3 AND path = $4`

	// Everything under it, in one statement: a rewrite that took a row at a
	// time would leave the tree in a state nothing could read if it stopped
	// halfway. substr is 1-based, so the offset is the character after the old
	// prefix, and parent_path is rewritten from parent_path rather than from
	// path so the two assignments cannot depend on each other's order.
	const moveTree = `UPDATE files
		SET path = $1 || substr(path, $2), parent_path = $1 || substr(parent_path, $2)
		WHERE owner_id = $3 AND path LIKE $4 ESCAPE '` + sqlutil.LikeEscape + `'`

	result, err := r.q.ExecContext(ctx, moveOne, to, db.ParentOf(to), owner, from)
	if err != nil {
		return fmt.Errorf("move %q to %q: %w", from, to, mapErr(err))
	}
	if err := sqlutil.CheckAffected(ctx, r.q, result, from, isDirProbe, owner, from); err != nil {
		return err
	}

	if _, err := r.q.ExecContext(ctx, moveTree, to, len(from)+1, owner, sqlutil.Under(from)); err != nil {
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
		WHERE owner_id = $1 AND path = $2
		  AND NOT EXISTS (SELECT 1 FROM files child WHERE child.owner_id = $1 AND child.parent_path = $2)`

	result, err := r.q.ExecContext(ctx, query, owner, path)
	if err != nil {
		return fmt.Errorf("delete %q: %w", path, mapErr(err))
	}
	return sqlutil.CheckAffected(ctx, r.q, result, path, isDirProbe, owner, path)
}

// scanFileRow reads one row of fileColumns, for the queries that return many.
func scanFileRow(rows *sql.Rows) (db.File, error) {
	var f db.File
	if err := rows.Scan(&f.ID, &f.OwnerID, &f.Path, &f.BlobKey, &f.Size, &f.MTime, &f.ETag, &f.MIMEType, &f.IsDir); err != nil {
		return db.File{}, err
	}
	f.MTime = f.MTime.UTC()
	return f, nil
}

func scanFile(row *sql.Row) (db.File, error) {
	var f db.File
	if err := row.Scan(&f.ID, &f.OwnerID, &f.Path, &f.BlobKey, &f.Size, &f.MTime, &f.ETag, &f.MIMEType, &f.IsDir); err != nil {
		return db.File{}, mapErr(err)
	}
	f.MTime = f.MTime.UTC()
	return f, nil
}

// mapErr turns the driver's errors into the port's sentinels.
func mapErr(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return db.ErrNotFound
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case uniqueViolation:
			return fmt.Errorf("%w: %w", db.ErrConflict, err)
		case foreignKeyViolation:
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
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		ON CONFLICT (id) DO UPDATE SET
			received = excluded.received, digest = excluded.digest,
			expires_at = excluded.expires_at`

	_, err := r.q.ExecContext(ctx, query,
		u.ID, u.OwnerID, u.Path, u.Size, u.Received, u.BlobKey, u.StoreID,
		u.Digest, u.MIMEType, u.ExpiresAt,
	)
	if err != nil {
		return fmt.Errorf("put upload %q: %w", u.ID, mapErr(err))
	}
	return nil
}

// UploadByID implements db.Uploads.
func (r *repo) UploadByID(ctx context.Context, owner, id string) (db.Upload, error) {
	const query = `SELECT ` + uploadColumns + ` FROM uploads WHERE id = $1 AND owner_id = $2`

	u, err := scanUpload(r.q.QueryRowContext(ctx, query, id, owner))
	if err != nil {
		return db.Upload{}, fmt.Errorf("get upload %q: %w", id, err)
	}
	return u, nil
}

// DeleteUpload implements db.Uploads.
func (r *repo) DeleteUpload(ctx context.Context, owner, id string) error {
	const query = `DELETE FROM uploads WHERE id = $1 AND owner_id = $2`

	if _, err := r.q.ExecContext(ctx, query, id, owner); err != nil {
		return fmt.Errorf("delete upload %q: %w", id, mapErr(err))
	}
	return nil
}

// ExpiredUploads implements db.Uploads.
func (r *repo) ExpiredUploads(ctx context.Context, now time.Time) iter.Seq2[db.Upload, error] {
	const query = `SELECT ` + uploadColumns + ` FROM uploads WHERE expires_at <= $1 ORDER BY expires_at`

	return sqlutil.Label(sqlutil.Seq(ctx, r.q, scanUploadRow, query, now), "list expired uploads")
}

func scanUpload(row *sql.Row) (db.Upload, error) {
	var u db.Upload
	var expires time.Time
	if err := row.Scan(&u.ID, &u.OwnerID, &u.Path, &u.Size, &u.Received,
		&u.BlobKey, &u.StoreID, &u.Digest, &u.MIMEType, &expires); err != nil {
		return db.Upload{}, mapErr(err)
	}
	u.ExpiresAt = expires.UTC()
	return u, nil
}

func scanUploadRow(rows *sql.Rows) (db.Upload, error) {
	var u db.Upload
	var expires time.Time
	if err := rows.Scan(&u.ID, &u.OwnerID, &u.Path, &u.Size, &u.Received,
		&u.BlobKey, &u.StoreID, &u.Digest, &u.MIMEType, &expires); err != nil {
		return db.Upload{}, err
	}
	u.ExpiresAt = expires.UTC()
	return u, nil
}
