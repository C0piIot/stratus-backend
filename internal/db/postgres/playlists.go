package postgres

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
	const query = `INSERT INTO playlists (owner_id, name, comment, public, created_at, changed_at)
		VALUES ($1, $2, $3, $4, $5, $6) RETURNING id`

	err := r.q.QueryRowContext(ctx, query, p.OwnerID, p.Name, p.Comment, p.Public,
		p.Created, p.Changed).Scan(&p.ID)
	if err != nil {
		return db.Playlist{}, fmt.Errorf("create playlist %q: %w", p.Name, mapErr(err))
	}
	p.SongCount, p.DurationMS = 0, 0
	return p, nil
}

// Playlists implements db.Playlists.
func (r *repo) Playlists(ctx context.Context, owner string) ([]db.Playlist, error) {
	const query = playlistSelect + ` WHERE p.owner_id = $1` + playlistGroup + ` ORDER BY p.name, p.id`

	out, err := sqlutil.Collect(ctx, r.q, scanPlaylist, query, owner)
	if err != nil {
		return nil, fmt.Errorf("list playlists: %w", mapErr(err))
	}
	return out, nil
}

// PlaylistByID implements db.Playlists.
func (r *repo) PlaylistByID(ctx context.Context, owner string, id int64) (db.Playlist, error) {
	const query = playlistSelect + ` WHERE p.owner_id = $1 AND p.id = $2` + playlistGroup

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
	const query = `SELECT id FROM playlists WHERE owner_id = $1 AND id = $2 FOR UPDATE`

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
		WHERE p.owner_id = $1 AND p.id = $2 AND m.kind = $3
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
	const query = `UPDATE playlists SET name = $1, comment = $2, public = $3, changed_at = $4
		WHERE owner_id = $5 AND id = $6`

	result, err := r.q.ExecContext(ctx, query, p.Name, p.Comment, p.Public, p.Changed, p.OwnerID, p.ID)
	if err != nil {
		return fmt.Errorf("update playlist %d: %w", p.ID, mapErr(err))
	}
	return affected(result, "update playlist", p.ID)
}

// entryBatch bounds one INSERT, since the placeholder list grows with it.
// PostgreSQL allows 65535 of them; three per row leaves plenty of room.
const entryBatch = 1000

// SetPlaylistTracks implements db.Playlists.
func (r *repo) SetPlaylistTracks(ctx context.Context, owner string, id int64, fileIDs []int64, changed time.Time) error {
	const touch = `UPDATE playlists SET changed_at = $1 WHERE owner_id = $2 AND id = $3`
	result, err := r.q.ExecContext(ctx, touch, changed, owner, id)
	if err != nil {
		return fmt.Errorf("touch playlist %d: %w", id, mapErr(err))
	}
	if err := affected(result, "set the tracks of playlist", id); err != nil {
		return err
	}

	if _, err := r.q.ExecContext(ctx, `DELETE FROM playlist_entries WHERE playlist_id = $1`, id); err != nil {
		return fmt.Errorf("empty playlist %d: %w", id, mapErr(err))
	}

	for start := 0; start < len(fileIDs); start += entryBatch {
		batch := fileIDs[start:min(start+entryBatch, len(fileIDs))]
		var values strings.Builder
		args := make([]any, 0, 3*len(batch))
		for i, fileID := range batch {
			if i > 0 {
				values.WriteString(", ")
			}
			n := 3 * i
			fmt.Fprintf(&values, "($%d, $%d, $%d)", n+1, n+2, n+3)
			args = append(args, id, start+i, fileID)
		}
		query := `INSERT INTO playlist_entries (playlist_id, position, file_id) VALUES ` + values.String()
		if _, err := r.q.ExecContext(ctx, query, args...); err != nil {
			return fmt.Errorf("fill playlist %d: %w", id, mapErr(err))
		}
	}
	return nil
}

// DeletePlaylist implements db.Playlists.
func (r *repo) DeletePlaylist(ctx context.Context, owner string, id int64) error {
	result, err := r.q.ExecContext(ctx, `DELETE FROM playlists WHERE owner_id = $1 AND id = $2`, owner, id)
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
	if err := rows.Scan(&p.ID, &p.OwnerID, &p.Name, &p.Comment, &p.Public, &p.Created, &p.Changed,
		&p.SongCount, &p.DurationMS); err != nil {
		return db.Playlist{}, err
	}
	p.Created, p.Changed = p.Created.UTC(), p.Changed.UTC()
	return p, nil
}
