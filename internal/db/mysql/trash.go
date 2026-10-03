package mysql

import (
	"context"
	"database/sql"
	"fmt"
	"iter"
	"time"

	"github.com/C0piIot/stratus-backend/internal/db"
	"github.com/C0piIot/stratus-backend/internal/db/sqlutil"
)

// trashColumns is the file row as it was, plus which deletion it arrived in
// and when. The order is the one scanTrashed reads back.
const trashColumns = `batch, owner_id, path, blob_key, size, mtime, etag, mime_type, is_dir, deleted_at`

// Trash implements db.Trash.
func (r *repo) Trash(ctx context.Context, batch string, rows []db.File, at time.Time) error {
	const query = `INSERT INTO trash (` + trashColumns + `)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

	for _, f := range rows {
		_, err := r.q.ExecContext(ctx, query,
			batch, f.OwnerID, f.Path, f.BlobKey, f.Size, f.MTime.UnixMilli(),
			f.ETag, f.MIMEType, f.IsDir, at.UnixMilli())
		if err != nil {
			return fmt.Errorf("trash %q: %w", f.Path, mapErr(err))
		}
	}
	return nil
}

// batchColumns is a deletion summed up: the shortest path in it is the folder
// that was deleted, because "holiday" sorts before "holiday/sunset.jpg" and
// every row of a tree is under its root.
//
// The count is of files and not of rows. A deletion carries its directories
// too, so that the tree can be put back (#275), but "three files" is what a
// page can say and "five rows" is not.
const batchColumns = `batch, MIN(path), SUM(CASE WHEN is_dir = 0 THEN 1 ELSE 0 END), COALESCE(SUM(size), 0), MAX(deleted_at)`

// TrashBatches implements db.Trash.
func (r *repo) TrashBatches(ctx context.Context, owner string, after db.TrashCursor, limit int) ([]db.TrashBatch, error) {
	if err := db.ValidateLimit(limit); err != nil {
		return nil, err
	}

	// Newest first. The cursor is spelled out rather than written as a row
	// comparison, which is this dialect's rule everywhere: it does not
	// range-optimise one. Nothing could seek here anyway -- what is compared
	// is an aggregate, so it is a HAVING -- and one driver reading two ways is
	// worse than a form that buys nothing.
	const query = `SELECT ` + batchColumns + ` FROM trash
		WHERE owner_id = ?
		GROUP BY batch
		HAVING ? = '' OR MAX(deleted_at) < ?
		    OR (MAX(deleted_at) = ? AND batch < ?)
		ORDER BY MAX(deleted_at) DESC, batch DESC
		LIMIT ?`

	out, err := sqlutil.Collect(ctx, r.q, scanBatch, query,
		owner, after.Batch, after.DeletedAt.UnixMilli(), after.DeletedAt.UnixMilli(), after.Batch, limit)
	if err != nil {
		return nil, fmt.Errorf("list the trash: %w", mapErr(err))
	}
	return out, nil
}

// TrashedIn implements db.Trash.
func (r *repo) TrashedIn(ctx context.Context, owner, batch string) ([]db.Trashed, error) {
	const query = `SELECT ` + trashColumns + ` FROM trash
		WHERE owner_id = ? AND batch = ? ORDER BY path`

	out, err := sqlutil.Collect(ctx, r.q, scanTrashed, query, owner, batch)
	if err != nil {
		return nil, fmt.Errorf("read the deletion %q: %w", batch, mapErr(err))
	}
	return out, nil
}

// DeleteTrash implements db.Trash.
func (r *repo) DeleteTrash(ctx context.Context, owner, batch string) error {
	const query = `DELETE FROM trash WHERE owner_id = ? AND batch = ?`

	if _, err := r.q.ExecContext(ctx, query, owner, batch); err != nil {
		return fmt.Errorf("delete the deletion %q: %w", batch, mapErr(err))
	}
	return nil
}

// TrashKeys implements db.Trash.
func (r *repo) TrashKeys(ctx context.Context) iter.Seq2[string, error] {
	const query = `SELECT blob_key FROM trash WHERE is_dir = 0`

	return sqlutil.Label(sqlutil.Seq(ctx, r.q, sqlutil.ScanOne[string], query), "list trashed keys")
}

// ExpiredTrash implements db.Trash.
func (r *repo) ExpiredTrash(ctx context.Context, before time.Time) iter.Seq2[db.Trashed, error] {
	const query = `SELECT ` + trashColumns + ` FROM trash
		WHERE deleted_at <= ? ORDER BY deleted_at`

	return sqlutil.Label(sqlutil.Seq(ctx, r.q, scanTrashed, query, before.UnixMilli()), "list expired trash")
}

// TrashTotals implements db.Trash.
func (r *repo) TrashTotals(ctx context.Context, owner string) (db.TrashTotals, error) {
	const query = `SELECT COUNT(*), COALESCE(SUM(size), 0) FROM trash
		WHERE owner_id = ? AND is_dir = 0`

	var t db.TrashTotals
	if err := r.q.QueryRowContext(ctx, query, owner).Scan(&t.Files, &t.Bytes); err != nil {
		return db.TrashTotals{}, fmt.Errorf("measure the trash: %w", mapErr(err))
	}
	return t, nil
}

func scanBatch(rows *sql.Rows) (db.TrashBatch, error) {
	var b db.TrashBatch
	var at int64
	if err := rows.Scan(&b.ID, &b.Root, &b.Files, &b.Bytes, &at); err != nil {
		return db.TrashBatch{}, err
	}
	b.DeletedAt = time.UnixMilli(at).UTC()
	return b, nil
}

func scanTrashed(rows *sql.Rows) (db.Trashed, error) {
	var t db.Trashed
	var mtime, deleted int64
	if err := rows.Scan(&t.Batch, &t.OwnerID, &t.Path, &t.BlobKey, &t.Size,
		&mtime, &t.ETag, &t.MIMEType, &t.IsDir, &deleted); err != nil {
		return db.Trashed{}, err
	}
	t.MTime = time.UnixMilli(mtime).UTC()
	t.DeletedAt = time.UnixMilli(deleted).UTC()
	return t, nil
}
