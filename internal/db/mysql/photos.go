package mysql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/C0piIot/stratus-backend/internal/db"
	"github.com/C0piIot/stratus-backend/internal/db/sqlutil"
)

// photoSelect is a photo: both halves and the time it is listed by. Every
// query here orders by (sort_at, file_id), which media_kind_sort is built in,
// so a page is a seek and a month is a range of it.
var photoSelect = `SELECT ` + joinedFileColumns + `, ` + joinedMediaColumns + `, m.sort_at
	FROM media m JOIN files f ON f.id = m.file_id`

// PhotoTimeline implements db.Photos.
//
// The clauses are added only when used, rather than written once with a
// "? = 0 OR" in front of each: a condition the planner cannot rule out before
// it runs is one it will not seek on, and a page would then walk everything
// newer than itself -- an offset in all but name.
func (r *repo) PhotoTimeline(ctx context.Context, owner string, f db.PhotoFilter) ([]db.Photo, error) {
	if err := db.ValidateLimit(f.Limit); err != nil {
		return nil, err
	}
	query := photoSelect + ` WHERE m.kind = ? AND f.owner_id = ?`
	args := []any{string(db.KindImage), owner}
	if !f.After.AtStart() {
		at := f.After.At.UnixMilli()
		// Not a row comparison, which MySQL answers correctly and without
		// seeking: the first clause is the bound the index can use, and the
		// second breaks the tie inside it.
		query += ` AND m.sort_at <= ? AND (m.sort_at < ? OR m.file_id < ?)`
		args = append(args, at, at, f.After.FileID)
	}
	if !f.From.IsZero() {
		query += ` AND m.sort_at >= ?`
		args = append(args, f.From.UnixMilli())
	}
	if !f.To.IsZero() {
		query += ` AND m.sort_at < ?`
		args = append(args, f.To.UnixMilli())
	}
	query += ` ORDER BY m.sort_at DESC, m.file_id DESC LIMIT ?`
	args = append(args, f.Limit)

	out, err := sqlutil.Collect(ctx, r.q, scanPhoto, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list photos: %w", mapErr(err))
	}
	return out, nil
}

// PhotoAround implements db.Photos: the photo, then one seek in each direction
// from it.
func (r *repo) PhotoAround(ctx context.Context, owner string, fileID int64) (db.PhotoAround, error) {
	one := photoSelect + ` WHERE m.kind = ? AND f.owner_id = ? AND m.file_id = ?`
	got, err := sqlutil.Collect(ctx, r.q, scanPhoto, one, string(db.KindImage), owner, fileID)
	if err != nil {
		return db.PhotoAround{}, fmt.Errorf("get photo %d: %w", fileID, mapErr(err))
	}
	if len(got) == 0 {
		return db.PhotoAround{}, fmt.Errorf("get photo %d: %w", fileID, db.ErrNotFound)
	}
	around := db.PhotoAround{Photo: got[0]}
	at := around.Photo.SortAt.UnixMilli()

	newer := photoSelect + `
		WHERE m.kind = ? AND f.owner_id = ? AND m.sort_at >= ? AND (m.sort_at > ? OR m.file_id > ?)
		ORDER BY m.sort_at, m.file_id LIMIT 1`
	older := photoSelect + `
		WHERE m.kind = ? AND f.owner_id = ? AND m.sort_at <= ? AND (m.sort_at < ? OR m.file_id < ?)
		ORDER BY m.sort_at DESC, m.file_id DESC LIMIT 1`

	for _, side := range []struct {
		query string
		into  **db.Photo
	}{{newer, &around.Newer}, {older, &around.Older}} {
		got, err := sqlutil.Collect(ctx, r.q, scanPhoto, side.query, string(db.KindImage), owner, at, at, fileID)
		if err != nil {
			return db.PhotoAround{}, fmt.Errorf("get the neighbours of photo %d: %w", fileID, mapErr(err))
		}
		if len(got) > 0 {
			*side.into = &got[0]
		}
	}
	return around, nil
}

// PhotoMonths implements db.Photos.
//
// One seek per month rather than a GROUP BY: grouping reads every photo the
// owner has to find a hundred months in them, measured at half a second over a
// hundred thousand, while asking the index for the newest photo before the
// month just found is a handful of milliseconds for the same answer.
func (r *repo) PhotoMonths(ctx context.Context, owner string) ([]db.PhotoMonth, error) {
	const (
		newest = `SELECT m.sort_at FROM media m JOIN files f ON f.id = m.file_id
			WHERE m.kind = ? AND f.owner_id = ? ORDER BY m.sort_at DESC LIMIT 1`
		before = `SELECT m.sort_at FROM media m JOIN files f ON f.id = m.file_id
			WHERE m.kind = ? AND f.owner_id = ? AND m.sort_at < ? ORDER BY m.sort_at DESC LIMIT 1`
	)
	var out []db.PhotoMonth
	query, args := newest, []any{string(db.KindImage), owner}
	for {
		var at int64
		err := r.q.QueryRowContext(ctx, query, args...).Scan(&at)
		if errors.Is(err, sql.ErrNoRows) {
			return out, nil
		}
		if err != nil {
			return nil, fmt.Errorf("list photo months: %w", mapErr(err))
		}
		month := db.MonthOf(time.UnixMilli(at))
		out = append(out, month)
		query, args = before, []any{string(db.KindImage), owner, month.Start().UnixMilli()}
	}
}

func scanPhoto(rows *sql.Rows) (db.Photo, error) {
	var at int64
	t, err := scanTrackWith(rows, &at)
	if err != nil {
		return db.Photo{}, err
	}
	return db.Photo{Track: t, SortAt: time.UnixMilli(at).UTC()}, nil
}
