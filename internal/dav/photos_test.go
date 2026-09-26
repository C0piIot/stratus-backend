package dav_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/C0piIot/stratus-backend/internal/dav"
	"github.com/C0piIot/stratus-backend/internal/db"
	"github.com/C0piIot/stratus-backend/internal/db/sqlite"
	"github.com/C0piIot/stratus-backend/internal/files"
	"github.com/C0piIot/stratus-backend/internal/storage/disk"
)

const photosPrefix = "/photos/"

// countingOpener is the file layer with a count of how many times the bytes
// were asked for, which is what a listing must not do.
type countingOpener struct {
	dav.Opener
	opens atomic.Int64
	fail  bool
}

func (c *countingOpener) OpenFile(ctx context.Context, f db.File) (io.ReadSeekCloser, error) {
	c.opens.Add(1)
	if c.fail {
		return nil, errors.New("the blob store is on fire")
	}
	return c.Opener.OpenFile(ctx, f)
}

type photoRig struct {
	h     http.Handler
	files *files.Service
	meta  *sqlite.Store
	open  *countingOpener
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
	open := &countingOpener{Opener: service}
	return &photoRig{h: withUser(dav.Photos(photosPrefix, meta, open), "edu"), files: service, meta: meta, open: open}
}

func (r *photoRig) add(t *testing.T, p, body, mime string, taken time.Time) db.File {
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
	if err := r.meta.PutMedia(t.Context(), db.Media{FileID: f.ID, Kind: db.KindImage, IndexedAt: time.Now(), Version: 1, TakenAt: taken}); err != nil {
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

// TestAListingReadsNoBytes is the trap at the top of photos.go: x/net opens
// every resource a PROPFIND lists, and a month of photographs must not become
// a read of every one of them from the blob store.
func TestAListingReadsNoBytes(t *testing.T) {
	t.Parallel()
	r := photoServer(t)
	for i := range 20 {
		r.add(t, fmt.Sprintf("p%02d.jpg", i), "x", "image/jpeg", june)
	}
	hrefs(t, do(t, r.h, "PROPFIND", "/photos/2024/06/", "", "Depth", "1"))
	do(t, r.h, "PROPFIND", "/photos/2024/06/p01.jpg", "", "Depth", "0")
	if n := r.open.opens.Load(); n != 0 {
		t.Errorf("listing a month read %d photos' bytes, want none", n)
	}
}

func TestAPhotoIsTheOriginal(t *testing.T) {
	t.Parallel()
	r := photoServer(t)
	f := r.add(t, "Holiday/beach.heic", "the original bytes", "image/heic", june)

	rec := do(t, r.h, http.MethodGet, "/photos/2024/06/beach.heic", "")
	if rec.Code != http.StatusOK || rec.Body.String() != "the original bytes" {
		t.Fatalf("GET = %d %q", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "image/heic" {
		t.Errorf("Content-Type = %q, want the row's", got)
	}
	// The same validator /dav/ sends for the same file.
	if got := rec.Header().Get("ETag"); got != strconv.Quote(f.ETag) {
		t.Errorf("ETag = %q, want %q", got, strconv.Quote(f.ETag))
	}
	if n := r.open.opens.Load(); n != 1 {
		t.Errorf("one GET opened the bytes %d times", n)
	}

	part := do(t, r.h, http.MethodGet, "/photos/2024/06/beach.heic", "", "Range", "bytes=4-11")
	if part.Code != http.StatusPartialContent || part.Body.String() != "original" {
		t.Errorf("a range = %d %q", part.Code, part.Body.String())
	}
	if head := do(t, r.h, http.MethodHead, "/photos/2024/06/beach.heic", ""); head.Code != http.StatusOK {
		t.Errorf("HEAD = %d", head.Code)
	}
}

// TestNamesInAMonthCannotCollide: two cameras both count from IMG_0001, and a
// case-folding client would see these as one file.
func TestNamesInAMonthCannotCollide(t *testing.T) {
	t.Parallel()
	r := photoServer(t)
	r.add(t, "A/IMG_0001.JPG", "a", "image/jpeg", june)
	r.add(t, "B/IMG_0001.JPG", "b", "image/jpeg", june)
	r.add(t, "C/img_0001.jpg", "c", "image/jpeg", june)

	wantHrefs(t, hrefs(t, do(t, r.h, "PROPFIND", "/photos/2024/06/", "", "Depth", "1")),
		"/photos/2024/06/", "/photos/2024/06/IMG_0001.JPG", "/photos/2024/06/IMG_0001 (2).JPG",
		"/photos/2024/06/img_0001 (3).jpg")
	// The oldest keeps the plain name.
	if got := do(t, r.h, http.MethodGet, "/photos/2024/06/IMG_0001.JPG", "").Body.String(); got != "a" {
		t.Errorf("the plain name serves %q, want the first one", got)
	}
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
	if rec := do(t, dav.Photos(photosPrefix, r.meta, r.open), http.MethodGet, "/photos/", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("with nobody authenticated = %d", rec.Code)
	}
	theirs := withUser(dav.Photos(photosPrefix, r.meta, r.open), "someone-else")
	wantHrefs(t, hrefs(t, do(t, theirs, "PROPFIND", "/photos/", "", "Depth", "1")), "/photos/")
}

// fakePhotos serves a month of many photos from memory, to cross the batch a
// month is read in without writing thousands of rows.
type fakePhotos struct {
	photos []db.Photo
	fail   string
}

func (f fakePhotos) PhotoMonths(context.Context, string) ([]db.PhotoMonth, error) {
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
	h := withUser(dav.Photos(photosPrefix, fakePhotos{photos: many}, nil), "edu")
	if got := hrefs(t, do(t, h, "PROPFIND", "/photos/2024/06/", "", "Depth", "1")); len(got) != 2501 {
		t.Errorf("a month of 2500 lists %d entries", len(got))
	}
}

func TestABrokenBackendIsNotANotFound(t *testing.T) {
	t.Parallel()
	for _, call := range []string{"PhotoMonths", "PhotoTimeline"} {
		h := withUser(dav.Photos(photosPrefix, fakePhotos{fail: call}, nil), "edu")
		if rec := do(t, h, "PROPFIND", "/photos/2024/06/", "", "Depth", "1"); rec.Code != http.StatusInternalServerError {
			t.Errorf("PROPFIND with %s broken = %d, want 500", call, rec.Code)
		}
	}

	r := photoServer(t)
	r.add(t, "a.jpg", "a", "image/jpeg", june)
	r.open.fail = true
	if rec := do(t, r.h, http.MethodGet, "/photos/2024/06/a.jpg", ""); rec.Code != http.StatusInternalServerError {
		t.Errorf("GET with the blob store broken = %d, want 500", rec.Code)
	}
}
