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

const lockColumns = `token, owner_id, root, zero_depth, owner_xml, expires_at, held_by, held_until`

// CreateLock implements db.Locks.
//
// The insert ignores a lock that has timed out; the unique index cannot, so a
// root whose last lock is dead but not yet swept comes back refused. That is
// what the second half of this is for, and it runs only when the answer was
// going to be a refusal anyway.
func (r *repo) CreateLock(ctx context.Context, l db.Lock, now time.Time) error {
	err := r.insertLock(ctx, l, now)
	if !errors.Is(err, db.ErrConflict) {
		return err
	}
	swept, serr := r.DeleteExpiredLocks(ctx, now)
	if serr != nil {
		return fmt.Errorf("create lock on %q: %w", l.Root, serr)
	}
	if swept == 0 {
		return err
	}
	return r.insertLock(ctx, l, now)
}

// insertLock is the one statement a create is when nothing is in the way.
func (r *repo) insertLock(ctx context.Context, l db.Lock, now time.Time) error {
	ancestors := db.LockAncestors(l.Root)
	if len(ancestors) == 0 {
		return fmt.Errorf("%w: a lock with no root", db.ErrInvalidPath)
	}

	// The casts are not decoration: a parameter in the select list of an
	// INSERT ... SELECT has no column to take its type from, and the server
	// refuses to guess.
	//
	// The covering check rides inside the statement rather than a read the
	// caller makes first, which is the rule the tree invariant follows: asking
	// and then inserting leaves a window a concurrent LOCK fits in.
	query := `INSERT INTO locks (` + lockColumns + `)
		SELECT $1::text, $2::text, $3::text, $4::boolean, $5::text, $6::timestamptz, $7::text, $8::timestamptz
		WHERE NOT EXISTS (
			SELECT 1 FROM locks
			WHERE owner_id = $2 AND expires_at > $9
			  AND root IN (` + placeholders(10, len(ancestors)) + `)
			  AND (zero_depth = FALSE OR root = $3)
		)`
	args := []any{
		l.Token, l.OwnerID, l.Root, l.ZeroDepth, l.OwnerXML,
		l.ExpiresAt, l.HeldBy, l.HeldUntil, now,
	}
	for _, root := range ancestors {
		args = append(args, root)
	}

	// A lock over a whole collection also has to find nothing locked inside it,
	// which is the one question ancestors cannot answer. A range rather than a
	// prefix match, so it is a seek on the unique index.
	if !l.ZeroDepth {
		from, to := db.LockSubtree(l.Root)
		next := len(args) + 1
		query += `
		AND NOT EXISTS (
			SELECT 1 FROM locks
			WHERE owner_id = $2 AND expires_at > $9
			  AND root >= $` + strconv.Itoa(next) + ` AND root < $` + strconv.Itoa(next+1) + `
			  AND root <> $3
		)`
		args = append(args, from, to)
	}

	n, err := sqlutil.Affected(ctx, r.q, query, args...)
	if err != nil {
		return fmt.Errorf("create lock on %q: %w", l.Root, mapErr(err))
	}
	if n == 0 {
		return fmt.Errorf("%w: %q is locked", db.ErrConflict, l.Root)
	}
	return nil
}

// LocksCovering implements db.Locks.
func (r *repo) LocksCovering(ctx context.Context, owner string, names []string, now time.Time) ([]db.Lock, error) {
	roots := db.LockRoots(names)
	if len(roots) == 0 {
		return nil, nil
	}

	query := `SELECT ` + lockColumns + ` FROM locks
		WHERE owner_id = $1 AND expires_at > $2
		  AND root IN (` + placeholders(3, len(roots)) + `)`
	args := []any{owner, now}
	for _, root := range roots {
		args = append(args, root)
	}

	out, err := sqlutil.Collect(ctx, r.q, scanLockRow, query, args...)
	if err != nil {
		return nil, fmt.Errorf("locks covering %v: %w", names, mapErr(err))
	}
	return out, nil
}

// HoldLocks implements db.Locks.
func (r *repo) HoldLocks(ctx context.Context, owner string, tokens []string, h db.Hold, now time.Time) (int, error) {
	if len(tokens) == 0 {
		return 0, nil
	}

	query := `UPDATE locks SET held_by = $1, held_until = $2, expires_at = $3
		WHERE owner_id = $4 AND expires_at > $5
		  AND token IN (` + placeholders(6, len(tokens)) + `)
		  AND (held_by = '' OR held_by = $1 OR held_until <= $5)`
	args := []any{h.Holder, h.Until, h.Expires, owner, now}
	for _, token := range tokens {
		args = append(args, token)
	}

	n, err := sqlutil.Affected(ctx, r.q, query, args...)
	if err != nil {
		return 0, fmt.Errorf("hold locks: %w", mapErr(err))
	}
	return int(n), nil
}

// ReleaseLocks implements db.Locks.
func (r *repo) ReleaseLocks(ctx context.Context, owner string, tokens []string, holder string) error {
	if len(tokens) == 0 {
		return nil
	}

	query := `UPDATE locks SET held_by = '', held_until = 'epoch'
		WHERE owner_id = $1 AND held_by = $2
		  AND token IN (` + placeholders(3, len(tokens)) + `)`
	args := []any{owner, holder}
	for _, token := range tokens {
		args = append(args, token)
	}

	if _, err := r.q.ExecContext(ctx, query, args...); err != nil {
		return fmt.Errorf("release locks: %w", mapErr(err))
	}
	return nil
}

// RefreshLock implements db.Locks.
func (r *repo) RefreshLock(ctx context.Context, owner, token, holder string, expires, now time.Time) (db.Lock, error) {
	const update = `UPDATE locks SET expires_at = $1
		WHERE owner_id = $2 AND token = $3 AND expires_at > $4
		  AND (held_by = '' OR held_by = $5 OR held_until <= $4)`

	_, err := r.q.ExecContext(ctx, update, expires, owner, token, now, holder)
	if err != nil {
		return db.Lock{}, fmt.Errorf("refresh lock %q: %w", token, mapErr(err))
	}

	// What happened is read off the row rather than counted: the update has to
	// be read back anyway, since a client is told the lock it still holds, and
	// "how many rows changed" is the one thing the three drivers do not agree
	// about for a write that sets a column to the value it already had.
	const query = `SELECT ` + lockColumns + ` FROM locks WHERE owner_id = $1 AND token = $2`
	lock, err := scanLock(r.q.QueryRowContext(ctx, query, owner, token).Scan)
	if err != nil {
		return db.Lock{}, fmt.Errorf("refresh lock %q: %w", token, mapErr(err))
	}
	switch {
	case !lock.ExpiresAt.After(now):
		return db.Lock{}, fmt.Errorf("refresh lock %q: %w: it has expired", token, db.ErrNotFound)
	case lock.Held(now) && lock.HeldBy != holder:
		return db.Lock{}, fmt.Errorf("refresh lock %q: %w: a request in flight holds it", token, db.ErrConflict)
	}
	return lock, nil
}

// DeleteLock implements db.Locks.
func (r *repo) DeleteLock(ctx context.Context, owner, token, holder string, now time.Time) error {
	const query = `DELETE FROM locks
		WHERE owner_id = $1 AND token = $2 AND expires_at > $3
		  AND (held_by = '' OR held_by = $4 OR held_until <= $3)`

	n, err := sqlutil.Affected(ctx, r.q, query, owner, token, now, holder)
	if err != nil {
		return fmt.Errorf("delete lock %q: %w", token, mapErr(err))
	}
	if n == 0 {
		return fmt.Errorf("delete lock %q: %w", token, r.lockRefusal(ctx, owner, token, holder, now))
	}
	return nil
}

// DeleteExpiredLocks implements db.Locks.
func (r *repo) DeleteExpiredLocks(ctx context.Context, now time.Time) (int, error) {
	const query = `DELETE FROM locks WHERE expires_at <= $1`

	n, err := sqlutil.Affected(ctx, r.q, query, now)
	if err != nil {
		return 0, fmt.Errorf("delete expired locks: %w", mapErr(err))
	}
	return int(n), nil
}

// lockRefusal says why a delete that named one lock removed nothing. It always
// has an answer: the row is gone, or timed out, or somebody is using it.
func (r *repo) lockRefusal(ctx context.Context, owner, token, holder string, now time.Time) error {
	const probe = `SELECT expires_at, held_by, held_until FROM locks WHERE owner_id = $1 AND token = $2`

	var expires, until time.Time
	var by string
	switch err := r.q.QueryRowContext(ctx, probe, owner, token).Scan(&expires, &by, &until); {
	case errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("%w: no lock %q", db.ErrNotFound, token)
	case err != nil:
		return err
	case !expires.After(now):
		return fmt.Errorf("%w: lock %q has expired", db.ErrNotFound, token)
	case by != "" && by != holder && until.After(now):
		return fmt.Errorf("%w: lock %q is held by a request in flight", db.ErrConflict, token)
	}
	// Live, free, and the delete took nothing, so it went between the two
	// statements -- another unlock winning. Nothing useful to say beyond that
	// it is not there now.
	return fmt.Errorf("%w: no lock %q", db.ErrNotFound, token)
}

// scanLock reads one row of lockColumns, whether it came from a QueryRow or
// from the row loop: the two differ only in which Scan they hand over, and the
// conversion after it was written twice for no other reason.
func scanLock(scan func(dest ...any) error) (db.Lock, error) {
	var l db.Lock
	if err := scan(&l.Token, &l.OwnerID, &l.Root, &l.ZeroDepth, &l.OwnerXML,
		&l.ExpiresAt, &l.HeldBy, &l.HeldUntil); err != nil {
		return db.Lock{}, err
	}
	l.ExpiresAt = l.ExpiresAt.UTC()
	l.HeldUntil = l.HeldUntil.UTC()
	return l, nil
}

func scanLockRow(rows *sql.Rows) (db.Lock, error) { return scanLock(rows.Scan) }
