package db

import (
	"context"
	"fmt"
)

// Finder is the repository for finding things by the words in them: a file or
// a folder by its name, a track by its tags.
//
// Separate from Files and from Music because it is neither -- it answers across
// both -- and because what it costs is each driver's own problem. **How a driver
// matches text is not part of this port.** One keeps a full-text index, another
// a generated column, another neither; what they all owe is the floor below,
// and internal/db/dbtest is where that floor is held.
//
// Music.Search stays beside this and is not the same thing: that one is what
// OpenSubsonic's search3 answers from, with its own pages and its own meaning,
// and a client depends on it.
type Finder interface {
	// Find matches files and tracks against text, and answers both in one call
	// because a page that shows both would otherwise make two.
	//
	// **What every driver promises:**
	//
	//   - A whole word of a file's or a folder's name finds it. The name only:
	//     a word that appears in the folders above a file does not find the
	//     file, it finds the folder. The separators a name is made of --
	//     ``._-()[]`` -- are word boundaries, so IMG_0001.JPG is three words.
	//   - The same of a track's title, artist, album or album artist finds that
	//     track.
	//   - Several words are matched as a phrase: all of them, in the order they
	//     were typed and next to each other. It is what somebody means by
	//     typing two words, and it is the one multi-word meaning all three
	//     engines express without being argued with.
	//   - Case does not matter, at least for ASCII.
	//   - Results are ordered by path, which is unique per owner, so a cursor
	//     resumes exactly and two calls agree.
	//   - An empty Text matches nothing. Unlike Music.Search, where a client
	//     with no query is asking for the library, a person with an empty box
	//     is asking for nothing.
	//
	// **What no driver promises:** ranking, partial words, substrings, accents
	// folded, or anything about word order. A driver whose engine gives one of
	// those for free gives it; nothing may depend on it.
	//
	// Ordering by path rather than by relevance is what makes the cursor
	// possible at all. Relevance differs per engine, is not comparable between
	// them, and moves when an index is rebuilt -- there is no stable key in it
	// to resume from.
	Find(ctx context.Context, owner string, f FindFilter) (FindResult, error)
}

// FindFilter is one search over the two things a library holds that have words
// in them. Each half is asked for separately because a page shows them
// separately and walks them separately.
type FindFilter struct {
	Text string
	// Files and Tracks are the two halves. A zero Limit asks for nothing, which
	// is how a caller that only wants one of them says so.
	Files, Tracks Window
}

// Window is one page of one half: where to resume and how much to take.
//
// A cursor and not an offset, for the reason ListFilesPage gives -- an offset
// makes the database count past everything it skips, and repeats or drops a row
// when the library changes under the reader. Of Cursor's fields only Path is
// used here, because the ordering is the path and nothing else.
type Window struct {
	After Cursor
	Limit int
}

// Wanted reports whether this half was asked for at all.
func (w Window) Wanted() bool { return w.Limit > 0 }

// Validate refuses what the dialects would not agree on. A negative limit is
// not "no page": SQLite reads it as no limit at all and PostgreSQL refuses it,
// so a caller's bug would be the whole library on one driver and an error on
// another. Here rather than in each of them, for the reason ValidateLimit is.
func (f FindFilter) Validate() error {
	for _, w := range []Window{f.Files, f.Tracks} {
		if w.Limit < 0 {
			return fmt.Errorf("db: a page asks for no rows or some, not %d", w.Limit)
		}
	}
	return nil
}

// FindResult is what one search answers.
type FindResult struct {
	Files  []File
	Tracks []Track
}
