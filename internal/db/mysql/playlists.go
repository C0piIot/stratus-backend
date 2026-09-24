package mysql

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/C0piIot/stratus-backend/internal/db"
	"github.com/C0piIot/stratus-backend/internal/db/sqlutil"
)

// playlistSelect counts over the entries PlaylistTracks lists -- the ones with
// a media row of the audio kind -- so a listing and the playlist opened from
// it agree.
const playlistSelect = `SELECT p.id, p.owner_id, p.name, p.comment, p.public, p.created_at, p.changed_at,
		COUNT(m.file_id), COALESCE(SUM(m.duration_ms), 0)
	FROM playlists p
	LEFT JOIN playlist_entries e ON e.playlist_id = p.id
	LEFT JOIN media m ON m.file_id = e.file_id AND m.kind = 'audio'`

const playlistGroup = ` GROUP BY p.id, p.owner_id, p.name, p.comment, p.public, p.created_at, p.changed_at`

// CreatePlaylist implements db.Playlists.
func (r *repo) CreatePlaylist(ctx context.Context, p db.Playlist) (db.Playlist, error) {
	p = p.Normalize()
	// No RETURNING here: the id is the one the insert reports.
	const query = `INSERT INTO playlists (owner_id, name, comment, public, created_at, changed_at)
		VALUES (?, ?, ?, ?, ?, ?)`

	result, err := r.q.ExecContext(ctx, query, p.OwnerID, p.Name, p.Comment, p.Public,
		p.Created.UnixMilli(), p.Changed.UnixMilli())
	if err != nil {
		return db.Playlist{}, fmt.Errorf("create playlist %q: %w", p.Name, mapErr(err))
	}
	if p.ID, err = result.LastInsertId(); err != nil {
		return db.Playlist{}, fmt.Errorf("create playlist %q: %w", p.Name, err)
	}
	p.SongCount, p.DurationMS = 0, 0
	return p, nil
}

// Playlists implements db.Playlists.
func (r *repo) Playlists(ctx context.Context, owner string) ([]db.Playlist, error) {
	const query = playlistSelect + ` WHERE p.owner_id = ?` + playlistGroup + ` ORDER BY p.name, p.id`

	out, err := sqlutil.Collect(ctx, r.q, scanPlaylist, query, owner)
	if err != nil {
		return nil, fmt.Errorf("list playlists: %w", mapErr(err))
	}
	return out, nil
}

// PlaylistByID implements db.Playlists.
func (r *repo) PlaylistByID(ctx context.Context, owner string, id int64) (db.Playlist, error) {
	const query = playlistSelect + ` WHERE p.owner_id = ? AND p.id = ?` + playlistGroup

	out, err := sqlutil.Collect(ctx, r.q, scanPlaylist, query, owner, id)
	if err != nil {
		return db.Playlist{}, fmt.Errorf("get playlist %d: %w", id, mapErr(err))
	}
	if len(out) == 0 {
		return db.Playlist{}, fmt.Errorf("get playlist %d: %w", id, db.ErrNotFound)
	}
	return out[0], nil
}

// LockPlaylist implements db.Playlists.
func (r *repo) LockPlaylist(ctx context.Context, owner string, id int64) error {
	const query = `SELECT id FROM playlists WHERE owner_id = ? AND id = ? FOR UPDATE`

	var got int64
	if err := r.q.QueryRowContext(ctx, query, owner, id).Scan(&got); err != nil {
		return fmt.Errorf("lock playlist %d: %w", id, mapErr(err))
	}
	return nil
}

// PlaylistTracks implements db.Playlists.
func (r *repo) PlaylistTracks(ctx context.Context, owner string, id int64) ([]db.Track, error) {
	query := `SELECT ` + joinedFileColumns + `, ` + joinedMediaColumns + `
		FROM playlist_entries e
		JOIN playlists p ON p.id = e.playlist_id
		JOIN files f ON f.id = e.file_id
		JOIN media m ON m.file_id = f.id
		WHERE p.owner_id = ? AND p.id = ? AND m.kind = ?
		ORDER BY e.position`

	out, err := sqlutil.Collect(ctx, r.q, scanTrack, query, owner, id, string(db.KindAudio))
	if err != nil {
		return nil, fmt.Errorf("list the tracks of playlist %d: %w", id, mapErr(err))
	}
	return out, nil
}

// UpdatePlaylist implements db.Playlists.
func (r *repo) UpdatePlaylist(ctx context.Context, p db.Playlist) error {
	p = p.Normalize()
	const query = `UPDATE playlists SET name = ?, comment = ?, public = ?, changed_at = ?
		WHERE owner_id = ? AND id = ?`

	result, err := r.q.ExecContext(ctx, query, p.Name, p.Comment, p.Public, p.Changed.UnixMilli(), p.OwnerID, p.ID)
	if err != nil {
		return fmt.Errorf("update playlist %d: %w", p.ID, mapErr(err))
	}
	return affected(result, "update playlist", p.ID)
}

// entryBatch bounds one INSERT, since the placeholder list grows with it.
// MySQL allows 65535 of them; three per row leaves plenty of room.
const entryBatch = 1000

// SetPlaylistTracks implements db.Playlists.
func (r *repo) SetPlaylistTracks(ctx context.Context, owner string, id int64, fileIDs []int64, changed time.Time) error {
	const touch = `UPDATE playlists SET changed_at = ? WHERE owner_id = ? AND id = ?`
	result, err := r.q.ExecContext(ctx, touch, changed.UnixMilli(), owner, id)
	if err != nil {
		return fmt.Errorf("touch playlist %d: %w", id, mapErr(err))
	}
	if err := affected(result, "set the tracks of playlist", id); err != nil {
		return err
	}

	if _, err := r.q.ExecContext(ctx, `DELETE FROM playlist_entries WHERE playlist_id = ?`, id); err != nil {
		return fmt.Errorf("empty playlist %d: %w", id, mapErr(err))
	}

	for start := 0; start < len(fileIDs); start += entryBatch {
		batch := fileIDs[start:min(start+entryBatch, len(fileIDs))]
		query := `INSERT INTO playlist_entries (playlist_id, position, file_id) VALUES (?, ?, ?)` +
			strings.Repeat(", (?, ?, ?)", len(batch)-1)
		args := make([]any, 0, 3*len(batch))
		for i, fileID := range batch {
			args = append(args, id, start+i, fileID)
		}
		if _, err := r.q.ExecContext(ctx, query, args...); err != nil {
			return fmt.Errorf("fill playlist %d: %w", id, mapErr(err))
		}
	}
	return nil
}

// DeletePlaylist implements db.Playlists.
func (r *repo) DeletePlaylist(ctx context.Context, owner string, id int64) error {
	result, err := r.q.ExecContext(ctx, `DELETE FROM playlists WHERE owner_id = ? AND id = ?`, owner, id)
	if err != nil {
		return fmt.Errorf("delete playlist %d: %w", id, mapErr(err))
	}
	return affected(result, "delete playlist", id)
}

// affected is ErrNotFound for a statement that matched no playlist.
func affected(result sql.Result, what string, id int64) error {
	n, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("%s %d: %w", what, id, err)
	}
	if n == 0 {
		return fmt.Errorf("%s %d: %w", what, id, db.ErrNotFound)
	}
	return nil
}

func scanPlaylist(rows *sql.Rows) (db.Playlist, error) {
	var p db.Playlist
	var created, changed int64
	if err := rows.Scan(&p.ID, &p.OwnerID, &p.Name, &p.Comment, &p.Public, &created, &changed,
		&p.SongCount, &p.DurationMS); err != nil {
		return db.Playlist{}, err
	}
	p.Created = time.UnixMilli(created).UTC()
	p.Changed = time.UnixMilli(changed).UTC()
	return p, nil
}
