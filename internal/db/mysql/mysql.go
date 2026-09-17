// Package mysql implements the metadata port on MySQL, through the pure-Go
// go-sql-driver/mysql so the binary stays CGO-free and the image stays
// distroless.
//
// It targets MySQL 8.0.19 or newer, for the row alias an upsert needs here.
// MariaDB is a different database wearing the same name -- it has RETURNING,
// which would make half of this file simpler -- and is not what this is tested
// against.
//
// Three things are genuinely different rather than differently spelled, and
// each is commented where it bites: a path cannot be indexed, so uniqueness
// rides on a hash column; there is no RETURNING, so an insert is followed by a
// read; and a subquery in UPDATE or DELETE may not name the table being
// written, so the guard against emptying a directory goes through a derived
// table.
package mysql

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"iter"
	"net"
	"net/url"
	"strings"
	"time"

	mysqldriver "github.com/go-sql-driver/mysql"

	"github.com/C0piIot/stratus-backend/internal/db"
	"github.com/C0piIot/stratus-backend/internal/db/sqlutil"
)

//go:embed migrations/*.sql
var migrations embed.FS

// duplicateEntry is ER_DUP_ENTRY, which covers the primary key and the unique
// index on (owner_id, path_hash) alike.
const duplicateEntry = 1062

// Connection parameters this package sets rather than taking from the operator,
// for the same reason the SQLite driver sets its pragmas: neither is a
// preference.
//
//   - clientFoundRows, so an UPDATE reports the rows it matched and not the
//     rows it changed. Without it, renaming a file to the name it already has
//     affects no rows and sqlutil.CheckAffected reads that as "no such file".
//   - collation, because the server default is accent- and case-insensitive.
//     Under it, Photo.jpg and phóto.jpg are one row in a unique index; binary
//     is what the other two drivers and every filesystem do.
var params = map[string]string{
	"clientFoundRows": "true",
	"collation":       "utf8mb4_0900_bin",
}

// Store is a db.Store backed by a MySQL database.
type Store struct {
	*repo
	db *sql.DB
}

var _ db.Store = (*Store)(nil)

// hash is what the unique index is really on. MySQL cannot index a path: InnoDB
// stops at 3072 bytes and a path is up to MaxPathLen, and a prefix index would
// make two paths sharing their first characters collide as duplicates -- which
// for an upsert means one silently replacing the other.
func hash(path string) []byte {
	sum := sha256.Sum256([]byte(path))
	return sum[:]
}

// New connects to the database described by dsn, which is a URL --
// mysql://user:pass@host:3306/database -- and not the driver's own
// user:pass@tcp(host)/database syntax. One grammar for every driver is what
// internal/config hands out, and translating it is this package's job rather
// than an operator's.
func New(ctx context.Context, dsn string) (*Store, error) {
	driverDSN, err := DSN(dsn)
	if err != nil {
		return nil, err
	}
	sqlDB, err := sql.Open("mysql", driverDSN)
	if err != nil {
		return nil, fmt.Errorf("mysql: open: %w", err)
	}
	if err := sqlDB.PingContext(ctx); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("mysql: connect: %w", err)
	}
	return &Store{repo: &repo{q: sqlDB}, db: sqlDB}, nil
}

// DSN translates the URL internal/config hands out into the one the driver
// speaks. Exported because it is the only part of this package a test can need
// without a Store: opening a connection to the server itself, to make the
// database a case will then connect to.
func DSN(raw string) (string, error) {
	if raw == "" {
		return "", errors.New("mysql: dsn is required")
	}
	u, err := url.Parse(raw)
	if err != nil {
		// The DSN carries a password, so it is never in the message.
		return "", errors.New("mysql: dsn is not a valid URL")
	}

	cfg := mysqldriver.NewConfig()
	cfg.Params = map[string]string{}
	cfg.Net = "tcp"
	cfg.Addr = u.Host
	if u.Port() == "" {
		cfg.Addr = net.JoinHostPort(u.Hostname(), "3306")
	}
	cfg.DBName = strings.TrimPrefix(u.Path, "/")
	if u.User != nil {
		cfg.User = u.User.Username()
		cfg.Passwd, _ = u.User.Password()
	}
	for k, v := range u.Query() {
		cfg.Params[k] = v[0]
	}
	// Applied after the operator's parameters rather than before: these are
	// correctness, and a DSN that quietly turned one off would be a bug nobody
	// could see. The two the driver keeps in fields of its own are set there,
	// since a Params entry alone would not take.
	for k, v := range params {
		cfg.Params[k] = v
	}
	cfg.ClientFoundRows = true
	cfg.Collation = params["collation"]
	delete(cfg.Params, "clientFoundRows")
	delete(cfg.Params, "collation")

	return cfg.FormatDSN(), nil
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
const isDirProbe = `SELECT is_dir FROM files WHERE owner_id = ? AND path_hash = ?`

const fileColumns = `id, owner_id, path, blob_key, size, mtime, etag, mime_type, is_dir`

// PutFile implements db.Repo.
func (r *repo) PutFile(ctx context.Context, f db.File) (db.File, error) {
	if err := db.ValidatePath(f.Path); err != nil {
		return db.File{}, err
	}
	f = f.Normalize()

	// Two statements where the other drivers need one, and both halves are
	// MySQL: ON DUPLICATE KEY UPDATE takes no WHERE, so a file must be stopped
	// from replacing a directory by leaving every column as it was -- that is
	// what the IFs do -- and there is no RETURNING, so the id comes from a read
	// that also reports whether the row it hit was a directory after all.
	const upsert = `INSERT INTO files (owner_id, path, path_hash, parent_path, blob_key, size, mtime, etag, mime_type)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?) AS new
		ON DUPLICATE KEY UPDATE
			blob_key  = IF(files.is_dir, files.blob_key, new.blob_key),
			size      = IF(files.is_dir, files.size, new.size),
			mtime     = IF(files.is_dir, files.mtime, new.mtime),
			etag      = IF(files.is_dir, files.etag, new.etag),
			mime_type = IF(files.is_dir, files.mime_type, new.mime_type)`

	const stored = `SELECT id, is_dir FROM files WHERE owner_id = ? AND path_hash = ?`

	key := hash(f.Path)
	if _, err := r.q.ExecContext(ctx, upsert,
		f.OwnerID, f.Path, key, db.ParentOf(f.Path), f.BlobKey, f.Size,
		f.MTime.UnixMilli(), f.ETag, f.MIMEType,
	); err != nil {
		return db.File{}, fmt.Errorf("put %q: %w", f.Path, mapErr(err))
	}

	var isDir bool
	if err := r.q.QueryRowContext(ctx, stored, f.OwnerID, key).Scan(&f.ID, &isDir); err != nil {
		return db.File{}, fmt.Errorf("put %q: %w", f.Path, mapErr(err))
	}
	if isDir {
		// The upsert declined to change anything, which the IFs above only do
		// for a directory: a file must not replace a collection.
		return db.File{}, fmt.Errorf("put %q: %w: it is a directory", f.Path, db.ErrConflict)
	}
	return f, nil
}

// CreateDir implements db.Repo.
func (r *repo) CreateDir(ctx context.Context, owner, path string) (db.File, error) {
	if err := db.ValidatePath(path); err != nil {
		return db.File{}, err
	}

	const query = `INSERT INTO files (owner_id, path, path_hash, parent_path, blob_key, size, mtime, etag, mime_type, is_dir)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 1)`

	dir := db.File{OwnerID: owner, Path: path, MTime: time.Now(), IsDir: true}.Normalize()
	// A plain insert, so unlike the upsert above the id is the one the server
	// just generated and LastInsertId is the whole of RETURNING here.
	result, err := r.q.ExecContext(ctx, query,
		dir.OwnerID, dir.Path, hash(dir.Path), db.ParentOf(dir.Path), "", 0,
		dir.MTime.UnixMilli(), "", "",
	)
	if err != nil {
		return db.File{}, fmt.Errorf("create directory %q: %w", path, mapErr(err))
	}
	if dir.ID, err = result.LastInsertId(); err != nil {
		return db.File{}, fmt.Errorf("create directory %q: %w", path, err)
	}
	return dir, nil
}

// FileByPath implements db.Repo.
func (r *repo) FileByPath(ctx context.Context, owner, path string) (db.File, error) {
	if err := db.ValidatePath(path); err != nil {
		return db.File{}, err
	}
	const query = `SELECT ` + fileColumns + ` FROM files WHERE owner_id = ? AND path_hash = ?`

	f, err := scanFile(r.q.QueryRowContext(ctx, query, owner, hash(path)))
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

const mediaColumns = `file_id, kind, indexed_at, version, error, taken_at, width, height, orientation, latitude, longitude, camera, duration_ms, codec, artist, album, title, track_no, disc_no, year, genre, album_artist`

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

	var lat, lon any
	if m.GPS != nil {
		lat, lon = m.GPS.Latitude, m.GPS.Longitude
	}

	const query = `INSERT INTO media (` + mediaWriteColumns + `)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		AS new
		ON DUPLICATE KEY UPDATE
			kind = new.kind, indexed_at = new.indexed_at, version = new.version,
			error = new.error, taken_at = new.taken_at, width = new.width,
			height = new.height, orientation = new.orientation,
			latitude = new.latitude, longitude = new.longitude, camera = new.camera,
			duration_ms = new.duration_ms, codec = new.codec, artist = new.artist,
			album = new.album, title = new.title, track_no = new.track_no,
			disc_no = new.disc_no, year = new.year, genre = new.genre,
			album_artist = new.album_artist, search_song = new.search_song,
			search_album = new.search_album,
			search_album_artist = new.search_album_artist`

	_, err := r.q.ExecContext(ctx, query,
		m.FileID, string(m.Kind), m.IndexedAt.UnixMilli(), m.Version, m.Error, takenAt,
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
		&m.TrackNo, &m.DiscNo, &m.Year, &m.Genre, &m.AlbumArtist)
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
		return `RAND()`, nil
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
		return `RAND()`, nil
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
				THEN CONCAT(m.album_artist, CHAR(31 USING utf8mb4), m.album) END)
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

// likeEscape is sqlutil.LikeEscape written the way MySQL parses a string
// literal: a backslash is itself an escape there, so the one character
// sqlutil.Contains puts in front of a wildcard has to be spelled with two.
const likeEscape = `\\`

// Search implements db.Repo.
//
// Three queries, and every one of them matches a column this process folded
// rather than calling lower() here: SQLite folds ASCII, PostgreSQL folds
// Unicode and MySQL folds whatever its collation says -- and this schema pins a
// binary one, so it would fold nothing at all. Letting the engine decide would
// make three drivers give three answers for an accented capital.
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
		  AND m.search_album_artist LIKE ? ESCAPE '` + likeEscape + `'
		GROUP BY m.album_artist
		ORDER BY m.album_artist
		LIMIT ? OFFSET ?`

	const albums = albumSelect + `
		WHERE f.owner_id = ? AND m.kind = ? AND m.album <> ''
		  AND m.search_album LIKE ? ESCAPE '` + likeEscape + `'` + albumGroup + `
		ORDER BY m.album_artist, m.album
		LIMIT ? OFFSET ?`

	tracks := `SELECT ` + joinedFileColumns + `, ` + joinedMediaColumns + `
		FROM media m JOIN files f ON f.id = m.file_id
		WHERE f.owner_id = ? AND m.kind = ?
		  AND (m.search_song LIKE ? ESCAPE '` + likeEscape + `'
		    OR m.search_album LIKE ? ESCAPE '` + likeEscape + `'
		    OR m.search_album_artist LIKE ? ESCAPE '` + likeEscape + `')
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
	var takenAt sql.NullInt64

	err := rows.Scan(
		&t.File.ID, &t.File.OwnerID, &t.File.Path, &t.File.BlobKey, &t.File.Size,
		&mtime, &t.File.ETag, &t.File.MIMEType, &t.File.IsDir,
		&t.Media.FileID, &kind, &indexedAt, &t.Media.Version, &t.Media.Error, &takenAt,
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
func (r *repo) MoveFile(ctx context.Context, owner, from, to string) error {
	if err := db.ValidateMove(from, to); err != nil {
		return err
	}
	const moveOne = `UPDATE files SET path = ?, path_hash = ?, parent_path = ?
		WHERE owner_id = ? AND path_hash = ?`

	// Everything under it, in one statement. Two things here are MySQL's and
	// not the other drivers': SUBSTRING rather than substr, and the hash being
	// recomputed from the column that was just assigned -- MySQL evaluates a
	// SET left to right and a later assignment sees the earlier one, which is
	// the documented behaviour this relies on rather than a coincidence. The
	// digest has to match what hash() computes in Go, and SHA2 over the same
	// utf8mb4 bytes does.
	const moveTree = `UPDATE files
		SET path = CONCAT(?, SUBSTRING(path, ?)),
		    path_hash = UNHEX(SHA2(path, 256)),
		    parent_path = CONCAT(?, SUBSTRING(parent_path, ?))
		WHERE owner_id = ? AND path LIKE ? ESCAPE '` + likeEscape + `'`

	result, err := r.q.ExecContext(ctx, moveOne, to, hash(to), db.ParentOf(to), owner, hash(from))
	if err != nil {
		return fmt.Errorf("move %q to %q: %w", from, to, mapErr(err))
	}
	if err := sqlutil.CheckAffected(ctx, r.q, result, from, isDirProbe, owner, hash(from)); err != nil {
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
	// Same derived table, and the same reason, as MoveFile.
	const query = `DELETE FROM files
		WHERE owner_id = ? AND path_hash = ?
		  AND NOT EXISTS (
			SELECT 1 FROM (
				SELECT 1 FROM files child
				WHERE child.owner_id = ? AND child.parent_path = ? LIMIT 1
			) AS occupied)`

	result, err := r.q.ExecContext(ctx, query, owner, hash(path), owner, path)
	if err != nil {
		return fmt.Errorf("delete %q: %w", path, mapErr(err))
	}
	return sqlutil.CheckAffected(ctx, r.q, result, path, isDirProbe, owner, hash(path))
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
	var merr *mysqldriver.MySQLError
	if errors.As(err, &merr) && merr.Number == duplicateEntry {
		return fmt.Errorf("%w: %w", db.ErrConflict, err)
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
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?) AS new
		ON DUPLICATE KEY UPDATE
			received = new.received, digest = new.digest,
			expires_at = new.expires_at`

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
