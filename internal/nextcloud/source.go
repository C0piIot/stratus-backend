package nextcloud

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"

	mysqldriver "github.com/go-sql-driver/mysql"
	_ "github.com/jackc/pgx/v5/stdlib" // Nextcloud on PostgreSQL
	_ "modernc.org/sqlite"             // Nextcloud on SQLite
)

// The engines Nextcloud runs on, which are the three this server already
// speaks -- so reading somebody else's instance costs no new dependency, only
// the depguard exemption that already exists for SQLite.
const (
	engineSQLite   = "sqlite"
	enginePostgres = "postgres"
	engineMySQL    = "mysql"

	defaultMySQLPort = "3306"
)

// source is an open Nextcloud database.
//
// Reads go through a querier rather than a *sql.DB because what that is
// differs by engine: SQLite is opened read-only by its DSN, while the two
// server engines get a read-only transaction instead. Both satisfy this.
type source struct {
	q      querier
	engine string
	// name is what the report prints: the DSN with any password taken out,
	// since a survey is something people paste into an issue.
	name  string
	close func() error
}

// querier is the half of *sql.DB a read needs, and all a *sql.Tx offers.
type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// arg renders the nth bind parameter. It is the only thing in this package's
// SQL that is not the same on all three engines.
func (s *source) arg(n int) string {
	if s.engine == enginePostgres {
		return "$" + strconv.Itoa(n)
	}
	return "?"
}

// open connects to the foreign database and promises to write nothing to it.
//
// The promise is kept differently on each engine and is not decoration: the
// instance being read is usually still running, and a migration that wrote to
// the thing it was migrating from would be unforgivable. SQLite says `mode=ro`
// in the DSN; PostgreSQL and MySQL get a read-only transaction, which also
// buys a consistent snapshot of a filecache somebody may still be adding to.
func open(ctx context.Context, src string) (*source, error) {
	switch engineOf(src) {
	case enginePostgres:
		return openServer(ctx, enginePostgres, "pgx", src, redact(src))
	case engineMySQL:
		dsn, err := mysqlDSN(src)
		if err != nil {
			return nil, err
		}
		return openServer(ctx, engineMySQL, "mysql", dsn, redact(src))
	default:
		return openSQLite(src)
	}
}

// engineOf reads the engine off the front of what was typed. Anything without
// one of the two server schemes is a path to a SQLite file, which is both the
// shape Nextcloud's own small installs have and what this flag used to accept
// when it accepted nothing else.
func engineOf(src string) string {
	switch {
	case strings.HasPrefix(src, "postgres://"), strings.HasPrefix(src, "postgresql://"):
		return enginePostgres
	case strings.HasPrefix(src, "mysql://"):
		return engineMySQL
	default:
		return engineSQLite
	}
}

// openSQLite opens the file read-only.
//
// Built through url.URL for the reason internal/db/sqlite builds its own that
// way: a path with a space or a question mark in it still has to produce a DSN
// the driver can parse. The busy timeout is because the instance may be
// writing while we read.
func openSQLite(dbPath string) (*source, error) {
	dsn := url.URL{Scheme: "file", Opaque: dbPath}
	query := url.Values{}
	query.Set("mode", "ro")
	query.Add("_pragma", "busy_timeout(5000)")
	dsn.RawQuery = query.Encode()

	conn, err := sql.Open("sqlite", dsn.String())
	if err != nil {
		return nil, fmt.Errorf("nextcloud: open %s: %w", dbPath, err)
	}
	return &source{q: conn, engine: engineSQLite, name: dbPath, close: conn.Close}, nil
}

// openServer opens PostgreSQL or MySQL and holds one read-only transaction
// open for the whole survey.
func openServer(ctx context.Context, engine, driver, dsn, name string) (*source, error) {
	conn, err := sql.Open(driver, dsn)
	if err != nil {
		// The DSN is never in the message: it carries a password.
		return nil, fmt.Errorf("nextcloud: open the %s database: %w", engine, err)
	}
	tx, err := conn.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("nextcloud: begin a read-only transaction on the %s database: %w", engine, err)
	}
	return &source{
		q:      tx,
		engine: engine,
		name:   name,
		close: func() error {
			// Rollback and not Commit: there is nothing to commit, and saying
			// so is the point.
			_ = tx.Rollback()
			return conn.Close()
		},
	}, nil
}

// mysqlDSN turns the URL somebody types into the form the driver wants.
//
// Written here rather than reused from internal/db/mysql because that one is
// about *this* server's database and forces a collation MySQL 8 has and
// MariaDB does not -- and a Nextcloud on MariaDB is the commonest instance
// there is.
func mysqlDSN(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("nextcloud: the MySQL DSN is not a valid URL: %w", err)
	}
	if u.Host == "" {
		return "", fmt.Errorf("nextcloud: the MySQL DSN %q names no host", redact(raw))
	}
	name := strings.TrimPrefix(u.Path, "/")
	if name == "" || strings.Contains(name, "/") {
		return "", fmt.Errorf("nextcloud: the MySQL DSN %q names no database", redact(raw))
	}

	cfg := mysqldriver.NewConfig()
	cfg.Net = "tcp"
	cfg.Addr = u.Host
	if u.Port() == "" {
		cfg.Addr = net.JoinHostPort(u.Hostname(), defaultMySQLPort)
	}
	cfg.DBName = name
	if u.User != nil {
		cfg.User = u.User.Username()
		cfg.Passwd, _ = u.User.Password()
	}
	for key, values := range u.Query() {
		if len(values) == 0 {
			continue
		}
		// tls is the one parameter the driver keeps in a field of its own
		// rather than in Params, and it is the one somebody connecting to a
		// database on another machine actually reaches for.
		if key == "tls" {
			cfg.TLSConfig = values[0]
			continue
		}
		if cfg.Params == nil {
			cfg.Params = map[string]string{}
		}
		cfg.Params[key] = values[0]
	}
	return cfg.FormatDSN(), nil
}

// redact takes the password out of a DSN.
//
// The report is printed to a terminal and pasted into issues, and the one
// thing on that line nobody meant to share is the password of the instance
// being migrated.
func redact(src string) string {
	u, err := url.Parse(src)
	if err != nil || u.User == nil {
		return src
	}
	if _, set := u.User.Password(); set {
		u.User = url.UserPassword(u.User.Username(), "xxxxx")
	}
	return u.String()
}
