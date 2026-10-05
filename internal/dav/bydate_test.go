package dav_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/C0piIot/stratus-backend/internal/dav"
	"github.com/C0piIot/stratus-backend/internal/db"
	"github.com/C0piIot/stratus-backend/internal/db/sqlite"
	"github.com/C0piIot/stratus-backend/internal/files"
	"github.com/C0piIot/stratus-backend/internal/storage/disk"
)

const photosPrefix = "/photos/"

type photoRig struct {
	h     http.Handler
	files *files.Service
	meta  *sqlite.Store
}

func photoServer(t *testing.T) *photoRig {
	t.Helper()
	dir := t.TempDir()
	blobs, err := disk.New(filepath.Join(dir, "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = blobs.Close() })
	meta, err := sqlite.New(t.Context(), filepath.Join(dir, "stratus.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = meta.Close() })
	if err := meta.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	service := files.New(blobs, meta)
	return &photoRig{h: withUser(dav.ByDate(photosPrefix, meta, db.KindImage), "edu"), files: service, meta: meta}
}

func (r *photoRig) add(t *testing.T, p, body, mime string, taken time.Time) db.File {
	t.Helper()
	return r.addKind(t, p, body, mime, taken, db.KindImage)
}

// addKind stores a file and the row the indexer would have written for it,
// under the kind that decides which collection it lands in (#215).
func (r *photoRig) addKind(t *testing.T, p, body, mime string, taken time.Time, kind db.Kind) db.File {
	t.Helper()
	if dir := p[:max(strings.LastIndex(p, "/"), 0)]; dir != "" {
		var built string
		for seg := range strings.SplitSeq(dir, "/") {
			built = strings.TrimPrefix(built+"/"+seg, "/")
			if _, err := r.files.Mkdir(t.Context(), "edu", built); err != nil && !errors.Is(err, db.ErrConflict) {
				t.Fatal(err)
			}
		}
	}
	f, err := r.files.Write(t.Context(), "edu", p, strings.NewReader(body), int64(len(body)), mime)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.meta.PutMedia(t.Context(), db.Media{FileID: f.ID, Kind: kind, IndexedAt: time.Now(), Version: 1, TakenAt: taken}); err != nil {
		t.Fatal(err)
	}
	return f
}

var june = time.Date(2024, 6, 15, 10, 0, 0, 0, time.UTC)

// TestPhotosAreFoldersByDate is the mount: years, months, and the photos of a
// month wherever they were filed.
func TestPhotosAreFoldersByDate(t *testing.T) {
	t.Parallel()
	r := photoServer(t)
	r.add(t, "Camera/IMG_0001.JPG", "one", "image/jpeg", june)
	r.add(t, "Holiday/beach.heic", "two", "image/heic", june.AddDate(0, 0, 3))
	r.add(t, "old.jpg", "three", "image/jpeg", time.Date(2019, 1, 2, 0, 0, 0, 0, time.UTC))

	wantHrefs(t, hrefs(t, do(t, r.h, "PROPFIND", "/photos/", "", "Depth", "1")),
		"/photos/", "/photos/2024/", "/photos/2019/")
	wantHrefs(t, hrefs(t, do(t, r.h, "PROPFIND", "/photos/2024/", "", "Depth", "1")),
		"/photos/2024/", "/photos/2024/06/")
	wantHrefs(t, hrefs(t, do(t, r.h, "PROPFIND", "/photos/2024/06/", "", "Depth", "1")),
		"/photos/2024/06/", "/photos/2024/06/IMG_0001.JPG", "/photos/2024/06/beach.heic")
}

// TestAListingIsOneQueryPerMonth: x/net opens every resource a PROPFIND
// lists, which is why nothing under this handler reads bytes at all -- it is
// not given a blob store to read them from. What it must also not do is ask
// the database once per photograph.
func TestAListingIsOneQueryPerMonth(t *testing.T) {
	t.Parallel()
	r := photoServer(t)
	for i := range 20 {
		r.add(t, fmt.Sprintf("p%02d.jpg", i), "x", "image/jpeg", june)
	}
	got := hrefs(t, do(t, r.h, "PROPFIND", "/photos/2024/06/", "", "Depth", "1"))
	if len(got) != 21 {
		t.Fatalf("a month of twenty listed %d hrefs", len(got))
	}
}

// TestTheBytesAreTheOtherHalfs: a GET is answered at this address by the
// browser handler, which the composition root puts in front of this one
// (#279). Unwrapped, this handler says so rather than serving a second
// implementation of the same bytes -- and still advertises the method, because
// the address does answer it.
func TestTheBytesAreTheOtherHalfs(t *testing.T) {
	t.Parallel()
	r := photoServer(t)
	r.add(t, "beach.heic", "the original bytes", "image/heic", june)

	rec := do(t, r.h, http.MethodGet, "/photos/2024/06/beach.heic", "")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET straight to the mount = %d, want 405", rec.Code)
	}
	opts := do(t, r.h, http.MethodOptions, "/photos/", "")
	for _, want := range []string{"GET", "HEAD", "PROPFIND"} {
		if !strings.Contains(opts.Header().Get("Allow"), want) {
			t.Errorf("OPTIONS Allow = %q, want it to name %s", opts.Header().Get("Allow"), want)
		}
	}
}

func TestNamesInAMonthCannotCollide(t *testing.T) {
	t.Parallel()
	r := photoServer(t)
	r.add(t, "A/IMG_0001.JPG", "a", "image/jpeg", june)
	r.add(t, "B/IMG_0001.JPG", "b", "image/jpeg", june)
	r.add(t, "C/img_0001.jpg", "c", "image/jpeg", june)

	wantHrefs(t, hrefs(t, do(t, r.h, "PROPFIND", "/photos/2024/06/", "", "Depth", "1")),
		"/photos/2024/06/", "/photos/2024/06/IMG_0001.JPG", "/photos/2024/06/IMG_0001 (2).JPG",
		"/photos/2024/06/img_0001 (3).jpg")
}

func TestThePhotosMountIsReadOnly(t *testing.T) {
	t.Parallel()
	r := photoServer(t)
	r.add(t, "a.jpg", "a", "image/jpeg", june)

	if rec := do(t, r.h, http.MethodOptions, "/photos/", ""); rec.Header().Get("DAV") != "1" {
		t.Errorf("OPTIONS DAV = %q, want class 1", rec.Header().Get("DAV"))
	}
	for _, method := range []string{"PUT", "DELETE", "MKCOL", "MOVE", "COPY", "PROPPATCH", "LOCK"} {
		if rec := do(t, r.h, method, "/photos/2024/06/a.jpg", "x"); rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s = %d, want 405", method, rec.Code)
		}
	}
	if rec := do(t, r.h, "PROPFIND", "/photos/", ""); rec.Code != http.StatusForbidden {
		t.Errorf("PROPFIND with no Depth = %d, want 403", rec.Code)
	}
}

func TestWhatIsNotThereIsNotFound(t *testing.T) {
	t.Parallel()
	r := photoServer(t)
	r.add(t, "a.jpg", "a", "image/jpeg", june)

	for _, target := range []string{
		"/photos/1999/", "/photos/2024/05/", "/photos/2024/6/", "/photos/2024/13/",
		"/photos/abc/", "/photos/02024/", "/photos/2024/06/b.jpg", "/photos/2024/06/a.jpg/x",
	} {
		if rec := do(t, r.h, "PROPFIND", target, "", "Depth", "0"); rec.Code != http.StatusNotFound {
			t.Errorf("PROPFIND %s = %d, want 404", target, rec.Code)
		}
	}
	if rec := do(t, dav.ByDate(photosPrefix, r.meta, db.KindImage), "PROPFIND", "/photos/", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("with nobody authenticated = %d", rec.Code)
	}
	theirs := withUser(dav.ByDate(photosPrefix, r.meta, db.KindImage), "someone-else")
	wantHrefs(t, hrefs(t, do(t, theirs, "PROPFIND", "/photos/", "", "Depth", "1")), "/photos/")
}

// fakePhotos serves a month of many photos from memory, to cross the batch a
// month is read in without writing thousands of rows.
type fakePhotos struct {
	photos []db.Photo
	fail   string
}

func (f fakePhotos) PhotoMonths(context.Context, string, db.Kind) ([]db.PhotoMonth, error) {
	if f.fail == "PhotoMonths" {
		return nil, errBroken
	}
	return []db.PhotoMonth{db.MonthOf(june)}, nil
}

func (f fakePhotos) PhotoTimeline(_ context.Context, _ string, pf db.PhotoFilter) ([]db.Photo, error) {
	if f.fail == "PhotoTimeline" {
		return nil, errBroken
	}
	var out []db.Photo
	for _, p := range f.photos {
		if !pf.After.AtStart() && p.File.ID >= pf.After.FileID {
			continue
		}
		out = append(out, p)
		if len(out) == pf.Limit {
			break
		}
	}
	return out, nil
}

func TestAMonthIsReadWhole(t *testing.T) {
	t.Parallel()
	var many []db.Photo
	for i := 2500; i > 0; i-- {
		many = append(many, db.Photo{
			Track:  db.Track{File: db.File{ID: int64(i), Path: fmt.Sprintf("p%04d.jpg", i)}},
			SortAt: june,
		})
	}
	h := withUser(dav.ByDate(photosPrefix, fakePhotos{photos: many}, db.KindImage), "edu")
	if got := hrefs(t, do(t, h, "PROPFIND", "/photos/2024/06/", "", "Depth", "1")); len(got) != 2501 {
		t.Errorf("a month of 2500 lists %d entries", len(got))
	}
}

func TestABrokenBackendIsNotANotFound(t *testing.T) {
	t.Parallel()
	for _, call := range []string{"PhotoMonths", "PhotoTimeline"} {
		h := withUser(dav.ByDate(photosPrefix, fakePhotos{fail: call}, db.KindImage), "edu")
		if rec := do(t, h, "PROPFIND", "/photos/2024/06/", "", "Depth", "1"); rec.Code != http.StatusInternalServerError {
			t.Errorf("PROPFIND with %s broken = %d, want 500", call, rec.Code)
		}
	}

}

// TestTheVideosAreTheirOwnCollection is #215: the same handler over the same
// tree, told apart by the kind it was given. Neither collection carries the
// other's files, which is what keeps a film out of somebody's camera roll.
func TestTheVideosAreTheirOwnCollection(t *testing.T) {
	t.Parallel()
	r := photoServer(t)
	r.add(t, "Camera/IMG_0001.JPG", "a still", "image/jpeg", june)
	r.addKind(t, "Camera/VID_0002.MP4", "a recording", "video/mp4", june, db.KindVideo)

	videos := withUser(dav.ByDate("/videos/", r.meta, db.KindVideo), "edu")
	wantHrefs(t, hrefs(t, do(t, videos, "PROPFIND", "/videos/2024/06/", "", "Depth", "1")),
		"/videos/2024/06/", "/videos/2024/06/VID_0002.MP4")

	// And the gallery still holds only the still.
	wantHrefs(t, hrefs(t, do(t, r.h, "PROPFIND", "/photos/2024/06/", "", "Depth", "1")),
		"/photos/2024/06/", "/photos/2024/06/IMG_0001.JPG")

	// A year with only video in it is not a year in the gallery at all.
	if rec := do(t, r.h, "PROPFIND", "/photos/2024/06/VID_0002.MP4", "", "Depth", "0"); rec.Code != http.StatusNotFound {
		t.Errorf("the recording answers under /photos/: %d", rec.Code)
	}
}
