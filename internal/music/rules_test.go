package music_test

import (
	"context"
	"errors"
	"testing"

	"github.com/C0piIot/stratus-backend/internal/db"
	"github.com/C0piIot/stratus-backend/internal/music"
)

func TestTitleIsTheTagOrTheName(t *testing.T) {
	t.Parallel()
	tagged := db.Track{File: db.File{Path: "music/01.flac"}, Media: db.Media{Title: "Foil"}}
	if got := music.Title(tagged); got != "Foil" {
		t.Errorf("Title = %q, want the tag", got)
	}
	bare := db.Track{File: db.File{Path: "music/amber/01.flac"}}
	if got := music.Title(bare); got != "01.flac" {
		t.Errorf("Title = %q, want the file's name", got)
	}
}

// albumOf answers one album's tracks and nothing else.
type albumOf []db.Track

func (a albumOf) Tracks(context.Context, string, string, string) ([]db.Track, error) { return a, nil }

type refusing struct{}

func (refusing) Tracks(context.Context, string, string, string) ([]db.Track, error) {
	return nil, errors.New("the backend is on fire")
}

func TestFirstTrackIsTheAlbumsOwn(t *testing.T) {
	t.Parallel()
	first := db.File{ID: 7, Path: "music/b.flac"}
	lib := albumOf{{File: first}, {File: db.File{ID: 8, Path: "music/a.flac"}}}
	got, err := music.FirstTrack(t.Context(), lib, "edu", "Autechre", "Amber")
	if err != nil || got.ID != first.ID {
		t.Errorf("FirstTrack = %+v, %v; want the first in the library's order, not the path's", got, err)
	}
	if _, err := music.FirstTrack(t.Context(), albumOf{}, "edu", "Autechre", "Nothing"); !errors.Is(err, db.ErrNotFound) {
		t.Errorf("FirstTrack of no tracks = %v, want ErrNotFound", err)
	}
	if _, err := music.FirstTrack(t.Context(), refusing{}, "edu", "Autechre", "Amber"); err == nil || errors.Is(err, db.ErrNotFound) {
		t.Errorf("FirstTrack over a library that refuses = %v, want its error", err)
	}
}
