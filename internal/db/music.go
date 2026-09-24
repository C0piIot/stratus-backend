package db

import (
	"context"
	"time"
)

// The music entities are not rows. There is no artists table and no albums
// table: an album is the pairing of an album artist and an album name, which is
// the only identity a set of tags gives it, and these are what a GROUP BY over
// media returns.
//
// That is a decision rather than a stage on the way to one. Derived tables
// would be a second source of truth to rebuild every time a tag is corrected,
// and the query here is always the current tags. It sits behind this port, so
// changing the answer later changes a driver and not a protocol.

// Artist is one album artist in the library.
type Artist struct {
	Name string
	// AlbumCount is what a listing shows beside the name, and what the
	// Subsonic schema requires on an artist.
	AlbumCount int
}

// Album is one album: an album artist and an album name, plus what a listing
// needs without opening it.
type Album struct {
	Artist string
	Name   string

	SongCount int
	// DurationMS is the sum of its tracks.
	DurationMS int64
	Year       int
	// Genre is whatever its tracks say. An album has no genre of its own, so
	// one of theirs is taken, deterministically rather than correctly.
	Genre string
	// Created is when the earliest of its tracks arrived, which is the "added
	// to the library" date a client sorts by.
	Created time.Time
}

// Track is a file and its metadata together, which is the only shape that can
// be played: the tags say what it is and the row says where the bytes are.
//
// Two structs rather than a flattened one, so that neither File nor Media has
// to grow a field for the benefit of the other.
type Track struct {
	File  File
	Media Media
}

// Page bounds a listing. Limit is required: every endpoint that pages has a
// default and a maximum of its own, and a zero here means zero rows, the same
// as PendingMedia.
type Page struct {
	Limit, Offset int
}

// AlbumOrder is how a listing of albums is sorted. There is one for each way a
// client asks to see a library, and no more: the orders that need a play count
// are not here, because nothing records one yet (#195).
type AlbumOrder string

// The orders AlbumList answers.
const (
	// AlbumsByName and AlbumsByArtist are the two alphabetical listings.
	AlbumsByName   AlbumOrder = "name"
	AlbumsByArtist AlbumOrder = "artist"
	// AlbumsByAdded is newest first, by when the earliest track of each album
	// arrived. See Album.Created for why that is a real arrival time.
	AlbumsByAdded AlbumOrder = "added"
	// AlbumsByYear and AlbumsByYearDesc are the same listing in both
	// directions, because a caller asks for either.
	AlbumsByYear     AlbumOrder = "year"
	AlbumsByYearDesc AlbumOrder = "year-desc"
	// AlbumsRandom is a different set on every call, which is what it is for.
	// Paging through it is meaningless and no caller does.
	AlbumsRandom AlbumOrder = "random"
	// AlbumsStarred and AlbumsHighest are filters as well as orders: only the
	// albums starred, most recent first, and only the albums rated, highest
	// first. See Annotations.
	AlbumsStarred AlbumOrder = "starred"
	AlbumsHighest AlbumOrder = "highest"
)

// AlbumFilter is a paged listing of albums.
type AlbumFilter struct {
	Order AlbumOrder
	// Genre keeps the albums with at least one track in it. Empty means every
	// album. An album has no genre of its own -- its tracks do -- so this
	// matches on theirs, which is also what Genres counts.
	Genre string
	// FromYear and ToYear bound the year inclusively, zero for unbounded. They
	// are a range and not a direction: the order says which way to read it.
	FromYear, ToYear int
	Page             Page
}

// TrackOrder is how a listing of tracks is sorted.
type TrackOrder string

// The orders TrackList answers.
const (
	TracksByPath TrackOrder = "path"
	TracksRandom TrackOrder = "random"
)

// TrackFilter is a paged listing of tracks that is not an album and not a
// folder: a genre, or a handful at random.
type TrackFilter struct {
	Order            TrackOrder
	Genre            string
	FromYear, ToYear int
	Page             Page
}

// Genre is one genre in the library, with what it holds. Both counts are what a
// client shows beside the name, and neither is derivable from the other.
type Genre struct {
	Name                  string
	SongCount, AlbumCount int
}

// SearchFilter is one search over the three things a library holds. Each gets
// its own page because a client pages them independently -- that is how it
// walks the whole library for offline use.
//
// An empty Text matches everything. That is not a convenience: a client with no
// query is asking for the library, and answering nothing would break the sync
// every one of them does.
type SearchFilter struct {
	Text                    string
	Artists, Albums, Tracks Page
}

// SearchResult is what one search answers. Three slices rather than three calls
// because it is one action, and because a driver can hold one connection for
// all of it.
type SearchResult struct {
	Artists []Artist
	Albums  []Album
	Tracks  []Track
}

// Music is the repository a music library browses. Separate from Files and
// MediaIndex for the reason the other two are separate: a feature takes the
// interface it uses.
//
// Every query here joins files, and not for the path: media carries no owner,
// so the join is the only way to answer "mine".
type Music interface {
	// Artists lists the album artists that have at least one album, in name
	// order. An artist with no album artist tag and no artist tag is not one.
	Artists(ctx context.Context, owner string) ([]Artist, error)

	// Albums lists albums in artist then name order. An empty artist means
	// every album, which is what an alphabetical listing of the whole library
	// asks for.
	Albums(ctx context.Context, owner, artist string) ([]Album, error)

	// Tracks lists an album's tracks in disc, then track, then path order --
	// the last so that an album whose tags carry no track numbers still comes
	// back in a stable order rather than whatever the database chose.
	Tracks(ctx context.Context, owner, artist, album string) ([]Track, error)

	// TracksIn lists the audio files directly inside dir, in path order: the
	// folder view of the same library, for the clients that browse by folder
	// rather than by tag.
	//
	// It exists so that a directory listing is one query rather than one per
	// child, and it answers only files the indexer has reached. That is the
	// same rule tag browsing follows, which is what keeps the two views of one
	// library from disagreeing.
	TracksIn(ctx context.Context, owner, dir string) ([]Track, error)

	// TrackByFile returns one track, or ErrNotFound. It takes a file id
	// because that is what a stream request carries: the client was given an
	// id that names a row, not a name that names a tag.
	TrackByFile(ctx context.Context, owner string, fileID int64) (Track, error)

	// AlbumList is Albums for a client's home screen: sorted, filtered and
	// paged rather than everything an artist has.
	//
	// A second method rather than options on Albums, because the two answer
	// different questions. Albums is unbounded on purpose -- an artist's own
	// albums must not be truncated by a default -- and adding a limit to it
	// would make zero mean both "all" and "none" depending on the caller.
	AlbumList(ctx context.Context, owner string, f AlbumFilter) ([]Album, error)

	// TrackList is the listing that is neither an album nor a folder: the
	// tracks of a genre, or a few at random. One method because they are one
	// query with a different ORDER BY.
	TrackList(ctx context.Context, owner string, f TrackFilter) ([]Track, error)

	// Genres lists the genres in the library, in name order, with what each
	// holds. A track with no genre tag is in none of them.
	Genres(ctx context.Context, owner string) ([]Genre, error)

	// Search matches artists, albums and tracks by name.
	//
	// Matching is case-insensitive **by construction rather than by SQL**: the
	// comparison is against text this package folded with strings.ToLower, so
	// the answer cannot depend on the engine's own lower(). See Media.Fold.
	Search(ctx context.Context, owner string, f SearchFilter) (SearchResult, error)
}
