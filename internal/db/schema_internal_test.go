package db

import (
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
)

// The two migration directories are written by hand, one per dialect, and until
// this test nothing noticed when a column landed in one and not the other. That
// bug looks like a green test run and a failure in production on the other
// driver, and it has already happened once: the Postgres schema declared
// `size INTEGER`, which is 32 bits there, and capped every file at 2 GB.
//
// Two things this check deliberately does not do.
//
// It does not compare types. That is the one thing the dialects legitimately
// disagree on, and comparing it is both noisy and useless for the bug above:
// `size INTEGER` is the same text in both and 32 bits in only one. Worse,
// SQLite's INTEGER stands in for BIGINT, TIMESTAMPTZ and BOOLEAN, so any
// equivalence table has to accept INTEGER against almost anything and says
// nothing where it matters. Width and precision are pinned from the other side,
// by the round-trip cases in internal/db/dbtest.
//
// And it does not open a database. It reads the directories from disk rather
// than either driver's embed, because a driver imports this package and an
// internal test here therefore cannot import a driver. Reading text also means
// it runs under `make test` and `make test-race`, where the Postgres suite
// skips for want of STRATUS_TEST_POSTGRES_DSN.

func TestMigrationSetsAgree(t *testing.T) {
	t.Parallel()

	lite := load(t, "sqlite")
	pg := load(t, "postgres")

	// The worst divergence of all, and the cheapest to catch: a migration only
	// one driver has. Migrate decides what is applied from MAX(version), so a
	// 0004 that exists only in sqlite leaves the Postgres database a version
	// behind with nothing saying so.
	if !slices.Equal(names(lite), names(pg)) {
		t.Fatalf("the two drivers do not carry the same migrations:\n  sqlite:   %v\n  postgres: %v",
			names(lite), names(pg))
	}

	for i := range lite {
		file := names(lite)[i]
		a := canonical(t, file+" (sqlite)", lite[i].SQL)
		b := canonical(t, file+" (postgres)", pg[i].SQL)

		if len(a) != len(b) {
			t.Errorf("%s: %d statements in sqlite and %d in postgres", file, len(a), len(b))
			continue
		}
		for j := range a {
			compare(t, file, a[j], b[j])
		}
	}
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

// statement is a migration statement with everything dialect-specific removed:
// what it creates, what it is called, and its columns in order.
type statement struct {
	kind    string // "table", "index" or "added column"
	name    string // the table, or the index
	on      string // the table an index is on
	unique  bool
	columns []string
}

func canonical(t *testing.T, where, sql string) []statement {
	t.Helper()
	var out []statement
	for _, s := range statements(sql) {
		out = append(out, parse(t, where, s))
	}
	return out
}

// parse is strict on purpose. It reads three small files that this project
// writes, so a statement it does not recognise is a reason to fail loudly and
// either teach it the shape or keep the migration simple -- the same bargain
// statements() already strikes with its semicolon split.
func parse(t *testing.T, where, s string) statement {
	t.Helper()
	flat := strings.Join(strings.Fields(strings.ToLower(s)), " ")

	switch {
	case strings.HasPrefix(flat, "create table "):
		head, body, ok := cutParens(flat)
		if !ok {
			break
		}
		return statement{
			kind:    "table",
			name:    strings.TrimSpace(strings.TrimPrefix(head, "create table ")),
			columns: columns(splitTop(body)),
		}

	case strings.HasPrefix(flat, "create index "), strings.HasPrefix(flat, "create unique index "):
		head, body, ok := cutParens(flat)
		if !ok {
			break
		}
		// head is "create [unique] index <name> on <table> "
		words := strings.Fields(head)
		if len(words) < 5 || words[len(words)-2] != "on" {
			break
		}
		return statement{
			kind:    "index",
			name:    words[len(words)-3],
			on:      words[len(words)-1],
			unique:  strings.HasPrefix(flat, "create unique index "),
			columns: trimAll(splitTop(body)),
		}

	case strings.HasPrefix(flat, "alter table "):
		table, added, ok := strings.Cut(strings.TrimPrefix(flat, "alter table "), " add column ")
		if !ok {
			break
		}
		return statement{
			kind:    "added column",
			name:    strings.TrimSpace(table),
			columns: columns([]string{added}),
		}
	}

	t.Fatalf("%s: this check does not understand %q.\n"+
		"Teach parse() the shape, or keep the migration to the shapes it knows.", where, s)
	return statement{}
}

// columns canonicalises a column definition: its name, then what is left after
// the dialect is taken out.
func columns(defs []string) []string {
	out := make([]string, 0, len(defs))
	for _, def := range defs {
		words := strings.Fields(def)
		if len(words) == 0 {
			continue
		}

		canon := []string{words[0]}
		for _, w := range words[1:] {
			switch w {
			// The type, in every spelling either driver uses. Dropped rather
			// than mapped: see the note at the top of this file.
			case "integer", "bigint", "bigserial", "text", "timestamptz",
				"boolean", "real", "double", "precision":
			// How SQLite spells what BIGSERIAL does on its own.
			case "autoincrement":
			// The boolean literals, which is the same column either way.
			case "false":
				canon = append(canon, "0")
			case "true":
				canon = append(canon, "1")
			default:
				canon = append(canon, w)
			}
		}
		out = append(out, strings.Join(canon, " "))
	}
	return out
}

// cutParens splits on the outermost parentheses: everything before the first
// one, and everything inside it.
func cutParens(s string) (head, body string, ok bool) {
	open := strings.Index(s, "(")
	close := strings.LastIndex(s, ")")
	if open < 0 || close < open {
		return "", "", false
	}
	return s[:open], s[open+1 : close], true
}

// splitTop splits on commas outside parentheses, so a REFERENCES clause or a
// type with a precision does not split a column in half.
func splitTop(s string) []string {
	var out []string
	depth, start := 0, 0
	for i, r := range s {
		switch r {
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				out = append(out, s[start:i])
				start = i + 1
			}
		}
	}
	return append(out, s[start:])
}

func trimAll(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		out = append(out, strings.TrimSpace(s))
	}
	return out
}

func compare(t *testing.T, file string, a, b statement) {
	t.Helper()

	if a.kind != b.kind || a.name != b.name || a.on != b.on || a.unique != b.unique {
		t.Errorf("%s: sqlite declares %s and postgres declares %s", file, describe(a), describe(b))
		return
	}

	if len(a.columns) != len(b.columns) {
		t.Errorf("%s: %s %s has %d columns in sqlite and %d in postgres\n"+
			"  only in sqlite:   %v\n  only in postgres: %v",
			file, a.kind, a.name, len(a.columns), len(b.columns),
			only(a.columns, b.columns), only(b.columns, a.columns))
		return
	}

	for i := range a.columns {
		if a.columns[i] != b.columns[i] {
			t.Errorf("%s: %s %s, column %d: sqlite has %q and postgres has %q",
				file, a.kind, a.name, i+1, a.columns[i], b.columns[i])
		}
	}
}

func describe(s statement) string {
	if s.kind == "index" {
		if s.unique {
			return fmt.Sprintf("unique index %s on %s", s.name, s.on)
		}
		return fmt.Sprintf("index %s on %s", s.name, s.on)
	}
	return fmt.Sprintf("%s %s", s.kind, s.name)
}

// only names the columns present in a and not in b, which is what makes a
// dropped or renamed column readable instead of an off-by-one in a count.
func only(a, b []string) []string {
	have := make(map[string]bool, len(b))
	for _, s := range b {
		have[first(s)] = true
	}
	var out []string
	for _, s := range a {
		if !have[first(s)] {
			out = append(out, first(s))
		}
	}
	return out
}

func first(s string) string {
	name, _, _ := strings.Cut(strings.TrimSpace(s), " ")
	return name
}
