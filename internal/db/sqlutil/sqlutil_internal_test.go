package sqlutil

import (
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/C0piIot/stratus-backend/internal/db"
)

// The fault-injecting driver this file registers is the reason the plumbing
// lives in a package of its own. Inside the real adapters these branches are
// unreachable -- scripts/coverage.sh says as much -- because a working SQLite
// or PostgreSQL will not fail a RowsAffected or stop halfway through a result
// set on request. Here they are just fields in a struct.

func init() { sql.Register("sqlutilfake", fakeDriver{}) }

// config is what a test wants the driver to do. The zero value is a driver that
// succeeds at everything and returns no rows.
type config struct {
	beginErr    error
	commitErr   error
	queryErr    error
	affected    int64
	affectedErr error

	columns []string
	rows    [][]driver.Value

	// nextErr is returned instead of the row at index errAfter, which is how a
	// result set that fails halfway is expressed.
	nextErr  error
	errAfter int

	closedRows atomic.Int32
}

var registry sync.Map // dsn -> *config

var dsnSeq atomic.Int64

// open registers cfg under a fresh DSN and returns a pool of exactly one
// connection, so which connection serves a query is never in question.
func open(t *testing.T, cfg *config) *sql.DB {
	t.Helper()

	dsn := strconv.FormatInt(dsnSeq.Add(1), 10)
	registry.Store(dsn, cfg)
	t.Cleanup(func() { registry.Delete(dsn) })

	sqlDB, err := sql.Open("sqlutilfake", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() {
		if err := sqlDB.Close(); err != nil {
			t.Errorf("close: %v", err)
		}
	})
	return sqlDB
}

var errBoom = errors.New("boom")

func TestInTxCommits(t *testing.T) {
	t.Parallel()
	sqlDB := open(t, &config{})

	var ran bool
	if err := InTx(t.Context(), sqlDB, func(Querier) error {
		ran = true
		return nil
	}); err != nil {
		t.Fatalf("InTx: %v", err)
	}
	if !ran {
		t.Error("InTx did not run the function")
	}
}

// TestInTxBeginFails is the branch a real database only reaches when the pool
// is exhausted or the file is gone.
func TestInTxBeginFails(t *testing.T) {
	t.Parallel()
	sqlDB := open(t, &config{beginErr: errBoom})

	err := InTx(t.Context(), sqlDB, func(Querier) error {
		t.Error("the function ran even though the transaction never began")
		return nil
	})
	if !errors.Is(err, errBoom) {
		t.Errorf("InTx = %v, want the error BeginTx returned", err)
	}
}

func TestInTxRollsBack(t *testing.T) {
	t.Parallel()
	sqlDB := open(t, &config{})

	err := InTx(t.Context(), sqlDB, func(Querier) error { return errBoom })
	if !errors.Is(err, errBoom) {
		t.Errorf("InTx = %v, want the error the function returned", err)
	}
}

// TestInTxCommitFails pins that a commit failure is reported rather than
// mistaken for success, which is the difference between losing an upload and
// telling the client it failed.
func TestInTxCommitFails(t *testing.T) {
	t.Parallel()
	sqlDB := open(t, &config{commitErr: errBoom})

	err := InTx(t.Context(), sqlDB, func(Querier) error { return nil })
	if !errors.Is(err, errBoom) {
		t.Errorf("InTx = %v, want the error Commit returned", err)
	}
}

func TestInTxRollsBackOnPanic(t *testing.T) {
	t.Parallel()
	sqlDB := open(t, &config{})

	func() {
		defer func() {
			if recover() == nil {
				t.Error("the panic was swallowed; it must reach the caller")
			}
		}()
		_ = InTx(t.Context(), sqlDB, func(Querier) error { panic("something went very wrong") })
	}()
}

func scanString(rows *sql.Rows) (string, error) {
	var s string
	err := rows.Scan(&s)
	return s, err
}

func TestCollect(t *testing.T) {
	t.Parallel()
	cfg := &config{columns: []string{"k"}, rows: [][]driver.Value{{"a"}, {"b"}}}

	got, err := Collect(t.Context(), open(t, cfg), scanString, "SELECT k")
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if want := []string{"a", "b"}; len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("Collect = %v, want %v", got, want)
	}
}

// TestCollectOfNothing pins the nil slice: a listing of an empty directory is
// not an error, and every caller treats a nil result as empty.
func TestCollectOfNothing(t *testing.T) {
	t.Parallel()

	got, err := Collect(t.Context(), open(t, &config{columns: []string{"k"}}), scanString, "SELECT k")
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if got != nil {
		t.Errorf("Collect = %v, want nil", got)
	}
}

func TestCollectQueryFails(t *testing.T) {
	t.Parallel()

	_, err := Collect(t.Context(), open(t, &config{queryErr: errBoom}), scanString, "SELECT k")
	if !errors.Is(err, errBoom) {
		t.Errorf("Collect = %v, want the error the query returned", err)
	}
}

func TestCollectScanFails(t *testing.T) {
	t.Parallel()
	cfg := &config{columns: []string{"k"}, rows: [][]driver.Value{{"a"}}}
	scan := func(*sql.Rows) (string, error) { return "", errBoom }

	_, err := Collect(t.Context(), open(t, cfg), scan, "SELECT k")
	if !errors.Is(err, errBoom) {
		t.Errorf("Collect = %v, want the error the scan returned", err)
	}
}

// TestCollectFailsHalfway is rows.Err: the result set began fine and broke
// after the first row, which must not look like a short but successful list.
func TestCollectFailsHalfway(t *testing.T) {
	t.Parallel()
	cfg := &config{
		columns:  []string{"k"},
		rows:     [][]driver.Value{{"a"}, {"b"}},
		nextErr:  errBoom,
		errAfter: 1,
	}

	_, err := Collect(t.Context(), open(t, cfg), scanString, "SELECT k")
	if !errors.Is(err, errBoom) {
		t.Errorf("Collect = %v, want the error the iteration returned", err)
	}
}

func TestSeq(t *testing.T) {
	t.Parallel()
	cfg := &config{columns: []string{"k"}, rows: [][]driver.Value{{"a"}, {"b"}}}

	var got []string
	for v, err := range Seq(t.Context(), open(t, cfg), scanString, "SELECT k") {
		if err != nil {
			t.Fatalf("Seq: %v", err)
		}
		got = append(got, v)
	}
	if want := []string{"a", "b"}; len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("Seq = %v, want %v", got, want)
	}
	if n := cfg.closedRows.Load(); n != 1 {
		t.Errorf("the rows were closed %d times, want once", n)
	}
}

// TestSeqAbandoned is the path the blob collector takes when it stops early.
// The rows must close anyway, or the next writer waits on a read lock nobody
// is holding on purpose.
func TestSeqAbandoned(t *testing.T) {
	t.Parallel()
	cfg := &config{columns: []string{"k"}, rows: [][]driver.Value{{"a"}, {"b"}, {"c"}}}

	for range Seq(t.Context(), open(t, cfg), scanString, "SELECT k") {
		break
	}
	if n := cfg.closedRows.Load(); n != 1 {
		t.Errorf("the rows were closed %d times, want once", n)
	}
}

func TestSeqQueryFails(t *testing.T) {
	t.Parallel()

	var got error
	for _, err := range Seq(t.Context(), open(t, &config{queryErr: errBoom}), scanString, "SELECT k") {
		got = err
	}
	if !errors.Is(got, errBoom) {
		t.Errorf("Seq yielded %v, want the error the query returned", got)
	}
}

func TestSeqScanFails(t *testing.T) {
	t.Parallel()
	cfg := &config{columns: []string{"k"}, rows: [][]driver.Value{{"a"}}}
	scan := func(*sql.Rows) (string, error) { return "", errBoom }

	var got error
	for _, err := range Seq(t.Context(), open(t, cfg), scan, "SELECT k") {
		got = err
	}
	if !errors.Is(got, errBoom) {
		t.Errorf("Seq yielded %v, want the error the scan returned", got)
	}
	if n := cfg.closedRows.Load(); n != 1 {
		t.Errorf("the rows were closed %d times, want once", n)
	}
}

func TestSeqFailsHalfway(t *testing.T) {
	t.Parallel()
	cfg := &config{
		columns:  []string{"k"},
		rows:     [][]driver.Value{{"a"}, {"b"}},
		nextErr:  errBoom,
		errAfter: 1,
	}

	var got []string
	var gotErr error
	for v, err := range Seq(t.Context(), open(t, cfg), scanString, "SELECT k") {
		if err != nil {
			gotErr = err
			continue
		}
		got = append(got, v)
	}
	if len(got) != 1 || got[0] != "a" {
		t.Errorf("Seq yielded %v, want the one row that arrived", got)
	}
	if !errors.Is(gotErr, errBoom) {
		t.Errorf("Seq yielded %v, want the error the iteration returned", gotErr)
	}
}

func TestLabel(t *testing.T) {
	t.Parallel()
	cfg := &config{columns: []string{"k"}, rows: [][]driver.Value{{"a"}}, queryErr: errBoom}

	var got error
	for _, err := range Label(Seq(t.Context(), open(t, cfg), scanString, "SELECT k"), "list blob keys") {
		got = err
	}
	if !errors.Is(got, errBoom) {
		t.Fatalf("Label dropped the error: %v", got)
	}
	if want := "list blob keys: boom"; got.Error() != want {
		t.Errorf("Label = %q, want %q", got, want)
	}
}

// TestLabelPassesRowsThrough pins that labelling does not disturb the happy
// path, including a caller that stops early.
func TestLabelPassesRowsThrough(t *testing.T) {
	t.Parallel()
	cfg := &config{columns: []string{"k"}, rows: [][]driver.Value{{"a"}, {"b"}}}

	var got []string
	for v, err := range Label(Seq(t.Context(), open(t, cfg), scanString, "SELECT k"), "whatever") {
		if err != nil {
			t.Fatalf("Label: %v", err)
		}
		got = append(got, v)
		break
	}
	if len(got) != 1 || got[0] != "a" {
		t.Errorf("Label = %v, want the first row", got)
	}
	if n := cfg.closedRows.Load(); n != 1 {
		t.Errorf("the rows were closed %d times, want once", n)
	}
}

const probe = "SELECT is_dir"

func TestCheckAffected(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		cfg  *config
		want error
		// wants, when the sentinel alone does not say which branch answered.
		wants string
	}{
		{
			name: "a row was changed",
			cfg:  &config{affected: 1},
		},
		{
			name:  "nothing at that path",
			cfg:   &config{columns: []string{"is_dir"}},
			want:  db.ErrNotFound,
			wants: "photos/img.jpg",
		},
		{
			name:  "a directory with something in it",
			cfg:   &config{columns: []string{"is_dir"}, rows: [][]driver.Value{{true}}},
			want:  db.ErrConflict,
			wants: "is a directory and is not empty",
		},
		{
			// The row is there, is not a directory, and was not touched, so it
			// went between the two queries.
			name:  "the row went between the two queries",
			cfg:   &config{columns: []string{"is_dir"}, rows: [][]driver.Value{{false}}},
			want:  db.ErrNotFound,
			wants: "photos/img.jpg",
		},
		{
			name: "RowsAffected itself fails",
			cfg:  &config{affectedErr: errBoom},
			want: errBoom,
		},
		{
			name: "the probe fails",
			cfg:  &config{queryErr: errBoom},
			want: errBoom,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sqlDB := open(t, tc.cfg)

			result, err := sqlDB.ExecContext(t.Context(), "DELETE FROM files")
			if err != nil {
				t.Fatalf("exec: %v", err)
			}

			err = CheckAffected(t.Context(), sqlDB, result, "photos/img.jpg", probe, "edu", "photos/img.jpg")
			if !errors.Is(err, tc.want) {
				t.Fatalf("CheckAffected = %v, want %v", err, tc.want)
			}
			if tc.wants != "" && !strings.Contains(err.Error(), tc.wants) {
				t.Errorf("CheckAffected = %q, want it to mention %q", err, tc.wants)
			}
		})
	}
}

// The driver itself. It answers whatever its config says and nothing more: no
// SQL is parsed, and a query is told apart from an exec only by which method
// database/sql calls.

type fakeDriver struct{}

func (fakeDriver) Open(dsn string) (driver.Conn, error) {
	cfg, ok := registry.Load(dsn)
	if !ok {
		return nil, fmt.Errorf("sqlutilfake: no config registered for %q", dsn)
	}
	return &fakeConn{cfg: cfg.(*config)}, nil
}

type fakeConn struct{ cfg *config }

func (c *fakeConn) Prepare(string) (driver.Stmt, error) { return &fakeStmt{cfg: c.cfg}, nil }
func (c *fakeConn) Close() error                        { return nil }

func (c *fakeConn) Begin() (driver.Tx, error) {
	if c.cfg.beginErr != nil {
		return nil, c.cfg.beginErr
	}
	return fakeTx{cfg: c.cfg}, nil
}

type fakeTx struct{ cfg *config }

func (tx fakeTx) Commit() error   { return tx.cfg.commitErr }
func (tx fakeTx) Rollback() error { return nil }

type fakeStmt struct{ cfg *config }

func (s *fakeStmt) Close() error  { return nil }
func (s *fakeStmt) NumInput() int { return -1 }

func (s *fakeStmt) Exec([]driver.Value) (driver.Result, error) {
	return fakeResult{cfg: s.cfg}, nil
}

func (s *fakeStmt) Query([]driver.Value) (driver.Rows, error) {
	if s.cfg.queryErr != nil {
		return nil, s.cfg.queryErr
	}
	return &fakeRows{cfg: s.cfg}, nil
}

type fakeResult struct{ cfg *config }

func (r fakeResult) LastInsertId() (int64, error) { return 0, nil }

func (r fakeResult) RowsAffected() (int64, error) {
	if r.cfg.affectedErr != nil {
		return 0, r.cfg.affectedErr
	}
	return r.cfg.affected, nil
}

type fakeRows struct {
	cfg *config
	i   int
}

func (r *fakeRows) Columns() []string { return r.cfg.columns }

func (r *fakeRows) Close() error {
	r.cfg.closedRows.Add(1)
	return nil
}

func (r *fakeRows) Next(dest []driver.Value) error {
	if r.cfg.nextErr != nil && r.i == r.cfg.errAfter {
		return r.cfg.nextErr
	}
	if r.i >= len(r.cfg.rows) {
		return io.EOF
	}
	copy(dest, r.cfg.rows[r.i])
	r.i++
	return nil
}
