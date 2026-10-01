package mysql

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/hex"
	"fmt"
	"log/slog"
	"time"

	"github.com/C0piIot/stratus-backend/internal/db"
)

var _ db.MigrationLocker = (*Store)(nil)

const (
	// migrationLockWait bounds how long an instance waits for another's run.
	// Long enough for a real migration over a real library, short enough that a
	// lock nobody is ever going to release is a message rather than a process
	// that never comes up.
	migrationLockWait = 5 * time.Minute

	// releaseTimeout bounds giving the lock back. It is one statement, and the
	// run it belongs to has already finished one way or the other.
	releaseTimeout = 10 * time.Second
)

// LockMigrations implements db.MigrationLocker.
//
// GET_LOCK and not a transaction, because MySQL has no transaction-scoped named
// lock: the lock lives on one connection taken out of the pool for the length
// of the run, and giving it back is a statement of its own. If that statement
// does not succeed the connection is thrown away rather than returned, since a
// pooled connection still holding the lock would block the next instance
// against this one -- and MySQL frees a named lock when its session ends, which
// is exactly what throwing it away does.
//
// **The name is server-wide and carries the schema on purpose.** A fixed name
// would make two Stratus databases on one MySQL server serialise their
// migrations against each other, which is wrong rather than merely slow the day
// one of them is ten minutes into a run. It is hashed because a lock name stops
// at 64 characters and a schema name may be 64 on its own.
func (s *Store) LockMigrations(ctx context.Context) (func(), error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("mysql: connect: %w", err)
	}

	var schema string
	if err := conn.QueryRowContext(ctx, `SELECT DATABASE()`).Scan(&schema); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("mysql: read the schema name: %w", err)
	}
	name := migrationLockName(schema)

	// NULL is what GET_LOCK answers on an error of its own, and 0 is the
	// timeout. Neither is a lock, and a caller told "1" for either would
	// migrate beside somebody else.
	var got sql.NullInt64
	if err := conn.QueryRowContext(ctx, `SELECT GET_LOCK(?, ?)`,
		name, int(migrationLockWait.Seconds())).Scan(&got); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("mysql: take the migration lock: %w", err)
	}
	if !got.Valid || got.Int64 != 1 {
		_ = conn.Close()
		return nil, fmt.Errorf("mysql: another instance has held the migration lock for more than %s", migrationLockWait)
	}

	return func() { releaseMigrationLock(ctx, conn, name) }, nil
}

// migrationLockName is what both instances have to spell the same way.
func migrationLockName(schema string) string {
	return "stratus-migrations-" + hex.EncodeToString(hash(schema))[:32]
}

func releaseMigrationLock(ctx context.Context, conn *sql.Conn, name string) {
	// Detached from the run's own context, which may well be why the run
	// ended: the lock has to come back either way, and a cancelled context
	// cannot release anything.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), releaseTimeout)
	defer cancel()

	var released sql.NullInt64
	err := conn.QueryRowContext(ctx, `SELECT RELEASE_LOCK(?)`, name).Scan(&released)
	if err == nil && released.Valid && released.Int64 == 1 {
		if cerr := conn.Close(); cerr != nil {
			slog.Error("returning the migration lock's connection", "err", cerr)
		}
		return
	}

	// The lock is still on this connection, so the connection must not be the
	// next query's. Handing the driver ErrBadConn is how database/sql is told
	// to close a session rather than pool it.
	slog.Error("releasing the migration lock", "err", err, "released", released.Int64)
	_ = conn.Raw(func(any) error { return driver.ErrBadConn })
}
