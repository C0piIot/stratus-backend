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

	// TrackByFile returns one track, or ErrNotFound. It takes a file id
	// because that is what a stream request carries: the client was given an
	// id that names a row, not a name that names a tag.
	TrackByFile(ctx context.Context, owner string, fileID int64) (Track, error)
}
