package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/C0piIot/stratus-backend/internal/db"
	"github.com/C0piIot/stratus-backend/internal/db/sqlutil"
)

// The queue is a query, like the indexer's (#217): an album with no row, or
// one whose row says to ask again by now. Albums are the GROUP BY the library
// browses by, so the ids come from whichever of the tracks carry one -- MAX
// over the group, since an empty string sorts before any id.
const pendingAlbumLookups = `SELECT f.owner_id, m.album_artist, m.album, MAX(m.mb_release_group), MAX(m.mb_release)
	FROM media m JOIN files f ON f.id = m.file_id
	LEFT JOIN album_lookups l ON l.owner_id = f.owner_id AND l.artist = m.album_artist AND l.album = m.album
	WHERE m.kind = $1 AND m.album <> '' AND m.album_artist <> ''
	  AND (l.owner_id IS NULL OR (l.retry_at IS NOT NULL AND l.retry_at <= $2))
	GROUP BY f.owner_id, m.album_artist, m.album
	ORDER BY f.owner_id, m.album_artist, m.album
	LIMIT $3`

// PendingAlbumLookups implements db.Lookups.
func (r *repo) PendingAlbumLookups(ctx context.Context, now time.Time, limit int) ([]db.AlbumCandidate, error) {
	out, err := sqlutil.Collect(ctx, r.q, scanAlbumCandidate, pendingAlbumLookups, string(db.KindAudio), now, limit)
	if err != nil {
		return nil, fmt.Errorf("list pending album lookups: %w", mapErr(err))
	}
	return out, nil
}

func scanAlbumCandidate(rows *sql.Rows) (db.AlbumCandidate, error) {
	var c db.AlbumCandidate
	err := rows.Scan(&c.Owner, &c.Artist, &c.Album, &c.ReleaseGroup, &c.Release)
	return c, err
}

// PutAlbumLookup implements db.Lookups.
func (r *repo) PutAlbumLookup(ctx context.Context, l db.AlbumLookup) error {
	const query = `INSERT INTO album_lookups (owner_id, artist, album, looked_up_at, retry_at, release_group, cover_blob, error)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (owner_id, artist, album) DO UPDATE SET
			looked_up_at = excluded.looked_up_at, retry_at = excluded.retry_at,
			release_group = excluded.release_group, cover_blob = excluded.cover_blob, error = excluded.error`

	var retryAt any
	if !l.RetryAt.IsZero() {
		retryAt = l.RetryAt
	}
	_, err := r.q.ExecContext(ctx, query, l.Owner, l.Artist, l.Album, l.LookedUpAt, retryAt,
		l.ReleaseGroup, l.CoverBlob, l.Error)
	if err != nil {
		return fmt.Errorf("put album lookup for %q / %q: %w", l.Artist, l.Album, mapErr(err))
	}
	return nil
}

const albumLookupColumns = `owner_id, artist, album, looked_up_at, retry_at, release_group, cover_blob, error`

// AlbumLookup implements db.Lookups.
func (r *repo) AlbumLookup(ctx context.Context, owner, artist, album string) (db.AlbumLookup, error) {
	const query = `SELECT ` + albumLookupColumns + ` FROM album_lookups WHERE owner_id = $1 AND artist = $2 AND album = $3`

	var l db.AlbumLookup
	var retryAt sql.NullTime
	err := r.q.QueryRowContext(ctx, query, owner, artist, album).
		Scan(&l.Owner, &l.Artist, &l.Album, &l.LookedUpAt, &retryAt, &l.ReleaseGroup, &l.CoverBlob, &l.Error)
	if err != nil {
		return db.AlbumLookup{}, fmt.Errorf("read album lookup for %q / %q: %w", artist, album, mapErr(err))
	}
	l.LookedUpAt = l.LookedUpAt.UTC()
	if retryAt.Valid {
		l.RetryAt = retryAt.Time.UTC()
	}
	return l, nil
}

// ForgetCover implements db.Lookups.
func (r *repo) ForgetCover(ctx context.Context, owner, artist, album string, retryAt time.Time) error {
	const query = `UPDATE album_lookups SET cover_blob = '', retry_at = $1
		WHERE owner_id = $2 AND artist = $3 AND album = $4`
	if _, err := r.q.ExecContext(ctx, query, retryAt, owner, artist, album); err != nil {
		return fmt.Errorf("forget the cover of %q / %q: %w", artist, album, mapErr(err))
	}
	return nil
}

// PruneAlbumLookups implements db.Lookups.
//
// The NOT EXISTS is a seek on media_kind_artist_album per row, and the table
// has one row per album, so this is cheap enough to run on every pass.
func (r *repo) PruneAlbumLookups(ctx context.Context) (int64, error) {
	const query = `DELETE FROM album_lookups WHERE NOT EXISTS (
		SELECT 1 FROM media m JOIN files f ON f.id = m.file_id
		WHERE f.owner_id = album_lookups.owner_id AND m.kind = $1
		  AND m.album_artist = album_lookups.artist AND m.album = album_lookups.album)`
	n, err := sqlutil.Affected(ctx, r.q, query, string(db.KindAudio))
	if err != nil {
		return 0, fmt.Errorf("prune album lookups: %w", mapErr(err))
	}
	return n, nil
}

// PendingLyrics implements db.Lookups.
//
// The same shape as PendingMedia, with the etag doing the same job: a replaced
// file keeps its id, and the words were looked up by the tags the old bytes
// carried.
func (r *repo) PendingLyrics(ctx context.Context, now time.Time, limit int) ([]db.Track, error) {
	query := `SELECT ` + joinedFileColumns + `, ` + joinedMediaColumns + `
		FROM media m JOIN files f ON f.id = m.file_id
		LEFT JOIN lyrics y ON y.file_id = f.id
		WHERE m.kind = $1 AND m.title <> '' AND m.artist <> ''
		  AND (y.file_id IS NULL OR y.etag <> f.etag OR (y.retry_at IS NOT NULL AND y.retry_at <= $2))
		ORDER BY f.id LIMIT $3`

	out, err := sqlutil.Collect(ctx, r.q, scanTrack, query, string(db.KindAudio), now, limit)
	if err != nil {
		return nil, fmt.Errorf("list pending lyrics: %w", mapErr(err))
	}
	return out, nil
}

// PutLyrics implements db.Lookups.
func (r *repo) PutLyrics(ctx context.Context, l db.Lyrics) error {
	const query = `INSERT INTO lyrics (file_id, etag, fetched_at, retry_at, plain, synced, instrumental, error)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (file_id) DO UPDATE SET
			etag = excluded.etag, fetched_at = excluded.fetched_at, retry_at = excluded.retry_at,
			plain = excluded.plain, synced = excluded.synced, instrumental = excluded.instrumental,
			error = excluded.error`

	var retryAt any
	if !l.RetryAt.IsZero() {
		retryAt = l.RetryAt
	}
	_, err := r.q.ExecContext(ctx, query, l.FileID, l.ETag, l.FetchedAt, retryAt,
		l.Plain, l.Synced, l.Instrumental, l.Error)
	if err != nil {
		return fmt.Errorf("put lyrics for file %d: %w", l.FileID, mapErr(err))
	}
	return nil
}

// LyricsOf implements db.Lookups.
func (r *repo) LyricsOf(ctx context.Context, fileIDs []int64) (map[int64]db.Lyrics, error) {
	out := make(map[int64]db.Lyrics, len(fileIDs))
	if len(fileIDs) == 0 {
		return out, nil
	}
	marks := make([]string, len(fileIDs))
	args := make([]any, len(fileIDs))
	for i, id := range fileIDs {
		marks[i] = "$" + strconv.Itoa(i+1)
		args[i] = id
	}
	query := `SELECT file_id, etag, fetched_at, retry_at, plain, synced, instrumental, error
		FROM lyrics WHERE file_id IN (` + strings.Join(marks, ", ") + `)`
	rows, err := sqlutil.Collect(ctx, r.q, scanLyrics, query, args...)
	if err != nil {
		return nil, fmt.Errorf("read lyrics: %w", mapErr(err))
	}
	for _, l := range rows {
		out[l.FileID] = l
	}
	return out, nil
}

func scanLyrics(rows *sql.Rows) (db.Lyrics, error) {
	var l db.Lyrics
	var retryAt sql.NullTime
	err := rows.Scan(&l.FileID, &l.ETag, &l.FetchedAt, &retryAt, &l.Plain, &l.Synced, &l.Instrumental, &l.Error)
	if err != nil {
		return db.Lyrics{}, err
	}
	l.FetchedAt = l.FetchedAt.UTC()
	if retryAt.Valid {
		l.RetryAt = retryAt.Time.UTC()
	}
	return l, nil
}

// LookupCounts implements db.Lookups.
func (r *repo) LookupCounts(ctx context.Context) (db.LookupCounts, error) {
	const albums = `SELECT COUNT(*),
			COUNT(l.owner_id),
			COUNT(CASE WHEN l.release_group <> '' THEN 1 END),
			COUNT(CASE WHEN l.cover_blob <> '' THEN 1 END)
		FROM (SELECT f.owner_id, m.album_artist, m.album
			FROM media m JOIN files f ON f.id = m.file_id
			WHERE m.kind = $1 AND m.album <> '' AND m.album_artist <> ''
			GROUP BY f.owner_id, m.album_artist, m.album) a
		LEFT JOIN album_lookups l ON l.owner_id = a.owner_id AND l.artist = a.album_artist AND l.album = a.album`
	const tracks = `SELECT COUNT(*),
			COUNT(y.file_id),
			COUNT(CASE WHEN y.plain <> '' OR y.synced <> '' OR y.instrumental THEN 1 END)
		FROM media m JOIN files f ON f.id = m.file_id
		LEFT JOIN lyrics y ON y.file_id = f.id
		WHERE m.kind = $1 AND m.title <> '' AND m.artist <> ''`

	var c db.LookupCounts
	kind := string(db.KindAudio)
	if err := r.q.QueryRowContext(ctx, albums, kind).Scan(&c.Albums, &c.AlbumsTried, &c.Identified, &c.Covers); err != nil {
		return db.LookupCounts{}, fmt.Errorf("count album lookups: %w", mapErr(err))
	}
	if err := r.q.QueryRowContext(ctx, tracks, kind).Scan(&c.Tracks, &c.TracksTried, &c.Lyrics); err != nil {
		return db.LookupCounts{}, fmt.Errorf("count lyrics: %w", mapErr(err))
	}
	return c, nil
}
