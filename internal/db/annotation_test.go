package db_test

import (
	"errors"
	"testing"

	"github.com/C0piIot/stratus-backend/internal/db"
)

func TestSubjectValidate(t *testing.T) {
	t.Parallel()
	for _, s := range []db.Subject{
		db.TrackSubject(1),
		db.AlbumSubject("Autechre", "Tri Repetae"),
		db.ArtistSubject("Autechre"),
	} {
		if err := s.Validate(); err != nil {
			t.Errorf("%+v.Validate() = %v, want what a constructor built to be valid", s, err)
		}
	}

	for _, s := range []db.Subject{
		{},
		db.TrackSubject(0),
		{Kind: db.SubjectTrack, FileID: 1, Artist: "x"},
		db.AlbumSubject("", "Tri Repetae"),
		db.AlbumSubject("Autechre", ""),
		{Kind: db.SubjectAlbum, FileID: 1, Artist: "a", Album: "b"},
		db.ArtistSubject(""),
		{Kind: db.SubjectArtist, Artist: "a", Album: "b"},
		{Kind: "playlist", Artist: "a"},
	} {
		if err := s.Validate(); err == nil {
			t.Errorf("%+v.Validate() = nil, want a refusal", s)
		}
	}
}

func TestValidateRating(t *testing.T) {
	t.Parallel()
	for rating := range db.MaxRating + 1 {
		if err := db.ValidateRating(rating); err != nil {
			t.Errorf("ValidateRating(%d) = %v", rating, err)
		}
	}
	for _, rating := range []int{-1, db.MaxRating + 1} {
		if err := db.ValidateRating(rating); !errors.Is(err, db.ErrInvalidRating) {
			t.Errorf("ValidateRating(%d) = %v, want ErrInvalidRating", rating, err)
		}
	}
}
