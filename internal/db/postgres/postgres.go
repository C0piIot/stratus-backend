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
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" driver

	"github.com/C0piIot/stratus-backend/internal/db"
)

//go:embed migrations/*.sql
var migrations embed.FS

// uniqueViolation is SQLSTATE 23505.
const uniqueViolation = "23505"

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
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback()
			panic(p)
		}
	}()

	if err := fn(&repo{q: tx}); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// querier is what *sql.DB and *sql.Tx have in common.
type querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

type repo struct{ q querier }

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
	const query = `SELECT ` + fileColumns + ` FROM files
		WHERE owner_id = $1 AND parent_path = $2 ORDER BY path`

	rows, err := r.q.QueryContext(ctx, query, owner, dir)
	if err != nil {
		return nil, fmt.Errorf("list %q: %w", dir, mapErr(err))
	}
	defer func() { _ = rows.Close() }()

	var out []db.File
	for rows.Next() {
		var f db.File
		if err := rows.Scan(&f.ID, &f.OwnerID, &f.Path, &f.BlobKey, &f.Size, &f.MTime, &f.ETag, &f.MIMEType, &f.IsDir); err != nil {
			return nil, fmt.Errorf("list %q: %w", dir, err)
		}
		f.MTime = f.MTime.UTC()
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list %q: %w", dir, err)
	}
	return out, nil
}

// BlobKeys implements db.Repo.
func (r *repo) BlobKeys(ctx context.Context) iter.Seq2[string, error] {
	return func(yield func(string, error) bool) {
		const query = `SELECT blob_key FROM files WHERE is_dir = FALSE`

		rows, err := r.q.QueryContext(ctx, query)
		if err != nil {
			yield("", fmt.Errorf("list blob keys: %w", err))
			return
		}
		defer func() { _ = rows.Close() }()

		for rows.Next() {
			var key string
			if err := rows.Scan(&key); err != nil {
				yield("", fmt.Errorf("list blob keys: %w", err))
				return
			}
			if !yield(key, nil) {
				return
			}
		}
		if err := rows.Err(); err != nil {
			yield("", fmt.Errorf("list blob keys: %w", err))
		}
	}
}

// MoveFile implements db.Repo.
func (r *repo) MoveFile(ctx context.Context, owner, from, to string) error {
	if err := db.ValidatePath(from); err != nil {
		return err
	}
	if err := db.ValidatePath(to); err != nil {
		return err
	}
	// The NOT EXISTS is what refuses to move a directory out from under its
	// contents: rewriting a whole subtree is a different operation, and this
	// one must not half-do it.
	const query = `UPDATE files SET path = $1, parent_path = $2
		WHERE owner_id = $3 AND path = $4
		  AND NOT EXISTS (SELECT 1 FROM files child WHERE child.owner_id = $3 AND child.parent_path = $4)`

	result, err := r.q.ExecContext(ctx, query, to, db.ParentOf(to), owner, from)
	if err != nil {
		return fmt.Errorf("move %q to %q: %w", from, to, mapErr(err))
	}
	return r.checkAffected(ctx, result, owner, from)
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
	return r.checkAffected(ctx, result, owner, path)
}

// checkAffected turns "nothing changed" into the reason it did not, which is
// either that there is no such row or that it is a directory with something
// still in it. It costs a second query only on the failing path.
func (r *repo) checkAffected(ctx context.Context, result sql.Result, owner, path string) error {
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n > 0 {
		return nil
	}

	var isDir bool
	switch err := r.q.QueryRowContext(ctx, `SELECT is_dir FROM files WHERE owner_id = $1 AND path = $2`, owner, path).Scan(&isDir); {
	case errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("%w: %q", db.ErrNotFound, path)
	case err != nil:
		return err
	case isDir:
		return fmt.Errorf("%w: %q is a directory and is not empty", db.ErrConflict, path)
	default:
		// The row is there and was not touched, so it went between the two
		// queries. Nothing useful to say beyond that it is not there now.
		return fmt.Errorf("%w: %q", db.ErrNotFound, path)
	}
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
	if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation {
		return fmt.Errorf("%w: %w", db.ErrConflict, err)
	}
	return err
}
