package media

import (
	"bytes"
	"errors"
	"image"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/C0piIot/stratus-backend/internal/db"
	"github.com/C0piIot/stratus-backend/internal/files"
	"github.com/C0piIot/stratus-backend/internal/storage"
	"github.com/C0piIot/stratus-backend/internal/storage/storagetest"
)

// TestAPassMakesTheSizesAGridAsksFor is the whole point of making one before
// anybody asks: the two sizes a listing and a grid draw come out of one
// decode, and the row says so afterwards so that no pass comes back to it.
func TestAPassMakesTheSizesAGridAsksFor(t *testing.T) {
	t.Parallel()
	th, service, blobs, meta := thumbsOver(t)
	f := writeImage(t, service, "photos/one.jpg", 400, 400)
	indexAs(t, meta, f, db.KindImage)

	switch state, err := th.Prepare(t.Context(), f); {
	case err != nil:
		t.Fatalf("Prepare: %v", err)
	case state != db.ThumbMade:
		t.Fatalf("Prepare = %v, want the pictures made", state)
	}

	for _, size := range passSizes {
		body, _, err := blobs.Get(t.Context(), thumbKey(f.BlobKey, size), storage.All())
		if err != nil {
			t.Fatalf("the %d picture was not kept: %v", size, err)
		}
		made, err := io.ReadAll(body)
		if err != nil {
			t.Fatal(err)
		}
		_ = body.Close()
		if cfg, _, derr := image.DecodeConfig(bytes.NewReader(made)); derr != nil || cfg.Width != int(size) {
			t.Errorf("the %d picture is %v, %v", size, cfg, derr)
		}
	}

	// The viewer's size stays lazy: it is one photograph at a time, where a
	// wait is a wait for the picture somebody is looking at.
	if _, err := blobs.Stat(t.Context(), thumbKey(f.BlobKey, thumbLarge)); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("the largest size was made ahead too: %v", err)
	}

	states, err := meta.MediaStates(t.Context(), []int64{f.ID})
	if err != nil {
		t.Fatal(err)
	}
	if states[f.ID].Thumb != db.ThumbMade {
		t.Errorf("the row says %+v, want the pictures made", states[f.ID])
	}
	if pending := stillPending(t, meta, db.KindImage); len(pending) != 0 {
		t.Errorf("the pass would come back for %+v", pending)
	}
}

// TestAPassLosesToWhoeverIsLooking is the rule that makes a background pass
// safe to run at all: a decoder taken from somebody who is looking at a page
// is the problem this was built to fix, one layer along.
func TestAPassLosesToWhoeverIsLooking(t *testing.T) {
	t.Parallel()
	th, service, blobs, meta := thumbsOver(t)
	f := writeImage(t, service, "photos/one.jpg", 400, 400)
	indexAs(t, meta, f, db.KindImage)

	// One decoder, and a request is in front of it.
	th.decoding = make(chan struct{}, 1)
	th.decoding <- struct{}{}
	th.inQueue.Add(1)

	if _, err := th.Prepare(t.Context(), f); !errors.Is(err, ErrBusy) {
		t.Errorf("Prepare while a request is decoding = %v, want ErrBusy", err)
	}
	if _, err := blobs.Stat(t.Context(), thumbKey(f.BlobKey, thumbMedium)); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("a picture was made anyway: %v", err)
	}
	// And nothing is written down, so the file is still waiting rather than
	// judged by a pass that never looked at it.
	states, err := meta.MediaStates(t.Context(), []int64{f.ID})
	if err != nil {
		t.Fatal(err)
	}
	if states[f.ID].Thumb != db.ThumbUntried {
		t.Errorf("the row says %+v, want nothing said", states[f.ID])
	}
}

// TestAPassWritesDownWhatThisBuildCannotRead is the one place a pass writes a
// verdict the lazy path does not: a file nothing can decode would otherwise
// stand at the head of every batch for ever, since the queue is ordered by
// date and limited.
func TestAPassWritesDownWhatThisBuildCannotRead(t *testing.T) {
	t.Parallel()
	th, service, blobs, meta := thumbsOver(t)
	// A recording by a name this build does not offer to decode -- .ts is the
	// one that is also TypeScript, which is why it is out of the list.
	f := write(t, service, "films/holiday.ts", []byte("not a recording"), "video/mp2t")
	indexAs(t, meta, f, db.KindVideo)

	switch state, err := th.Prepare(t.Context(), f); {
	case err != nil:
		t.Fatalf("Prepare: %v", err)
	case state != db.ThumbRefused:
		t.Fatalf("Prepare = %v, want it refused", state)
	}
	if _, err := blobs.Stat(t.Context(), verdictKey(f.BlobKey)); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("a note was written about bytes nothing read: %v", err)
	}

	// Left alone while the verdict stands, and offered again once it is stale:
	// what this build can read is a property of the build, so the answer has
	// to expire the way the note in the store does.
	if pending := stillPending(t, meta, db.KindVideo); len(pending) != 0 {
		t.Errorf("the pass would come straight back for %+v", pending)
	}
	stale, err := meta.PendingThumbs(t.Context(), db.KindVideo, files.DerivedGeneration,
		time.Now().Add(time.Hour), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(stale) != 1 {
		t.Errorf("after the verdict went stale the queue has %d files, want the recording back", len(stale))
	}
}

// TestAPassKeepsWhatIsAlreadyThere is the library somebody has already
// browsed: the pictures are in the store and only the row did not know, so
// what it costs is a Stat rather than a decode.
func TestAPassKeepsWhatIsAlreadyThere(t *testing.T) {
	t.Parallel()
	th, service, blobs, meta := thumbsOver(t)
	// Bytes nothing could decode, so a pass that tried would refuse them.
	f := write(t, service, "photos/one.jpg", []byte("not an image at all"), "image/jpeg")
	indexAs(t, meta, f, db.KindImage)
	for _, size := range passSizes {
		if _, err := blobs.Put(t.Context(), thumbKey(f.BlobKey, size), strings.NewReader("a picture"), 9); err != nil {
			t.Fatal(err)
		}
	}

	switch state, err := th.Prepare(t.Context(), f); {
	case err != nil:
		t.Fatalf("Prepare: %v", err)
	case state != db.ThumbMade:
		t.Fatalf("Prepare = %v, want the pictures that are there taken as made", state)
	}
}

// TestAPassSaysNothingAboutBytesItCouldNotReach is #157 on this side: a store
// that will not answer is not a verdict on anybody's photograph, so the row
// is left as it was and the next pass asks again.
func TestAPassSaysNothingAboutBytesItCouldNotReach(t *testing.T) {
	t.Parallel()
	_, service, blobs, meta := thumbsOver(t)
	f := writeImage(t, service, "photos/one.jpg", 400, 400)
	indexAs(t, meta, f, db.KindImage)

	// The store answers about the derived objects and refuses the file
	// itself, which is the shape of a bucket going quiet halfway: what fails
	// is the read the decode is made of.
	unreadable := files.New(storagetest.FailOn(t, blobs, "Get"), meta)
	th := NewThumbs(blobs, unreadable, meta, stubFFmpeg(t), t.TempDir())
	if _, err := th.Prepare(t.Context(), f); !errors.Is(err, storagetest.ErrInjected) {
		t.Errorf("Prepare against a store that will not read = %v, want the failure passed on", err)
	}

	states, err := meta.MediaStates(t.Context(), []int64{f.ID})
	if err != nil {
		t.Fatal(err)
	}
	if states[f.ID].Thumb != db.ThumbUntried {
		t.Errorf("the row says %+v, want nothing said about a file nothing read", states[f.ID])
	}
}

// TestAPassWorksThroughTheLibrary is the batch: photographs and films, what
// has no picture to make left alone, and a second pass with nothing to do.
func TestAPassWorksThroughTheLibrary(t *testing.T) {
	t.Parallel()
	th, service, blobs, meta := thumbsOver(t)

	photos := []db.File{
		writeImage(t, service, "photos/one.jpg", 120, 90),
		writeImage(t, service, "photos/two.jpg", 120, 90),
	}
	for _, f := range photos {
		indexAs(t, meta, f, db.KindImage)
	}
	// A track has a cover rather than a thumbnail, which is a different
	// picture answering a different surface, so no pass offers it.
	track := write(t, service, "music/song.mp3", []byte("not a track either"), "audio/mpeg")
	indexAs(t, meta, track, db.KindAudio)

	made, err := th.PrepareBatch(t.Context(), time.Now())
	if err != nil || made != len(photos) {
		t.Fatalf("PrepareBatch = %d, %v; want the two photographs", made, err)
	}
	for _, f := range photos {
		if _, err := blobs.Stat(t.Context(), thumbKey(f.BlobKey, thumbMedium)); err != nil {
			t.Errorf("%q has no picture: %v", f.Path, err)
		}
	}
	if _, err := blobs.Stat(t.Context(), coverKey(track.BlobKey, thumbMedium)); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("the pass made a cover: %v", err)
	}

	// And the second pass is the one that proves the first wrote down what it
	// did: nothing is left, with no decode to find that out.
	if made, err := th.PrepareBatch(t.Context(), time.Now()); err != nil || made != 0 {
		t.Errorf("a second pass = %d, %v; want nothing left to do", made, err)
	}
}

// TestARefusalIsBelievedForAWeek is the rule every reader of the column
// shares, and the week is the whole of it: a page that stopped offering for
// ever would be the reason nothing ever tried again after an upgrade that can
// read the format.
func TestARefusalIsBelievedForAWeek(t *testing.T) {
	t.Parallel()
	now := time.Now()

	tests := map[string]struct {
		state db.MediaState
		want  bool
	}{
		"nothing has tried":   {db.MediaState{}, false},
		"the picture is made": {db.MediaState{Thumb: db.ThumbMade, ThumbAt: now}, false},
		"refused just now":    {db.MediaState{Thumb: db.ThumbRefused, ThumbAt: now}, true},
		"refused yesterday":   {db.MediaState{Thumb: db.ThumbRefused, ThumbAt: now.AddDate(0, 0, -1)}, true},
		"refused a week ago": {
			db.MediaState{Thumb: db.ThumbRefused, ThumbAt: now.Add(-files.CacheRetention - time.Minute)}, false,
		},
	}
	for name, tc := range tests {
		if got := Refused(tc.state, now); got != tc.want {
			t.Errorf("%s: Refused = %v, want %v", name, got, tc.want)
		}
	}
}

// indexAs writes the media row a picture's verdict hangs off// indexAs writes the media row a picture's verdict hangs off, which is what
// the extractor would have written first: a pass works from the index rather
// than from the tree.
func indexAs(t *testing.T, meta db.MediaIndex, f db.File, kind db.Kind) {
	t.Helper()
	err := meta.PutMedia(t.Context(), db.Media{
		FileID: f.ID, Kind: kind, IndexedAt: time.Now(), Version: Version, ETag: f.ETag,
	})
	if err != nil {
		t.Fatalf("PutMedia(%q): %v", f.Path, err)
	}
}

// stillPending is what the next pass would be offered, with the week in the
// same place the pass puts it.
func stillPending(t *testing.T, meta db.MediaIndex, kind db.Kind) []db.File {
	t.Helper()
	pending, err := meta.PendingThumbs(t.Context(), kind, files.DerivedGeneration,
		time.Now().Add(-files.CacheRetention), 10)
	if err != nil {
		t.Fatalf("PendingThumbs: %v", err)
	}
	return pending
}
