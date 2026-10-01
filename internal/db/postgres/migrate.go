package postgres

import (
	"context"
	"fmt"

	"github.com/C0piIot/stratus-backend/internal/db"
)

var _ db.MigrationLocker = (*Store)(nil)

// migrationLock is the advisory lock key every Stratus process agrees on before
// it touches the schema (#242).
//
// The number is arbitrary and must never change. It is a rendezvous rather than
// a secret: two binaries that disagreed about it would exclude nothing. It
// spells STRATUS in ASCII, which is as good a reason as any to be able to
// recognise it in pg_locks.
const migrationLock = 0x53545241545553

// LockMigrations implements db.MigrationLocker.
//
// **A transaction-scoped lock rather than a session one**, because the release
// is the part that has to be right: pg_advisory_unlock is a statement and a
// statement can fail, and a connection that went back to the pool still holding
// the lock would block the next instance against this one. A rollback cannot
// end there -- it either releases the lock or kills the connection, and both
// free it -- so the release is a rollback and nothing else.
//
// The transaction then sits open and idle for the length of the run while the
// migrations themselves go out on other connections from the same pool. That is
// one connection held: a pool capped at one would deadlock here, and nothing
// caps this one.
func (s *Store) LockMigrations(ctx context.Context) (func(), error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("postgres: begin: %w", err)
	}
	// Blocks until it is ours. The wait is the point: the other instance is
	// migrating, and this one has nothing to do until it has finished.
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, int64(migrationLock)); err != nil {
		_ = tx.Rollback()
		return nil, fmt.Errorf("postgres: take the migration lock: %w", err)
	}
	return func() { _ = tx.Rollback() }, nil
}
