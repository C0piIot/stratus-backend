package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/C0piIot/stratus-backend/internal/db"
	"github.com/C0piIot/stratus-backend/internal/db/sqlutil"
)

const lockColumns = `token, owner_id, root, zero_depth, owner_xml, expires_at, held_by, held_until`

// CreateLock implements db.Locks, and overrides the repository's so that a
// creation made outside a transaction gets one.
//
// The guard and the insert have to be the same unit of work or the guard is
// released before it has guarded anything: a row lock lives until the
// transaction that took it ends, and with no transaction that is the end of the
// statement. Inside a caller's Tx the repository's own method runs instead and
// joins the transaction that is already open, which is what it should do.
func (s *Store) CreateLock(ctx context.Context, l db.Lock, now time.Time) error {
	return sqlutil.InTx(ctx, s.db, func(q sqlutil.Querier) error {
		return (&repo{q: q}).CreateLock(ctx, l, now)
	})
}

// CreateLock implements db.Locks.
//
// The insert ignores a lock that has timed out; the unique index cannot, so a
// root whose last lock is dead but not yet swept comes back refused. That is
// what the second half of this is for, and it runs only when the answer was
// going to be a refusal anyway.
func (r *repo) CreateLock(ctx context.Context, l db.Lock, now time.Time) error {
	if err := r.takeTheGuard(ctx, l.OwnerID); err != nil {
		return err
	}

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

// takeTheGuard queues this creation behind any other for the same owner.
//
// The INSERT below carries its own NOT EXISTS, which cannot race a concurrent
// insert *into the row it checks* -- but it can race one into a row that covers
// it, because at READ COMMITTED neither statement sees the other's uncommitted
// work and no index can express "no ancestor of this exists" (#244). So both
// creations first take a row only they contend for, and whichever gets it
// second re-evaluates its NOT EXISTS against a statement that has committed.
//
// An upsert rather than an update, so the row is made the first time without a
// separate path for it. Nothing is stored: the row is the rendezvous.
func (r *repo) takeTheGuard(ctx context.Context, owner string) error {
	const query = `INSERT INTO lock_guard (owner_id) VALUES (?)
		ON CONFLICT (owner_id) DO UPDATE SET owner_id = excluded.owner_id`

	if _, err := r.q.ExecContext(ctx, query, owner); err != nil {
		return fmt.Errorf("take the lock guard: %w", mapErr(err))
	}
	return nil
}

// insertLock is the one statement a create is when nothing is in the way.
func (r *repo) insertLock(ctx context.Context, l db.Lock, now time.Time) error {
	ancestors := db.LockAncestors(l.Root)
	if len(ancestors) == 0 {
		return fmt.Errorf("%w: a lock with no root", db.ErrInvalidPath)
	}

	// The covering check rides inside the INSERT, which is the rule the tree
	// invariant follows two hundred lines up: asking first and inserting after
	// leaves a window a concurrent LOCK fits in, and between two instances that
	// window is a network round trip wide.
	query := `INSERT INTO locks (` + lockColumns + `)
		SELECT ?, ?, ?, ?, ?, ?, ?, ?
		WHERE NOT EXISTS (
			SELECT 1 FROM locks
			WHERE owner_id = ? AND expires_at > ?
			  AND root IN (?` + strings.Repeat(", ?", len(ancestors)-1) + `)
			  AND (zero_depth = 0 OR root = ?)
		)`
	args := []any{
		l.Token, l.OwnerID, l.Root, l.ZeroDepth, l.OwnerXML,
		l.ExpiresAt.UnixMilli(), l.HeldBy, l.HeldUntil.UnixMilli(),
		l.OwnerID, now.UnixMilli(),
	}
	for _, root := range ancestors {
		args = append(args, root)
	}
	args = append(args, l.Root)

	// A lock over a whole collection also has to find nothing locked inside it,
	// which is the one question ancestors cannot answer. A range rather than a
	// prefix match, so it is a seek on the unique index.
	if !l.ZeroDepth {
		from, to := db.LockSubtree(l.Root)
		query += `
		AND NOT EXISTS (
			SELECT 1 FROM locks
			WHERE owner_id = ? AND expires_at > ?
			  AND root >= ? AND root < ? AND root <> ?
		)`
		args = append(args, l.OwnerID, now.UnixMilli(), from, to, l.Root)
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
		WHERE owner_id = ? AND expires_at > ?
		  AND root IN (?` + strings.Repeat(", ?", len(roots)-1) + `)`
	args := []any{owner, now.UnixMilli()}
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

	query := `UPDATE locks SET held_by = ?, held_until = ?, expires_at = ?
		WHERE owner_id = ? AND expires_at > ?
		  AND token IN (?` + strings.Repeat(", ?", len(tokens)-1) + `)
		  AND (held_by = '' OR held_by = ? OR held_until <= ?)`
	args := []any{h.Holder, h.Until.UnixMilli(), h.Expires.UnixMilli(), owner, now.UnixMilli()}
	for _, token := range tokens {
		args = append(args, token)
	}
	args = append(args, h.Holder, now.UnixMilli())

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

	query := `UPDATE locks SET held_by = '', held_until = 0
		WHERE owner_id = ? AND held_by = ?
		  AND token IN (?` + strings.Repeat(", ?", len(tokens)-1) + `)`
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
	const update = `UPDATE locks SET expires_at = ?
		WHERE owner_id = ? AND token = ? AND expires_at > ?
		  AND (held_by = '' OR held_by = ? OR held_until <= ?)`

	_, err := r.q.ExecContext(ctx, update,
		expires.UnixMilli(), owner, token, now.UnixMilli(), holder, now.UnixMilli())
	if err != nil {
		return db.Lock{}, fmt.Errorf("refresh lock %q: %w", token, mapErr(err))
	}
	// What happened is read off the row rather than counted: the update has to
	// be read back anyway, since a client is told the lock it still holds, and
	// "how many rows changed" is the one thing the three drivers do not agree
	// about for a write that sets a column to the value it already had.
	const query = `SELECT ` + lockColumns + ` FROM locks WHERE owner_id = ? AND token = ?`
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
		WHERE owner_id = ? AND token = ? AND expires_at > ?
		  AND (held_by = '' OR held_by = ? OR held_until <= ?)`

	n, err := sqlutil.Affected(ctx, r.q, query, owner, token, now.UnixMilli(), holder, now.UnixMilli())
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
	const query = `DELETE FROM locks WHERE expires_at <= ?`

	n, err := sqlutil.Affected(ctx, r.q, query, now.UnixMilli())
	if err != nil {
		return 0, fmt.Errorf("delete expired locks: %w", mapErr(err))
	}
	return int(n), nil
}

// lockRefusal says why a delete that named one lock removed nothing. It always
// has an answer: the row is gone, or timed out, or somebody is using it.
func (r *repo) lockRefusal(ctx context.Context, owner, token, holder string, now time.Time) error {
	const probe = `SELECT expires_at, held_by, held_until FROM locks WHERE owner_id = ? AND token = ?`

	var expires, until int64
	var by string
	switch err := r.q.QueryRowContext(ctx, probe, owner, token).Scan(&expires, &by, &until); {
	case errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("%w: no lock %q", db.ErrNotFound, token)
	case err != nil:
		return err
	case expires <= now.UnixMilli():
		return fmt.Errorf("%w: lock %q has expired", db.ErrNotFound, token)
	case by != "" && by != holder && until > now.UnixMilli():
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
	var expires, until int64
	if err := scan(&l.Token, &l.OwnerID, &l.Root, &l.ZeroDepth, &l.OwnerXML,
		&expires, &l.HeldBy, &until); err != nil {
		return db.Lock{}, err
	}
	l.ExpiresAt = time.UnixMilli(expires).UTC()
	l.HeldUntil = time.UnixMilli(until).UTC()
	return l, nil
}

func scanLockRow(rows *sql.Rows) (db.Lock, error) { return scanLock(rows.Scan) }
