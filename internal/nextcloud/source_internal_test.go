package nextcloud

import (
	"strings"
	"testing"
)

func TestTheEngineIsReadOffTheFrontOfWhatWasTyped(t *testing.T) {
	for src, want := range map[string]string{
		"/var/www/nextcloud/data/owncloud.db": engineSQLite,
		"nextcloud.db":                        engineSQLite,
		"postgres://u:p@db.lan/nextcloud":     enginePostgres,
		"postgresql://u:p@db.lan/nextcloud":   enginePostgres,
		"mysql://u:p@db.lan:3306/nextcloud":   engineMySQL,
		"/srv/mysql://not-a-dsn/owncloud.db":  engineSQLite,
	} {
		if got := engineOf(src); got != want {
			t.Errorf("engineOf(%q) = %q, want %q", src, got, want)
		}
	}
}

func TestOnlyPostgresNumbersItsParameters(t *testing.T) {
	for engine, want := range map[string]string{
		enginePostgres: "$1",
		engineMySQL:    "?",
		engineSQLite:   "?",
	} {
		if got := (&source{engine: engine}).arg(1); got != want {
			t.Errorf("%s arg = %q, want %q", engine, got, want)
		}
	}
}

func TestTheMySQLDSNIsTranslatedForTheDriver(t *testing.T) {
	got, err := mysqlDSN("mysql://nextcloud:secret@db.lan:3307/nc?charset=utf8mb4")
	if err != nil {
		t.Fatalf("mysqlDSN: %v", err)
	}
	for _, want := range []string{"nextcloud:secret@", "tcp(db.lan:3307)", "/nc", "charset=utf8mb4"} {
		if !strings.Contains(got, want) {
			t.Errorf("DSN %q does not carry %q", got, want)
		}
	}
}

func TestTheMySQLPortIsOptional(t *testing.T) {
	got, err := mysqlDSN("mysql://u:p@db.lan/nc")
	if err != nil {
		t.Fatalf("mysqlDSN: %v", err)
	}
	if !strings.Contains(got, "tcp(db.lan:3306)") {
		t.Errorf("DSN %q did not default the port", got)
	}
}

// tls is the one parameter the driver keeps in a field of its own, so a DSN
// that carried it in Params would connect in the clear and say nothing.
func TestTheMySQLDSNKeepsTLS(t *testing.T) {
	got, err := mysqlDSN("mysql://u:p@db.lan/nc?tls=skip-verify")
	if err != nil {
		t.Fatalf("mysqlDSN: %v", err)
	}
	if !strings.Contains(got, "tls=skip-verify") {
		t.Errorf("DSN %q lost the TLS setting", got)
	}
}

func TestAMySQLDSNWithNothingToConnectToIsRefused(t *testing.T) {
	for _, src := range []string{"mysql://u:p@db.lan", "mysql:///nc", "mysql://u:p@db.lan/a/b"} {
		if _, err := mysqlDSN(src); err == nil {
			t.Errorf("mysqlDSN(%q) was accepted", src)
		}
	}
}

// The report is printed to a terminal and pasted into issues, so the password
// of the instance being migrated must not be on that line.
func TestTheDSNIsPrintedWithoutItsPassword(t *testing.T) {
	got := redact("postgres://nextcloud:hunter2@db.lan:5432/nc?sslmode=require")
	if strings.Contains(got, "hunter2") {
		t.Fatalf("the password survived redaction: %q", got)
	}
	for _, want := range []string{"nextcloud", "db.lan:5432", "/nc", "sslmode=require"} {
		if !strings.Contains(got, want) {
			t.Errorf("redacted DSN %q lost %q", got, want)
		}
	}
}

func TestAPathIsPrintedAsItIs(t *testing.T) {
	const path = "/var/www/nextcloud/data/owncloud.db"
	if got := redact(path); got != path {
		t.Errorf("redact(%q) = %q", path, got)
	}
}

func TestAnUnreachableServerFailsToOpenRatherThanLater(t *testing.T) {
	// Port 1 on the loopback refuses, and the read-only transaction is what
	// turns that into an error here rather than at the first query.
	if _, err := open(t.Context(), "postgres://u:p@127.0.0.1:1/nc?sslmode=disable&connect_timeout=1"); err == nil {
		t.Error("a database that is not there was opened")
	}
}
