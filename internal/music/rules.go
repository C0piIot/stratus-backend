package music

import (
	"context"
	"fmt"
	"path"

	"github.com/stratus-private-cloud/stratus-backend/internal/db"
)

// The rules every surface that shows music has to agree on, kept once (#362).
// Each was written in two or three adapters before this file, and one of
// them had already drifted.

// M3U8MIME is the type players recognise an .m3u8 by, which is the type of
// the bytes M3U8 makes and therefore belongs beside it.
const M3U8MIME = "audio/x-mpegurl"

// Title is what a track is called: the tag, and the file's name when there
// is none -- a track the indexer has not read, or one whose tags say nothing.
func Title(t db.Track) string {
	if t.Media.Title != "" {
		return t.Media.Title
	}
	return path.Base(t.File.Path)
}

// Tracks is what FirstTrack needs from the library: db.Music's method of that
// name, which every caller already holds.
type Tracks interface {
	Tracks(ctx context.Context, owner, artist, album string) ([]db.Track, error)
}

// FirstTrack is the track an album is asked through when the question is
// about the album and not about a folder -- its cover, first of all. An
// album is a pair of tags and not a place: its first track in the order the
// library gives is a stable choice for as long as that track is there, and a
// folder's first track is not it when the folder holds several albums. An
// album with no tracks is ErrNotFound, since that is what an album with no
// tracks is.
func FirstTrack(ctx context.Context, lib Tracks, owner, artist, album string) (db.File, error) {
	tracks, err := lib.Tracks(ctx, owner, artist, album)
	if err != nil {
		return db.File{}, err
	}
	if len(tracks) == 0 {
		return db.File{}, fmt.Errorf("%w: album %q by %q", db.ErrNotFound, album, artist)
	}
	return tracks[0].File, nil
}
