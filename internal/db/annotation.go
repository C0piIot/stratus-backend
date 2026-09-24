package db

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// ErrInvalidRating is a rating outside 0 to MaxRating.
var ErrInvalidRating = errors.New("db: invalid rating")

// MaxRating is the top of the scale. Five, because that is the protocol's, and
// no other surface rates anything.
const MaxRating = 5

// SubjectKind is what an annotation is about.
type SubjectKind string

// The three things a user can star or rate.
const (
	SubjectTrack  SubjectKind = "track"
	SubjectAlbum  SubjectKind = "album"
	SubjectArtist SubjectKind = "artist"
)

// Subject is the thing an annotation is attached to.
//
// A track is a row, so it is named by its file id and goes when the file does.
// An album and an artist are not rows -- see music.go -- so they are named by
// the tags that make them one, and an annotation on them outlives the tracks:
// it is there again if the album comes back. What it does not survive is a
// retag, which makes a different album as far as anything here can tell. That
// is the price of having no derived tables, and it is paid knowingly.
//
// Comparable, so a caller can key a map on it. Build one with the three
// constructors below rather than by hand: AnnotationsOf answers with the
// subjects it read back, and a hand-built one with a stray field set would
// never match.
type Subject struct {
	Kind   SubjectKind
	FileID int64
	// Artist is the album artist for an album, and the name for an artist.
	Artist string
	Album  string
}

// TrackSubject names a track by the file it is.
func TrackSubject(fileID int64) Subject { return Subject{Kind: SubjectTrack, FileID: fileID} }

// AlbumSubject names an album the way the library does: its album artist and
// its name.
func AlbumSubject(artist, album string) Subject {
	return Subject{Kind: SubjectAlbum, Artist: artist, Album: album}
}

// ArtistSubject names an album artist.
func ArtistSubject(name string) Subject { return Subject{Kind: SubjectArtist, Artist: name} }

// Validate refuses a subject no constructor would have built.
func (s Subject) Validate() error {
	ok := false
	switch s.Kind {
	case SubjectTrack:
		ok = s.FileID > 0 && s.Artist == "" && s.Album == ""
	case SubjectAlbum:
		ok = s.FileID == 0 && s.Artist != "" && s.Album != ""
	case SubjectArtist:
		ok = s.FileID == 0 && s.Artist != "" && s.Album == ""
	}
	if !ok {
		return fmt.Errorf("db: invalid subject %+v", s)
	}
	return nil
}

// ValidateRating refuses a rating off the scale. Zero is on it: it is how a
// rating is taken away.
func ValidateRating(rating int) error {
	if rating < 0 || rating > MaxRating {
		return fmt.Errorf("%w: %d is not between 0 and %d", ErrInvalidRating, rating, MaxRating)
	}
	return nil
}

// Annotation is what one user has said about one subject. The zero value is
// the answer for a subject nobody has touched.
type Annotation struct {
	// Starred is when it was starred, and zero when it is not.
	Starred time.Time
	// Rating is 1 to MaxRating, and 0 for none.
	Rating int
}

// StarredItems is everything a user has starred that the library still holds.
type StarredItems struct {
	Artists []Artist
	Albums  []Album
	Tracks  []Track
}

// Annotations is the repository for what a user has said about their library:
// stars and ratings. Separate from Music because that one only reads tags, and
// this is the first thing about music that is written by somebody.
//
// Every write is idempotent. A client retries, and starring twice or clearing
// a rating nobody gave is the same answer as doing it once.
type Annotations interface {
	// Star marks s, at the time given. Starring what is already starred keeps
	// the first time: nothing new happened. A track whose file is not there is
	// ErrNotFound; an album or an artist is not checked, because nothing here
	// can tell a tag nobody has from one that has not been indexed yet.
	Star(ctx context.Context, owner string, s Subject, at time.Time) error

	// Unstar takes the star away, and is not an error when there is none.
	Unstar(ctx context.Context, owner string, s Subject) error

	// SetRating rates s, and 0 takes a rating away. Off the scale is
	// ErrInvalidRating.
	SetRating(ctx context.Context, owner string, s Subject, rating int) error

	// AnnotationsOf reads the annotations of the subjects named, for the ones
	// that have any. It exists so that decorating a whole response costs a
	// query rather than one per row, and like MediaStates the caller is what
	// bounds the list.
	AnnotationsOf(ctx context.Context, owner string, subjects []Subject) (map[Subject]Annotation, error)

	// Starred lists what is starred and still in the library, most recently
	// starred first. An album whose tracks are gone is not listed, and is
	// again if they come back.
	Starred(ctx context.Context, owner string) (StarredItems, error)
}
