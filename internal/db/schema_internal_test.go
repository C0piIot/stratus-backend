package db

import (
	"fmt"
	"os"
	"slices"
	"testing"
)

// TestBothDriversCarryTheSameMigrations catches the cheapest mistake there is to
// make with two hand-written migration sets: writing one half of a pair. Migrate
// decides what to apply from MAX(version), so a 0002 that exists only in sqlite
// leaves a Postgres database a version behind with nothing saying so.
//
// What this deliberately no longer checks is that the two sets describe the same
// schema. It used to compare every statement and every column in order, and that
// was the wrong contract: how a driver represents the data is the driver's own
// business, which is the same rule that keeps its SQL inside its package. A third
// adapter makes it concrete -- MySQL cannot index a TEXT path the way these two
// do, so it would need a path_hash column that exists nowhere else (#27), and a
// comparison of shape would have called that a bug.
//
// The port's contract is behaviour, and internal/db/dbtest is where it is
// enforced. That is also where the bug behind the old comparison is actually
// caught: the Postgres schema once declared `size INTEGER`, which is 32 bits
// there and capped every file at 2 GB, and no reading of the DDL finds it --
// the text is identical in both dialects. The round-trip cases do.
//
// It does not open a database. It reads the directories from disk rather than
// either driver's embed, because a driver imports this package and an internal
// test here therefore cannot import a driver. Reading text also means it runs
// under `make test` and `make test-race`, where the Postgres suite skips for
// want of STRATUS_TEST_POSTGRES_DSN.
// oneSided are the migrations one of the two deliberately does not have, with
// the reason. A driver answers in whatever way its engine is best at, and
// sometimes that is nothing at all -- but it has to be written down here, or
// the half-written pair this test exists to catch looks exactly the same.
//
// Empty since #269 collapsed the schema into its first migration, which is
// where the engines differ now: SQLite's 0001 carries two FTS5 tables and
// their triggers, PostgreSQL's two generated tsvectors, MySQL's four FULLTEXT
// keys. The map stays because the next pair that is written half-way is the
// one this catches.
var oneSided = map[string]string{}

func TestBothDriversCarryTheSameMigrations(t *testing.T) {
	t.Parallel()

	lite := shared(names(load(t, "sqlite")))
	pg := shared(names(load(t, "postgres")))
	if !slices.Equal(lite, pg) {
		t.Errorf("the two drivers do not carry the same migrations:\n  sqlite:   %v\n  postgres: %v", lite, pg)
	}
}

// shared drops the migrations one engine has on purpose.
func shared(migrations []string) []string {
	out := make([]string, 0, len(migrations))
	for _, m := range migrations {
		if _, deliberate := oneSided[m]; !deliberate {
			out = append(out, m)
		}
	}
	return out
}

func load(t *testing.T, driver string) []Migration {
	t.Helper()
	// Tests run with the package directory as the working directory, so the
	// driver's own migrations/ sits one level down.
	got, err := loadMigrations(os.DirFS(driver))
	if err != nil {
		t.Fatalf("loading the %s migrations: %v", driver, err)
	}
	return got
}

func names(migrations []Migration) []string {
	out := make([]string, 0, len(migrations))
	for _, m := range migrations {
		out = append(out, fmt.Sprintf("%04d_%s.sql", m.Version, m.Name))
	}
	return out
}
