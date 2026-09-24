package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/C0piIot/stratus-backend/internal/db"
	"github.com/C0piIot/stratus-backend/internal/db/sqlutil"
)

// Every write is an upsert followed by a prune: a row is created by whichever
// of a star, a rating or a play arrives first, and deleted once it holds none
// of them. The
// prune is its own statement so that the upsert stays one, and a prune that
// failed would leave a row answering as nothing was said, which is true.
const (
	trackStar = `INSERT INTO track_annotations (file_id, owner_id, starred_at, rating) VALUES (?, ?, ?, 0)
		ON CONFLICT (file_id, owner_id) DO UPDATE SET
			starred_at = COALESCE(track_annotations.starred_at, excluded.starred_at)`
	trackRate = `INSERT INTO track_annotations (file_id, owner_id, starred_at, rating) VALUES (?, ?, NULL, ?)
		ON CONFLICT (file_id, owner_id) DO UPDATE SET rating = excluded.rating`
	trackUnstar = `UPDATE track_annotations SET starred_at = NULL WHERE file_id = ? AND owner_id = ?`
	trackPrune  = `DELETE FROM track_annotations
		WHERE file_id = ? AND owner_id = ? AND starred_at IS NULL AND rating = 0 AND play_count = 0`
	// trackPlay keeps the later of two times with the two-argument max(),
	// which is SQLite's spelling of GREATEST.
	trackPlay = `INSERT INTO track_annotations (file_id, owner_id, starred_at, rating, play_count, played_at)
		VALUES (?, ?, NULL, 0, 1, ?)
		ON CONFLICT (file_id, owner_id) DO UPDATE SET
			play_count = track_annotations.play_count + 1,
			played_at = max(COALESCE(track_annotations.played_at, excluded.played_at), excluded.played_at)`

	tagStar = `INSERT INTO tag_annotations (owner_id, kind, artist, album, starred_at, rating) VALUES (?, ?, ?, ?, ?, 0)
		ON CONFLICT (owner_id, kind, artist, album) DO UPDATE SET
			starred_at = COALESCE(tag_annotations.starred_at, excluded.starred_at)`
	tagRate = `INSERT INTO tag_annotations (owner_id, kind, artist, album, starred_at, rating) VALUES (?, ?, ?, ?, NULL, ?)
		ON CONFLICT (owner_id, kind, artist, album) DO UPDATE SET rating = excluded.rating`
	tagUnstar = `UPDATE tag_annotations SET starred_at = NULL
		WHERE owner_id = ? AND kind = ? AND artist = ? AND album = ?`
	tagPrune = `DELETE FROM tag_annotations
		WHERE owner_id = ? AND kind = ? AND artist = ? AND album = ? AND starred_at IS NULL AND rating = 0`
)

// subjectArgs is the key of s in the order every statement above names it.
func subjectArgs(owner string, s db.Subject) []any {
	if s.Kind == db.SubjectTrack {
		return []any{s.FileID, owner}
	}
	return []any{owner, string(s.Kind), s.Artist, s.Album}
}

func pick(s db.Subject, track, tag string) string {
	if s.Kind == db.SubjectTrack {
		return track
	}
	return tag
}

// Star implements db.Annotations.
func (r *repo) Star(ctx context.Context, owner string, s db.Subject, at time.Time) error {
	if err := s.Validate(); err != nil {
		return err
	}
	args := append(subjectArgs(owner, s), at.UnixMilli())
	if _, err := r.q.ExecContext(ctx, pick(s, trackStar, tagStar), args...); err != nil {
		return fmt.Errorf("star %+v: %w", s, mapErr(err))
	}
	return nil
}

// Unstar implements db.Annotations.
func (r *repo) Unstar(ctx context.Context, owner string, s db.Subject) error {
	if err := s.Validate(); err != nil {
		return err
	}
	if _, err := r.q.ExecContext(ctx, pick(s, trackUnstar, tagUnstar), subjectArgs(owner, s)...); err != nil {
		return fmt.Errorf("unstar %+v: %w", s, mapErr(err))
	}
	return r.prune(ctx, owner, s)
}

// SetRating implements db.Annotations.
func (r *repo) SetRating(ctx context.Context, owner string, s db.Subject, rating int) error {
	if err := s.Validate(); err != nil {
		return err
	}
	if err := db.ValidateRating(rating); err != nil {
		return err
	}
	args := append(subjectArgs(owner, s), rating)
	if _, err := r.q.ExecContext(ctx, pick(s, trackRate, tagRate), args...); err != nil {
		return fmt.Errorf("rate %+v: %w", s, mapErr(err))
	}
	return r.prune(ctx, owner, s)
}

// RecordPlay implements db.Annotations.
func (r *repo) RecordPlay(ctx context.Context, owner string, fileID int64, at time.Time) error {
	if err := db.TrackSubject(fileID).Validate(); err != nil {
		return err
	}
	if _, err := r.q.ExecContext(ctx, trackPlay, fileID, owner, at.UnixMilli()); err != nil {
		return fmt.Errorf("record a play of %d: %w", fileID, mapErr(err))
	}
	return nil
}

func (r *repo) prune(ctx context.Context, owner string, s db.Subject) error {
	if _, err := r.q.ExecContext(ctx, pick(s, trackPrune, tagPrune), subjectArgs(owner, s)...); err != nil {
		return fmt.Errorf("prune %+v: %w", s, mapErr(err))
	}
	return nil
}

// AnnotationsOf implements db.Annotations.
//
// Built rather than declared, like MediaStates, because the list is as long as
// what the caller is rendering: at most one query per table.
func (r *repo) AnnotationsOf(ctx context.Context, owner string, subjects []db.Subject) (map[db.Subject]db.Annotation, error) {
	out := make(map[db.Subject]db.Annotation)

	var trackArgs, tagArgs, albumArgs []any
	var tags, albums int
	for _, s := range subjects {
		switch s.Kind {
		case db.SubjectTrack:
			trackArgs = append(trackArgs, s.FileID)
		case db.SubjectAlbum:
			albumArgs = append(albumArgs, s.Artist, s.Album)
			albums++
			fallthrough
		default:
			tagArgs = append(tagArgs, string(s.Kind), s.Artist, s.Album)
			tags++
		}
	}

	if len(trackArgs) > 0 {
		query := `SELECT file_id, starred_at, rating, play_count, played_at FROM track_annotations
			WHERE owner_id = ? AND file_id IN (?` + strings.Repeat(", ?", len(trackArgs)-1) + `)`
		rows, err := sqlutil.Collect(ctx, r.q, scanTrackAnnotation, query, append([]any{owner}, trackArgs...)...)
		if err != nil {
			return nil, fmt.Errorf("read track annotations: %w", mapErr(err))
		}
		for _, row := range rows {
			out[row.subject] = row.annotation
		}
	}

	if tags > 0 {
		match := `(kind = ? AND artist = ? AND album = ?)`
		query := `SELECT kind, artist, album, starred_at, rating FROM tag_annotations
			WHERE owner_id = ? AND (` + match + strings.Repeat(" OR "+match, tags-1) + `)`
		rows, err := sqlutil.Collect(ctx, r.q, scanTagAnnotation, query, append([]any{owner}, tagArgs...)...)
		if err != nil {
			return nil, fmt.Errorf("read tag annotations: %w", mapErr(err))
		}
		for _, row := range rows {
			out[row.subject] = row.annotation
		}
	}

	// An album's plays are its tracks', added up here rather than kept on the
	// album's own row, which would be a second count to keep in step. Merged
	// into whatever the tag row said.
	if albums > 0 {
		match := `(m.album_artist = ? AND m.album = ?)`
		query := `SELECT m.album_artist, m.album, SUM(t.play_count), MAX(t.played_at)
			FROM media m JOIN files f ON f.id = m.file_id
			JOIN track_annotations t ON t.file_id = f.id AND t.owner_id = f.owner_id
			WHERE f.owner_id = ? AND m.kind = ? AND t.play_count > 0
			  AND (` + match + strings.Repeat(" OR "+match, albums-1) + `)
			GROUP BY m.album_artist, m.album`
		args := append([]any{owner, string(db.KindAudio)}, albumArgs...)
		rows, err := sqlutil.Collect(ctx, r.q, scanAlbumPlays, query, args...)
		if err != nil {
			return nil, fmt.Errorf("read album plays: %w", mapErr(err))
		}
		for _, row := range rows {
			a := out[row.subject]
			a.PlayCount, a.Played = row.annotation.PlayCount, row.annotation.Played
			out[row.subject] = a
		}
	}
	return out, nil
}

type annotationRow struct {
	subject    db.Subject
	annotation db.Annotation
}

func scanTrackAnnotation(rows *sql.Rows) (annotationRow, error) {
	var id int64
	var starred, played sql.NullInt64
	var row annotationRow
	if err := rows.Scan(&id, &starred, &row.annotation.Rating, &row.annotation.PlayCount, &played); err != nil {
		return annotationRow{}, err
	}
	row.subject = db.TrackSubject(id)
	if starred.Valid {
		row.annotation.Starred = time.UnixMilli(starred.Int64).UTC()
	}
	if played.Valid {
		row.annotation.Played = time.UnixMilli(played.Int64).UTC()
	}
	return row, nil
}

func scanAlbumPlays(rows *sql.Rows) (annotationRow, error) {
	var artist, album string
	var played int64
	var row annotationRow
	if err := rows.Scan(&artist, &album, &row.annotation.PlayCount, &played); err != nil {
		return annotationRow{}, err
	}
	row.subject = db.AlbumSubject(artist, album)
	row.annotation.Played = time.UnixMilli(played).UTC()
	return row, nil
}

func scanTagAnnotation(rows *sql.Rows) (annotationRow, error) {
	var kind, artist, album string
	var starred sql.NullInt64
	var row annotationRow
	if err := rows.Scan(&kind, &artist, &album, &starred, &row.annotation.Rating); err != nil {
		return annotationRow{}, err
	}
	row.subject = db.Subject{Kind: db.SubjectKind(kind), Artist: artist, Album: album}
	if starred.Valid {
		row.annotation.Starred = time.UnixMilli(starred.Int64).UTC()
	}
	return row, nil
}

// albumAnnotated joins an album aggregate to its annotation. One row per
// album, so it multiplies nothing the aggregate counts.
const albumAnnotated = ` JOIN tag_annotations a ON a.owner_id = f.owner_id AND a.kind = 'album'
	AND a.artist = m.album_artist AND a.album = m.album`

// albumPlayed joins each track of an album aggregate to its plays. A LEFT JOIN,
// at most one row per track: an album is listed whole, with the tracks nobody
// has played in its song count, and filtered in HAVING on what the ones played
// add up to.
const albumPlayed = ` LEFT JOIN track_annotations t ON t.file_id = f.id AND t.owner_id = f.owner_id`

// Starred implements db.Annotations.
//
// Three queries joined to the library rather than read from the annotations
// alone, so that a star on something no longer there is not listed: it waits,
// and answers again if the tags come back.
func (r *repo) Starred(ctx context.Context, owner string) (db.StarredItems, error) {
	kind := string(db.KindAudio)

	const artists = `SELECT m.album_artist, COUNT(DISTINCT m.album)
		FROM media m JOIN files f ON f.id = m.file_id
		JOIN tag_annotations a ON a.owner_id = f.owner_id AND a.kind = 'artist'
			AND a.artist = m.album_artist AND a.album = '' AND a.starred_at IS NOT NULL
		WHERE f.owner_id = ? AND m.kind = ? AND m.album_artist <> '' AND m.album <> ''
		GROUP BY m.album_artist
		ORDER BY MAX(a.starred_at) DESC, m.album_artist`

	const albums = albumSelect + albumAnnotated + ` AND a.starred_at IS NOT NULL
		WHERE f.owner_id = ? AND m.kind = ? AND m.album <> ''` + albumGroup + `
		ORDER BY MAX(a.starred_at) DESC, m.album_artist, m.album`

	tracks := `SELECT ` + joinedFileColumns + `, ` + joinedMediaColumns + `
		FROM media m JOIN files f ON f.id = m.file_id
		JOIN track_annotations t ON t.file_id = f.id AND t.owner_id = f.owner_id AND t.starred_at IS NOT NULL
		WHERE f.owner_id = ? AND m.kind = ?
		ORDER BY t.starred_at DESC, f.path`

	var out db.StarredItems
	var err error
	if out.Artists, err = sqlutil.Collect(ctx, r.q, scanArtist, artists, owner, kind); err != nil {
		return db.StarredItems{}, fmt.Errorf("list starred artists: %w", mapErr(err))
	}
	if out.Albums, err = sqlutil.Collect(ctx, r.q, scanAlbum, albums, owner, kind); err != nil {
		return db.StarredItems{}, fmt.Errorf("list starred albums: %w", mapErr(err))
	}
	if out.Tracks, err = sqlutil.Collect(ctx, r.q, scanTrack, tracks, owner, kind); err != nil {
		return db.StarredItems{}, fmt.Errorf("list starred tracks: %w", mapErr(err))
	}
	return out, nil
}
