package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/C0piIot/stratus-backend/internal/db"
	"github.com/C0piIot/stratus-backend/internal/db/sqlutil"
)

// captureSelect is a photo: both halves and the time it is listed by. Every
// query here orders by (sort_at, file_id), which media_kind_sort is built in,
// so a page is a seek and a month is a range of it.
var captureSelect = `SELECT ` + joinedFileColumns + `, ` + joinedMediaColumns + `, m.sort_at
	FROM media m JOIN files f ON f.id = m.file_id`

// Timeline implements db.Captures.
//
// The clauses are added only when used, rather than written once with a
// "? = 0 OR" in front of each: a condition the planner cannot rule out before
// it runs is one it will not seek on, and a page would then walk everything
// newer than itself -- an offset in all but name.
func (r *repo) Timeline(ctx context.Context, owner string, f db.CaptureFilter) ([]db.Capture, error) {
	if err := db.ValidateLimit(f.Limit); err != nil {
		return nil, err
	}
	args := []any{string(f.Kind), owner}
	next := func(v any) string {
		args = append(args, v)
		return "$" + strconv.Itoa(len(args))
	}
	query := captureSelect + ` WHERE m.kind = $1 AND f.owner_id = $2`
	if !f.After.AtStart() {
		at, id := next(f.After.At), next(f.After.FileID)
		query += ` AND (m.sort_at, m.file_id) < (` + at + `, ` + id + `)`
	}
	if !f.From.IsZero() {
		query += ` AND m.sort_at >= ` + next(f.From)
	}
	if !f.To.IsZero() {
		query += ` AND m.sort_at < ` + next(f.To)
	}
	query += ` ORDER BY m.sort_at DESC, m.file_id DESC LIMIT ` + next(f.Limit)

	out, err := sqlutil.Collect(ctx, r.q, scanCapture, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list photos: %w", mapErr(err))
	}
	return out, nil
}

// Around implements db.Captures: the photo, then one seek in each direction
// from it.
func (r *repo) Around(ctx context.Context, owner string, kind db.Kind, fileID int64) (db.Around, error) {
	one := captureSelect + ` WHERE m.kind = $1 AND f.owner_id = $2 AND m.file_id = $3`
	got, err := sqlutil.Collect(ctx, r.q, scanCapture, one, string(kind), owner, fileID)
	if err != nil {
		return db.Around{}, fmt.Errorf("get photo %d: %w", fileID, mapErr(err))
	}
	if len(got) == 0 {
		return db.Around{}, fmt.Errorf("get photo %d: %w", fileID, db.ErrNotFound)
	}
	around := db.Around{Capture: got[0]}
	at := around.Capture.SortAt

	newer := captureSelect + `
		WHERE m.kind = $1 AND f.owner_id = $2 AND (m.sort_at, m.file_id) > ($3, $4)
		ORDER BY m.sort_at, m.file_id LIMIT 1`
	older := captureSelect + `
		WHERE m.kind = $1 AND f.owner_id = $2 AND (m.sort_at, m.file_id) < ($3, $4)
		ORDER BY m.sort_at DESC, m.file_id DESC LIMIT 1`

	for _, side := range []struct {
		query string
		into  **db.Capture
	}{{newer, &around.Newer}, {older, &around.Older}} {
		got, err := sqlutil.Collect(ctx, r.q, scanCapture, side.query, string(kind), owner, at, fileID)
		if err != nil {
			return db.Around{}, fmt.Errorf("get the neighbours of photo %d: %w", fileID, mapErr(err))
		}
		if len(got) > 0 {
			*side.into = &got[0]
		}
	}
	return around, nil
}

// Months implements db.Captures.
//
// One seek per month rather than a GROUP BY: grouping reads every photo the
// owner has to find a hundred months in them, measured at half a second over a
// hundred thousand, while asking the index for the newest photo before the
// month just found is a handful of milliseconds for the same answer.
func (r *repo) Months(ctx context.Context, owner string, kind db.Kind) ([]db.Month, error) {
	const (
		newest = `SELECT m.sort_at FROM media m JOIN files f ON f.id = m.file_id
			WHERE m.kind = $1 AND f.owner_id = $2 ORDER BY m.sort_at DESC LIMIT 1`
		before = `SELECT m.sort_at FROM media m JOIN files f ON f.id = m.file_id
			WHERE m.kind = $1 AND f.owner_id = $2 AND m.sort_at < $3 ORDER BY m.sort_at DESC LIMIT 1`
	)
	var out []db.Month
	query, args := newest, []any{string(kind), owner}
	for {
		var at time.Time
		err := r.q.QueryRowContext(ctx, query, args...).Scan(&at)
		if errors.Is(err, sql.ErrNoRows) {
			return out, nil
		}
		if err != nil {
			return nil, fmt.Errorf("list photo months: %w", mapErr(err))
		}
		month := db.MonthOf(at)
		out = append(out, month)
		query, args = before, []any{string(kind), owner, month.Start()}
	}
}

func scanCapture(rows *sql.Rows) (db.Capture, error) {
	var at time.Time
	t, err := scanTrackWith(rows, &at)
	if err != nil {
		return db.Capture{}, err
	}
	return db.Capture{Track: t, SortAt: at.UTC()}, nil
}
