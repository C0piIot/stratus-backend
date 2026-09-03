// Package sqlutil is the plumbing the SQL drivers share, and nothing else.
//
// It exists because the two adapters for internal/db are hand-written per
// dialect on purpose, and that made them accumulate identical non-SQL
// machinery: the querier interface, the transaction wrapper, the row loop and
// the "nothing changed, why?" probe. Each new table copied all of it again.
//
// The line this package must not cross is SQL. Not one query string, not one
// placeholder, not one type or error mapping lives here -- those are the parts
// that genuinely differ between SQLite and PostgreSQL, and a third dialect
// would differ again: MySQL has no RETURNING and spells an upsert
// ON DUPLICATE KEY UPDATE. A helper here that knew how to insert a row and read
// its id back is exactly what such a driver would force us to undo, so there
// isn't one.
//
// It also sits below the port rather than inside it: internal/db declares the
// interface, the sentinels and the entities, and internal/files has no business
// seeing a Querier in what it imports.
package sqlutil

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"iter"

	"github.com/C0piIot/stratus-backend/internal/db"
)

// Querier is what *sql.DB and *sql.Tx have in common, so a driver writes each
// query once and it runs inside a transaction as well as outside one.
type Querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// InTx runs fn in a transaction, committing when it returns nil and rolling
// back on an error.
//
// A panic is rolled back and rethrown rather than swallowed: a transaction left
// open holds the write lock, and the next writer would block on it forever.
func InTx(ctx context.Context, sqlDB *sql.DB, fn func(Querier) error) error {
	tx, err := sqlDB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback()
			panic(p)
		}
	}()

	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// Collect runs query and scans every row it returns.
//
// The rows are opened, closed and checked here rather than handed back to the
// caller, which is what makes sqlclosecheck and rowserrcheck able to verify
// them: neither linter can follow ownership across a function boundary, and a
// leaked *sql.Rows is the classic bug in hand-written SQL.
//
// Errors come back unwrapped. The caller adds the context and its own
// classification, because which driver error means a conflict is the one thing
// this package must not know.
func Collect[T any](ctx context.Context, q Querier, scan func(*sql.Rows) (T, error),
	query string, args ...any,
) ([]T, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []T
	for rows.Next() {
		v, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// Seq is Collect for a result bounded by how much is stored rather than by what
// the caller asked for.
//
// Abandoning the iteration closes the rows, which is the whole reason a caller
// may break out of it: the blob collector stops early when it is shutting down.
func Seq[T any](ctx context.Context, q Querier, scan func(*sql.Rows) (T, error),
	query string, args ...any,
) iter.Seq2[T, error] {
	return func(yield func(T, error) bool) {
		var zero T

		rows, err := q.QueryContext(ctx, query, args...)
		if err != nil {
			yield(zero, err)
			return
		}
		defer func() { _ = rows.Close() }()

		for rows.Next() {
			v, err := scan(rows)
			if err != nil {
				yield(zero, err)
				return
			}
			if !yield(v, nil) {
				return
			}
		}
		if err := rows.Err(); err != nil {
			yield(zero, err)
		}
	}
}

// ScanOne reads a single-column row. It is dialect-free for the same reason it
// is useful: a column that maps onto a Go type with no help from the driver is
// the only kind it can read, and a query that returns one is common enough that
// both adapters had written this.
func ScanOne[T any](rows *sql.Rows) (T, error) {
	var v T
	err := rows.Scan(&v)
	return v, err
}

// Label prefixes every error a sequence yields, so a caller learns which query
// failed and not only that one did. It is what a Seq call site does instead of
// the fmt.Errorf a Collect call site can just write inline.
func Label[T any](seq iter.Seq2[T, error], what string) iter.Seq2[T, error] {
	return func(yield func(T, error) bool) {
		for v, err := range seq {
			if err != nil {
				err = fmt.Errorf("%s: %w", what, err)
			}
			if !yield(v, err) {
				return
			}
		}
	}
}

// CheckAffected turns "nothing changed" into the reason it did not, which is
// either that there is no row at path or that it is a directory with something
// still in it. It costs a second query only on the failing path.
//
// probe is the caller's own SQL, selecting the is_dir column of the row at
// path: placeholder syntax is the one thing the dialects cannot agree on, and
// their argument lists differ too, since Postgres can reuse $1 where SQLite has
// to be passed the same value twice.
func CheckAffected(ctx context.Context, q Querier, result sql.Result,
	path, probe string, args ...any,
) error {
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n > 0 {
		return nil
	}

	var isDir bool
	switch err := q.QueryRowContext(ctx, probe, args...).Scan(&isDir); {
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
