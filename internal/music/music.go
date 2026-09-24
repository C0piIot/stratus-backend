// Package music owns what an edit to a playlist means.
//
// It exists for the reason internal/files does. Removing by index is a read
// and a write -- the list as it is, then the list without those entries -- and
// the two have to be one transaction or a second client editing the same
// playlist in between is silently undone. An inbound adapter has no
// transaction to put them in, and must not grow one, so the edit lives here
// and the adapter calls it.
//
// Browsing is not here and should not move here: an album is a GROUP BY over
// tags, and reading one is calling db.Music.
package music

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/C0piIot/stratus-backend/internal/db"
)

// ErrIndex is a position that is not in the playlist.
var ErrIndex = errors.New("music: no such position in the playlist")

// Database is what this package needs from the metadata seam.
type Database interface {
	db.Playlists
	TrackByFile(ctx context.Context, owner string, fileID int64) (db.Track, error)
	Tx(ctx context.Context, fn func(db.Repo) error) error
}

// Service is the playlist layer.
type Service struct {
	meta Database
	now  func() time.Time
}

// New wires it to the database.
func New(meta Database) *Service { return &Service{meta: meta, now: time.Now} }

// Playlists lists an owner's playlists.
func (s *Service) Playlists(ctx context.Context, owner string) ([]db.Playlist, error) {
	return s.meta.Playlists(ctx, owner)
}

// Playlist returns one playlist and what it holds. The counts are taken from
// the tracks returned rather than from the row, so the two cannot disagree
// even if the playlist changes between the two reads.
func (s *Service) Playlist(ctx context.Context, owner string, id int64) (db.Playlist, []db.Track, error) {
	p, err := s.meta.PlaylistByID(ctx, owner, id)
	if err != nil {
		return db.Playlist{}, nil, err
	}
	tracks, err := s.meta.PlaylistTracks(ctx, owner, id)
	if err != nil {
		return db.Playlist{}, nil, err
	}
	p.SongCount, p.DurationMS = len(tracks), 0
	for _, t := range tracks {
		p.DurationMS += t.Media.DurationMS
	}
	return p, tracks, nil
}

// Create makes a playlist holding fileIDs, in that order. Every one of them has
// to be a track the owner has, or nothing is made.
func (s *Service) Create(ctx context.Context, owner, name string, fileIDs []int64) (db.Playlist, error) {
	now := s.now()
	var id int64
	err := s.meta.Tx(ctx, func(r db.Repo) error {
		if err := checkTracks(ctx, r, owner, fileIDs); err != nil {
			return err
		}
		p, err := r.CreatePlaylist(ctx, db.Playlist{OwnerID: owner, Name: name, Created: now, Changed: now})
		if err != nil {
			return err
		}
		id = p.ID
		return r.SetPlaylistTracks(ctx, owner, id, fileIDs, now)
	})
	if err != nil {
		return db.Playlist{}, err
	}
	p, _, err := s.Playlist(ctx, owner, id)
	return p, err
}

// Replace is createPlaylist given an id: what the playlist holds becomes
// fileIDs, and its name changes when one is given.
func (s *Service) Replace(ctx context.Context, owner string, id int64, name string, fileIDs []int64) (db.Playlist, error) {
	now := s.now()
	err := s.meta.Tx(ctx, func(r db.Repo) error {
		if err := r.LockPlaylist(ctx, owner, id); err != nil {
			return err
		}
		if err := checkTracks(ctx, r, owner, fileIDs); err != nil {
			return err
		}
		if name != "" {
			p, err := r.PlaylistByID(ctx, owner, id)
			if err != nil {
				return err
			}
			p.Name, p.Changed = name, now
			if err := r.UpdatePlaylist(ctx, p); err != nil {
				return err
			}
		}
		return r.SetPlaylistTracks(ctx, owner, id, fileIDs, now)
	})
	if err != nil {
		return db.Playlist{}, err
	}
	p, _, err := s.Playlist(ctx, owner, id)
	return p, err
}

// Edit is one updatePlaylist: any of the three fields, and entries taken out
// and added.
type Edit struct {
	Name, Comment *string
	Public        *bool
	// Remove are indices into the playlist as it was before this edit, in any
	// order, repeats allowed. They are taken out first and Add is appended to
	// what is left, which is the order every client assumes: an index it sends
	// is into the list it last saw, not into one its own additions changed.
	Remove []int
	Add    []int64
}

// Update applies e whole or not at all: an index that is not there or a track
// that is not the owner's changes nothing.
func (s *Service) Update(ctx context.Context, owner string, id int64, e Edit) error {
	now := s.now()
	return s.meta.Tx(ctx, func(r db.Repo) error {
		if err := r.LockPlaylist(ctx, owner, id); err != nil {
			return err
		}

		if len(e.Remove) > 0 || len(e.Add) > 0 {
			// Read inside the lock: the indices are into this list and no other.
			tracks, err := r.PlaylistTracks(ctx, owner, id)
			if err != nil {
				return err
			}
			drop := make(map[int]bool, len(e.Remove))
			for _, i := range e.Remove {
				if i < 0 || i >= len(tracks) {
					return fmt.Errorf("%w: %d of %d", ErrIndex, i, len(tracks))
				}
				drop[i] = true
			}
			if err := checkTracks(ctx, r, owner, e.Add); err != nil {
				return err
			}

			kept := make([]int64, 0, len(tracks)-len(drop)+len(e.Add))
			for i, t := range tracks {
				if !drop[i] {
					kept = append(kept, t.File.ID)
				}
			}
			if err := r.SetPlaylistTracks(ctx, owner, id, append(kept, e.Add...), now); err != nil {
				return err
			}
		}

		if e.Name == nil && e.Comment == nil && e.Public == nil {
			return nil
		}
		p, err := r.PlaylistByID(ctx, owner, id)
		if err != nil {
			return err
		}
		if e.Name != nil {
			p.Name = *e.Name
		}
		if e.Comment != nil {
			p.Comment = *e.Comment
		}
		if e.Public != nil {
			p.Public = *e.Public
		}
		p.Changed = now
		return r.UpdatePlaylist(ctx, p)
	})
}

// Delete removes a playlist. The tracks in it are untouched.
func (s *Service) Delete(ctx context.Context, owner string, id int64) error {
	return s.meta.DeletePlaylist(ctx, owner, id)
}

// checkTracks refuses a file that is not a track the owner has. A foreign key
// would catch a file that is not there, and nothing else: not somebody else's
// file, and not a photograph.
func checkTracks(ctx context.Context, r db.Repo, owner string, fileIDs []int64) error {
	for _, id := range fileIDs {
		t, err := r.TrackByFile(ctx, owner, id)
		if err != nil {
			return err
		}
		if t.Media.Kind != db.KindAudio {
			return fmt.Errorf("file %d is not a track: %w", id, db.ErrNotFound)
		}
	}
	return nil
}
