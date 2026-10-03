package db

import (
	"context"
	"fmt"
)

// Finder is the repository for finding things by the words in them: a file or
// a folder by its name, a track by its title, and the artists, albums and
// photographs the tags and the cameras name.
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
	// Find matches a library against text, and answers every half in one call
	// because a page that shows them all would otherwise make five.
	//
	// **What every driver promises:**
	//
	//   - A whole word of a file's or a folder's name finds it. The name only:
	//     a word that appears in the folders above a file does not find the
	//     file, it finds the folder. The separators a name is made of --
	//     ``._-()[]`` -- are word boundaries, so IMG_0001.JPG is three words.
	//   - The same of a track's **title** finds that track. Not its artist and
	//     not its album: those are answered as an artist and an album of their
	//     own, because a word that is somebody's name matches everything they
	//     ever recorded and two hundred rows are not an answer to it (#262).
	//   - The same of an album artist finds that artist, and of an album title
	//     that album. Neither is a row -- an album is a GROUP BY over tags --
	//     so what resumes them is the name itself.
	//   - The same of a photograph's camera, or the year the camera says it was
	//     taken, finds that photograph. Not where it was taken: a coordinate is
	//     two numbers, and a name for it would be a service this does not have.
	//   - Several words are matched as a phrase: all of them, in the order they
	//     were typed and next to each other. It is what somebody means by
	//     typing two words, and it is the one multi-word meaning all three
	//     engines express without being argued with.
	//   - Case does not matter, at least for ASCII.
	//   - Files, tracks and photographs are ordered by path, which is unique
	//     per owner, so a cursor resumes exactly and two calls agree. Artists
	//     and albums are ordered by the name they are grouped by, which is
	//     unique among them for the same reason.
	//   - Nothing is promised about *how* an engine sorts text, only that it
	//     sorts it the same way twice: a collation is the database's, and a
	//     page resumes where the one before it ended on that same database.
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

// FindFilter is one search over everything in a library that has words in it.
// Each bucket is asked for separately because a page shows them separately and
// walks them separately.
type FindFilter struct {
	Text string
	// Files, Tracks and Photos are the buckets whose rows are files, so they
	// are walked by path. A zero Limit asks for nothing, which is how a caller
	// that wants only some of them says so.
	Files, Tracks, Photos Window
	// Artists and Albums are the two whose rows are tags, so they are walked
	// by name.
	Artists, Albums TagWindow
}

// Window is one page of a bucket of files: where to resume and how much to
// take.
//
// A cursor and not an offset, for the reason ListFilesPage gives -- an offset
// makes the database count past everything it skips, and repeats or drops a row
// when the library changes under the reader. Of Cursor's fields only Path is
// used here, because the ordering is the path and nothing else.
type Window struct {
	After Cursor
	Limit int
}

// Wanted reports whether this bucket was asked for at all.
func (w Window) Wanted() bool { return w.Limit > 0 }

// TagCursor is where a bucket of tags resumes. There is no path to carry: an
// artist and an album are what a GROUP BY returns, so the group key is both
// the ordering and the cursor. An artist uses Artist alone and an album both,
// in that order.
type TagCursor struct{ Artist, Album string }

// TagWindow is one page of artists or of albums.
type TagWindow struct {
	After TagCursor
	Limit int
}

// Wanted reports whether this bucket was asked for at all.
func (w TagWindow) Wanted() bool { return w.Limit > 0 }

// Validate refuses what the dialects would not agree on. A negative limit is
// not "no page": SQLite reads it as no limit at all and PostgreSQL refuses it,
// so a caller's bug would be the whole library on one driver and an error on
// another. Here rather than in each of them, for the reason ValidateLimit is.
func (f FindFilter) Validate() error {
	for _, limit := range []int{f.Files.Limit, f.Tracks.Limit, f.Photos.Limit,
		f.Artists.Limit, f.Albums.Limit} {
		if limit < 0 {
			return fmt.Errorf("db: a page asks for no rows or some, not %d", limit)
		}
	}
	return nil
}

// FindResult is what one search answers.
type FindResult struct {
	Files  []File
	Tracks []Track
	// Artists and Albums are the same shapes Music lists, because they are the
	// same aggregate: one of them found here opens the page that shows the
	// rest of it.
	Artists []Artist
	Albums  []Album
	// Photos are the file rows, not the media ones: what a page shows of a
	// photograph is a thumbnail, and that is made from a path, a size and a
	// validator.
	Photos []File
}
