package web

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/C0piIot/stratus-backend/internal/db"
)

// The listing's cursor on the wire (#251): the last row of the page before,
// in the clear, with a letter in front of it saying which of the two groups
// the ordering had reached (#365).

// encodeCursor and parseCursor carry a position in a listing through a URL.
//
// Not opaque and not signed: it is a path the browser already has and is
// allowed to see. The letter in front of it is the half a path cannot say --
// which of the two groups the ordering had reached -- and without it a page
// boundary that falls between the directories and the files could not be
// resumed. An ordering that reads a column other than the path carries that
// column's value too, between the two, for exactly the same reason: it is the
// other half of the key, and a cursor missing it would resume in the wrong
// place among equal names.
func encodeCursor(key db.FileSortKey, c db.Cursor) string {
	kind := "f/"
	if c.IsDir {
		kind = "d/"
	}
	switch key {
	case db.SortSize:
		return kind + strconv.FormatInt(c.Size, 10) + "/" + c.Path
	case db.SortMTime:
		return kind + strconv.FormatInt(c.MTime.UnixMilli(), 10) + "/" + c.Path
	default:
		return kind + c.Path
	}
}

func parseCursor(key db.FileSortKey, v string) (db.Cursor, error) {
	if v == "" {
		return db.Cursor{}, nil
	}
	kind, rest, ok := strings.Cut(v, "/")
	if !ok {
		return db.Cursor{}, fmt.Errorf("cursor %q: no kind", v)
	}
	var c db.Cursor
	switch kind {
	case "d":
		c.IsDir = true
	case "f":
	default:
		return db.Cursor{}, fmt.Errorf("cursor %q: unknown kind %q", v, kind)
	}

	// A cursor is read in the shape the ordering names, so one made under
	// another ordering is refused rather than half understood.
	switch key {
	case db.SortSize, db.SortMTime:
		value, p, found := strings.Cut(rest, "/")
		if !found {
			return db.Cursor{}, fmt.Errorf("cursor %q: no sort value", v)
		}
		n, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return db.Cursor{}, fmt.Errorf("cursor %q: %w", v, err)
		}
		if key == db.SortSize {
			c.Size = n
		} else {
			c.MTime = time.UnixMilli(n).UTC()
		}
		rest = p
	}

	if err := db.ValidatePath(rest); err != nil {
		return db.Cursor{}, err
	}
	c.Path = rest
	return c, nil
}
