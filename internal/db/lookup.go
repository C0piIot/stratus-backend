package db

import (
	"context"
	"time"
)

// What outside sources said about the music library (#217): the identity of an
// album on MusicBrainz, the front cover the Cover Art Archive holds for it, and
// the words of a track from LRCLIB. Rows, not tags: the user's files are never
// rewritten, and what was fetched is kept beside the library the way stars are
// -- an album's answer keyed by the tags that make it one, a track's by its
// file.
//
// Every row is also a verdict with a date on it. A source that has nothing for
// an album today may have it next month, and a source that did not answer says
// nothing about the album at all, so each row says when to ask again: a month
// for "nothing there", an hour for "could not reach it", never for an answer
// that was found. The pass reads that date and nothing else, which is what
// makes the queue a query like the indexer's rather than a table.

// AlbumLookup is what was found about one album.
type AlbumLookup struct {
	Owner, Artist, Album string
	// LookedUpAt is when the sources were last asked.
	LookedUpAt time.Time
	// RetryAt is when to ask again, and zero for never: an answer that was
	// found is kept until something loses it.
	RetryAt time.Time
	// ReleaseGroup is the MusicBrainz release-group id, empty when none was
	// found. A release group is the album as a work -- every pressing of it --
	// which is what the Cover Art Archive files a front cover under.
	ReleaseGroup string
	// CoverBlob is the blob key of the track the fetched front cover was filed
	// under, as a derived object, and empty when none was fetched. Under a
	// track rather than keyed by the album because the sweep collects derived
	// objects by their parent: delete the track and the picture goes with it.
	CoverBlob string
	// Error is why the last attempt did not settle the row, in words, for
	// whoever reads the table.
	Error string
	// SearchVersion is which search wrote the row. A row that found nothing
	// is asked again when the search has moved on, the way media.Version
	// puts a library back in front of a better extractor; a row that found
	// an id keeps it whatever the version says.
	SearchVersion int
}

// AlbumCandidate is an album the pass should look at next, with what the
// tags already know: a library tagged by Picard carries the MusicBrainz ids,
// and an id in hand is a search that need not be made.
type AlbumCandidate struct {
	Owner, Artist, Album  string
	ReleaseGroup, Release string
}

// Lyrics is what was found about one track's words.
type Lyrics struct {
	FileID int64
	// ETag is the file's validator when the words were fetched. A replaced
	// file keeps its id, and the words were looked up by the tags the old
	// bytes carried.
	ETag      string
	FetchedAt time.Time
	// RetryAt is when to ask again, zero for never.
	RetryAt time.Time
	// Plain is the text, Synced the same text with a time in front of each
	// line in LRC form, and either is empty when the source has none.
	// Instrumental says the track has no words, which is an answer too.
	Plain, Synced string
	Instrumental  bool
	Error         string
}

// Found reports whether the source answered, which includes answering that
// there is nothing to sing.
func (l Lyrics) Found() bool { return l.Plain != "" || l.Synced != "" || l.Instrumental }

// LookupCounts is where the library stands, for the page that reports it.
type LookupCounts struct {
	// Albums is how many the library has, AlbumsTried how many have been
	// asked about at all, Identified how many were found on MusicBrainz and
	// Covers how many have a fetched front cover.
	Albums, AlbumsTried, Identified, Covers int64
	// Tracks is how many carry the tags a lookup needs, TracksTried how many
	// have been asked about, and Lyrics how many have an answer -- words, or
	// that there are none.
	Tracks, TracksTried, Lyrics int64
}

// Lookups is the repository for what outside sources said. Separate from Music
// because that one only reads tags, and from Annotations because nobody said
// this: it was fetched, and it can be fetched again.
type Lookups interface {
	// PendingAlbumLookups returns the albums the pass should ask about next:
	// those with no row, those whose row says to ask again by now, and those
	// whose row found nothing under a search older than version. An album
	// is the pairing Music groups by, so the ids come from whichever of its
	// tracks carry one.
	PendingAlbumLookups(ctx context.Context, version int, now time.Time, limit int) ([]AlbumCandidate, error)

	// PutAlbumLookup writes what was found, replacing any earlier row.
	PutAlbumLookup(ctx context.Context, l AlbumLookup) error

	// AlbumLookup reads one album's row, or ErrNotFound.
	AlbumLookup(ctx context.Context, owner, artist, album string) (AlbumLookup, error)

	// ForgetCover says the picture a row names is gone -- its track was
	// replaced or deleted -- so that the pass fetches it again from retryAt.
	// Not an error for a row that is not there.
	ForgetCover(ctx context.Context, owner, artist, album string, retryAt time.Time) error

	// PruneAlbumLookups deletes the rows of albums the library no longer has
	// and reports how many. A retag makes a different album as far as
	// anything here can tell, and the old row would otherwise stay for good.
	PruneAlbumLookups(ctx context.Context) (int64, error)

	// PendingLyrics returns the tracks the pass should ask about next: those
	// with a title and an artist and no row, those whose file has been
	// replaced since, and those whose row says to ask again by now.
	PendingLyrics(ctx context.Context, now time.Time, limit int) ([]Track, error)

	// PutLyrics writes what was found, replacing any earlier row. A file that
	// is not there is ErrNotFound.
	PutLyrics(ctx context.Context, l Lyrics) error

	// LyricsOf reads the rows of the files named, for the ones that have
	// any. One query for a page, like AnnotationsOf.
	LyricsOf(ctx context.Context, fileIDs []int64) (map[int64]Lyrics, error)

	// LookupCounts says where the library stands.
	LookupCounts(ctx context.Context) (LookupCounts, error)
}
