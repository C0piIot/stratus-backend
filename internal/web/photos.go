package web

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/C0piIot/stratus-backend/internal/db"
	"github.com/C0piIot/stratus-backend/internal/media"
)

// The photo gallery (#211): every image in the library, newest first by when
// the camera says it was taken, grouped by month. It reads the index rather
// than the tree, so where a photograph was filed does not matter -- which is
// the whole difference between this and /files/.
//
// Under /gallery/ because the plain /photos/ is the WebDAV mount of the same
// photographs by date, and videos will sit beside these at /gallery/videos.
const (
	galleryPhotos = "/gallery/photos"
	photoPrefix   = galleryPhotos + "/"
)

// photoPageSize is how many tiles one page of the grid holds, for the reason
// listPageSize is a hundred: a phone's camera roll is tens of thousands.
const photoPageSize = 100

// Sizes on the thumbnail ladder: a grid cell, and a photograph filling a
// screen. The large one is also what makes a HEIC viewable in a browser that
// cannot decode one, which is every browser but Safari.
const (
	tileThumb   = 300
	viewerThumb = 1200
)

// photosFragment is the part of the grid htmx asks for when it extends it.
const photosFragment = "tiles"

// tile is one cell of the grid. Heading is set on the first photo of a month,
// and is what the template draws a month's title from.
type tile struct {
	Heading string
	Href    string
	Thumb   string
	Name    string
}

// photoView is the viewer's photograph and what is known about it.
type photoView struct {
	Name     string
	Image    string
	Original string
	When     string
	// Arrived says When is the file's arrival rather than the camera's clock,
	// which is what a screenshot has.
	Arrived    bool
	Camera     string
	Dimensions string
	Newer      string
	Older      string
	Back       string
}

// photos is the grid.
func (h *handler) photos(w http.ResponseWriter, r *http.Request, user string) {
	after, err := parsePhotoCursor(r.URL.Query().Get("after"))
	if err != nil {
		h.badRequest(w, user, "That is not a place in the gallery to carry on from.")
		return
	}

	// One more than a page, so the last page knows it is the last rather than
	// offering a link to an empty one.
	page, err := h.photoIndex.PhotoTimeline(r.Context(), user, db.PhotoFilter{After: after, Limit: photoPageSize + 1})
	if err != nil {
		h.fail(w, r, user, err)
		return
	}
	more := len(page) > photoPageSize
	if more {
		page = page[:photoPageSize]
	}

	var next string
	if more {
		next = galleryPhotos + "?after=" + url.QueryEscape(encodePhotoCursor(page[len(page)-1].Cursor()))
	}
	// A fragment is appended below the month the page before it ended in, so
	// it knows that month and does not head it again. A whole page starts
	// with nothing above it -- the viewer's way back lands mid-month -- so its
	// first photo is always headed.
	if r.Header.Get("HX-Request") == "true" {
		h.renderTemplate(w, http.StatusOK, pagePhotos, photosFragment,
			view{Tiles: tiles(page, after), NextPage: next})
		return
	}
	h.render(w, http.StatusOK, pagePhotos,
		view{Title: "Photos", User: user, Tiles: tiles(page, db.PhotoCursor{}), NextPage: next})
}

// tiles lays a page out, with a month's heading on each month's first photo.
// Given the cursor it carries on from, it knows the month the last page ended
// in, so a month split across two pages is headed once.
func tiles(page []db.Photo, after db.PhotoCursor) []tile {
	var current db.PhotoMonth
	if !after.AtStart() {
		current = db.MonthOf(after.At)
	}
	out := make([]tile, 0, len(page))
	for _, p := range page {
		t := tile{
			Href: link(photoPrefix, p.File.Path),
			Name: path.Base(p.File.Path),
		}
		if month := db.MonthOf(p.SortAt); month != current {
			t.Heading = monthName(month)
			current = month
		}
		if media.CanThumbnail(p.File.Path, p.File.Size) {
			t.Thumb = thumbURL(p.File, tileThumb)
		}
		out = append(out, t)
	}
	return out
}

// photo is the viewer: one photograph, and the ones either side of it.
func (h *handler) photo(w http.ResponseWriter, r *http.Request, user string) {
	p, err := toPath(r.PathValue("path"))
	if err != nil {
		h.fail(w, r, user, err)
		return
	}
	if p == "" {
		redirectLocal(w, r, galleryPhotos)
		return
	}
	f, err := h.files.Stat(r.Context(), user, p)
	if err != nil {
		h.fail(w, r, user, err)
		return
	}
	around, err := h.photoIndex.PhotoAround(r.Context(), user, f.ID)
	if err != nil {
		h.fail(w, r, user, err)
		return
	}

	pv := photoView{
		Name:     path.Base(p),
		Original: href(p),
		Arrived:  around.Photo.Media.TakenAt.IsZero(),
		Camera:   around.Photo.Media.Camera,
		// Back to the page this photograph is on: the grid carried on from the
		// one before it, so it is the first thing there.
		Back: galleryPhotos,
		When: around.Photo.SortAt.Format("2 January 2006, 15:04"),
	}
	if m := around.Photo.Media; m.Width > 0 && m.Height > 0 {
		pv.Dimensions = fmt.Sprintf("%d × %d", m.Width, m.Height)
	}
	if media.CanThumbnail(p, f.Size) {
		pv.Image = thumbURL(around.Photo.File, viewerThumb)
	}
	if n := around.Newer; n != nil {
		pv.Newer = link(photoPrefix, n.File.Path)
		pv.Back = galleryPhotos + "?after=" + url.QueryEscape(encodePhotoCursor(n.Cursor()))
	}
	if o := around.Older; o != nil {
		pv.Older = link(photoPrefix, o.File.Path)
	}

	h.render(w, http.StatusOK, pagePhoto, view{Title: pv.Name, User: user, Photo: &pv})
}

// thumbURL is a picture of f at px on the ladder. The ETag is in it so the URL
// changes exactly when the picture does, which is what lets /thumb/ be cached
// for a year.
func thumbURL(f db.File, px int) string {
	return link(thumbPrefix, f.Path) + "?size=" + strconv.Itoa(px) + "&v=" + url.QueryEscape(f.ETag)
}

func monthName(m db.PhotoMonth) string {
	return time.Date(m.Year, m.Month, 1, 0, 0, 0, 0, time.UTC).Format("January 2006")
}

// encodePhotoCursor and parsePhotoCursor carry a place in the gallery through
// a URL: the time in milliseconds and the file id, which is the whole of the
// ordering. In the clear, like the listing's cursor: it says nothing a page
// did not already show.
func encodePhotoCursor(c db.PhotoCursor) string {
	return strconv.FormatInt(c.At.UnixMilli(), 10) + "." + strconv.FormatInt(c.FileID, 10)
}

func parsePhotoCursor(v string) (db.PhotoCursor, error) {
	if v == "" {
		return db.PhotoCursor{}, nil
	}
	at, id, ok := strings.Cut(v, ".")
	ms, err := strconv.ParseInt(at, 10, 64)
	if !ok || err != nil {
		return db.PhotoCursor{}, errors.New("photo cursor: no time")
	}
	fileID, err := strconv.ParseInt(id, 10, 64)
	if err != nil || fileID <= 0 {
		return db.PhotoCursor{}, errors.New("photo cursor: no file")
	}
	return db.PhotoCursor{At: time.UnixMilli(ms).UTC(), FileID: fileID}, nil
}
