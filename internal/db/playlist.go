package db

import (
	"context"
	"time"
)

// Playlist is one playlist without its tracks, which is what a listing shows.
//
// Unlike an album it is a row: somebody made it, and it has to survive every
// retag of what is in it. What it holds are file ids, so a rename and an
// overwrite keep a track in it and a delete takes it out.
type Playlist struct {
	ID      int64
	OwnerID string
	Name    string
	Comment string
	// Public is kept and reported, and means nothing yet: there is one user.
	// It is here so that sharing does not need a migration to find out what
	// somebody meant.
	Public  bool
	Created time.Time
	Changed time.Time

	// SongCount and DurationMS are computed on read, over the same entries
	// PlaylistTracks answers with, so a listing and the playlist opened from it
	// cannot disagree.
	SongCount  int
	DurationMS int64
}

// Normalize is Playlist the way every driver stores it: times in UTC and to the
// millisecond.
func (p Playlist) Normalize() Playlist {
	p.Created = p.Created.UTC().Truncate(time.Millisecond)
	p.Changed = p.Changed.UTC().Truncate(time.Millisecond)
	return p
}

// Playlists is the repository for playlists. It stores and reads; what an edit
// means -- which index is which, what goes first -- is internal/music's, since
// an edit is a read and a write that have to happen in one transaction.
type Playlists interface {
	// CreatePlaylist stores p with no tracks and returns it with its id.
	CreatePlaylist(ctx context.Context, p Playlist) (Playlist, error)

	// Playlists lists an owner's playlists in name order.
	Playlists(ctx context.Context, owner string) ([]Playlist, error)

	// PlaylistByID returns one playlist, or ErrNotFound. An id that belongs to
	// somebody else is not found either.
	PlaylistByID(ctx context.Context, owner string, id int64) (Playlist, error)

	// LockPlaylist takes the playlist's row for the rest of the transaction it
	// is called in, or answers ErrNotFound. It is what makes a read followed by
	// a rewrite safe: two edits at once would otherwise both read the old list
	// and the second would silently undo the first.
	LockPlaylist(ctx context.Context, owner string, id int64) error

	// PlaylistTracks returns what the playlist holds, in its order, with a
	// track as many times as it was added. Only what the library can play is
	// listed, so an entry whose file is no longer audio is not shown -- and an
	// index a client sends back is into this list, which is why nothing else
	// may compute one.
	PlaylistTracks(ctx context.Context, owner string, id int64) ([]Track, error)

	// UpdatePlaylist writes p's name, comment, public flag and changed time.
	UpdatePlaylist(ctx context.Context, p Playlist) error

	// SetPlaylistTracks replaces what the playlist holds with fileIDs, in that
	// order, and moves its changed time. The whole list rather than an insert
	// and a delete by position, so that nothing below internal/music knows what
	// a position is.
	SetPlaylistTracks(ctx context.Context, owner string, id int64, fileIDs []int64, changed time.Time) error

	// DeletePlaylist removes one and everything in it, or answers ErrNotFound.
	DeletePlaylist(ctx context.Context, owner string, id int64) error
}
