package sqlutil

import (
	"database/sql/driver"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// Times is how a driver puts a time on the wire and reads one back (#360).
//
// Two of the three engines keep milliseconds in an integer and the third a
// TIMESTAMPTZ, and that was the one difference between their scanners: sixteen
// functions per driver, byte for byte the same text apart from the time
// conversion. The dialect is still the driver's -- it names its codec once --
// and what moves below the line is the port entity's shape, which is the
// same by definition.
type Times interface {
	// Arg is a time as a bind parameter.
	Arg(t time.Time) any
	// NullArg is Arg for a time that may be zero, which is NULL on the wire.
	NullArg(t time.Time) any
	// Dest is where a scan writes a time: into *t, in UTC. A NULL is an
	// error, since the column is NOT NULL.
	Dest(t *time.Time) any
	// NullDest is Dest for a nullable column, leaving *t zero on NULL.
	NullDest(t *time.Time) any
}

// Millis keeps a time as milliseconds since the epoch in an integer column,
// which is what SQLite and MySQL do here.
type Millis struct{}

// Arg implements Times.
func (Millis) Arg(t time.Time) any { return t.UnixMilli() }

// NullArg implements Times.
func (Millis) NullArg(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UnixMilli()
}

// Dest implements Times.
func (Millis) Dest(t *time.Time) any { return &millisDest{t: t} }

// NullDest implements Times.
func (Millis) NullDest(t *time.Time) any { return &millisDest{t: t, nullable: true} }

type millisDest struct {
	t        *time.Time
	nullable bool
}

// Scan implements sql.Scanner. An integer arrives as int64 from a prepared
// statement and as its digits from MySQL's text protocol, and both are taken.
func (d *millisDest) Scan(v any) error {
	switch x := v.(type) {
	case nil:
		if !d.nullable {
			return errors.New("sqlutil: NULL in a time column that cannot hold one")
		}
		*d.t = time.Time{}
		return nil
	case int64:
		*d.t = time.UnixMilli(x).UTC()
		return nil
	case []byte:
		ms, err := strconv.ParseInt(string(x), 10, 64)
		if err != nil {
			return fmt.Errorf("sqlutil: %q is not a time in milliseconds", x)
		}
		*d.t = time.UnixMilli(ms).UTC()
		return nil
	}
	return fmt.Errorf("sqlutil: %T is not a time in milliseconds", v)
}

// Timestamps keeps a time in a column of the engine's own time type, which is
// what PostgreSQL does here.
type Timestamps struct{}

// Arg implements Times.
func (Timestamps) Arg(t time.Time) any { return t }

// NullArg implements Times.
func (Timestamps) NullArg(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}

// Dest implements Times.
func (Timestamps) Dest(t *time.Time) any { return &stampDest{t: t} }

// NullDest implements Times.
func (Timestamps) NullDest(t *time.Time) any { return &stampDest{t: t, nullable: true} }

type stampDest struct {
	t        *time.Time
	nullable bool
}

// Scan implements sql.Scanner.
func (d *stampDest) Scan(v any) error {
	switch x := v.(type) {
	case nil:
		if !d.nullable {
			return errors.New("sqlutil: NULL in a time column that cannot hold one")
		}
		*d.t = time.Time{}
		return nil
	case time.Time:
		*d.t = x.UTC()
		return nil
	}
	return fmt.Errorf("sqlutil: %T is not a timestamp", v)
}

// Both are driver.Valuer-free on purpose: the argument side hands the engine
// a plain int64 or time.Time, which every driver already binds.
var _ driver.Valuer = (driver.Valuer)(nil)
