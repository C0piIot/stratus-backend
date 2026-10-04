package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/C0piIot/stratus-backend/internal/db"
	"github.com/C0piIot/stratus-backend/internal/media"
	"github.com/C0piIot/stratus-backend/internal/photos"
)

// The photographs, by date (#211, #279): every image in the library, read from
// the index rather than the tree, so where one was filed does not matter --
// which is the whole difference between this and /files/.
//
// **One address, both protocols.** These are the same URLs the WebDAV mount
// answers for, and the composition root sends a browser's methods here and a
// client's there. So `/photos/` is the grid, `/photos/2024/` is a year and
// `/photos/2024/06/` is a month, and a photograph's own address serves its
// bytes exactly as a file's does under /files/ -- the page about it is the
// same URL with ?view, because one URL per thing is what the rest of this
// adapter promises.
//
// The names in those addresses are internal/photos', shared with the mount:
// two photographs in a month can share a filename, and a link that guessed
// which one is which would 404 against the collection behind it.
const (
	photosPrefix = "/photos/"
	// viewParam turns a photograph's own address into the page about it.
	viewParam = "view"
	// fileParam is how a caller holding a path in the tree reaches the viewer
	// without working out the date address itself: see photoByFile.
	fileParam = "file"
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
// and is what the template draws a month's title from; HeadingHref is that
// month's own page, which is how the grid is walked into.
type tile struct {
	Heading     string
	HeadingHref string
	Href        string
	Thumb       string
	Name        string
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

// photoHref is the URL of a place in the date tree.
func photoHref(p string) string { return link(photosPrefix, p) }

// photoByFileHref is the viewer, for a caller that has a path in the tree and
// not a date. Used by the search results, whose rows are files.
func photoByFileHref(p string) string {
	return photosPrefix + "?" + fileParam + "=" + url.QueryEscape(p)
}

// photos is every address under /photos/, split by what is at it.
//
// Resolution is internal/photos', which is what makes a 404 here and a 404
// from the mount the same 404.
func (h *handler) photos(w http.ResponseWriter, r *http.Request, user string) {
	raw := strings.Trim(r.PathValue("path"), "/")
	tree := photos.New(h.photoIndex, user)

	if raw == "" {
		if f := r.URL.Query().Get(fileParam); f != "" {
			h.photoByFile(w, r, user, tree, f)
			return
		}
		h.photoGrid(w, r, user, tree)
		return
	}

	n, err := tree.Resolve(r.Context(), raw)
	switch {
	case errors.Is(err, os.ErrNotExist):
		// The same page a path that is not in the tree gets, and the user is
		// the one signedIn already found rather than a second read of the
		// cookie.
		h.fail(w, r, user, db.ErrNotFound)
		return
	case err != nil:
		h.fail(w, r, user, err)
		return
	}

	switch {
	case n.Dir && n.Month == 0:
		h.photoYear(w, r, user, tree, n.Year)
	case n.Dir:
		h.photoMonth(w, r, user, tree, db.PhotoMonth{Year: n.Year, Month: n.Month})
	case r.URL.Query().Has(viewParam):
		h.photoViewer(w, r, user, tree, n)
	default:
		// A photograph's own address is the photograph, like a file's is under
		// /files/. This is also the GET a WebDAV client makes.
		h.download(w, r, user, n.Photo.File)
	}
}

// photoGrid is the whole library, newest first, grouped by month.
func (h *handler) photoGrid(w http.ResponseWriter, r *http.Request, user string, tree *photos.Tree) {
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
		next = photosPrefix + "?after=" + url.QueryEscape(encodePhotoCursor(page[len(page)-1].Cursor()))
	}

	// A fragment is appended below the month the page before it ended in, so
	// it is told that month and does not head it again. A whole page starts
	// with nothing above it -- the viewer's way back lands mid-month -- so its
	// first photo is always headed.
	var headed db.PhotoMonth
	if r.Header.Get("HX-Request") == "true" && !after.AtStart() {
		headed = db.MonthOf(after.At)
	}
	cells, err := h.tiles(r.Context(), tree, page, headed)
	if err != nil {
		h.fail(w, r, user, err)
		return
	}
	if r.Header.Get("HX-Request") == "true" {
		h.renderTemplate(w, http.StatusOK, pagePhotos, photosFragment,
			view{Tiles: cells, NextPage: next})
		return
	}
	h.render(w, http.StatusOK, pagePhotos,
		view{Title: "Photos", User: user, Tiles: cells, NextPage: next})
}

// photoYear is the months of one year.
func (h *handler) photoYear(w http.ResponseWriter, r *http.Request, user string, tree *photos.Tree, year int) {
	months, err := tree.Months(r.Context())
	if err != nil {
		h.fail(w, r, user, err)
		return
	}
	var links []crumb
	for _, m := range months {
		if m.Year == year {
			links = append(links, crumb{Name: monthName(m), Href: photoHref(monthPath(m)) + "/"})
		}
	}
	h.render(w, http.StatusOK, pageMonths, view{
		Title: strconv.Itoa(year), User: user,
		Crumbs: photoCrumbs(year, 0),
		Months: links,
	})
}

// photoMonth is one month's photographs, paged like the grid because a month
// of a camera roll is thousands.
func (h *handler) photoMonth(w http.ResponseWriter, r *http.Request, user string, tree *photos.Tree, m db.PhotoMonth) {
	after, err := parsePhotoCursor(r.URL.Query().Get("after"))
	if err != nil {
		h.badRequest(w, user, "That is not a place in the gallery to carry on from.")
		return
	}

	page, err := h.photoIndex.PhotoTimeline(r.Context(), user,
		db.PhotoFilter{From: m.Start(), To: m.End(), After: after, Limit: photoPageSize + 1})
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
		next = photoHref(monthPath(m)) + "/?after=" + url.QueryEscape(encodePhotoCursor(page[len(page)-1].Cursor()))
	}
	// The page is already titled with this month, so no cell heads it again.
	cells, err := h.tiles(r.Context(), tree, page, m)
	if err != nil {
		h.fail(w, r, user, err)
		return
	}
	h.render(w, http.StatusOK, pagePhotos, view{
		Title: monthName(m), User: user,
		Crumbs:   photoCrumbs(m.Year, m.Month),
		Tiles:    cells,
		NextPage: next,
	})
}

// photoViewer is one photograph and the ones either side of it.
func (h *handler) photoViewer(w http.ResponseWriter, r *http.Request, user string, tree *photos.Tree, n photos.Node) {
	around, err := h.photoIndex.PhotoAround(r.Context(), user, n.Photo.File.ID)
	if err != nil {
		h.fail(w, r, user, err)
		return
	}

	f := around.Photo.File
	pv := photoView{
		Name:     n.Name,
		Original: href(f.Path),
		Arrived:  around.Photo.Media.TakenAt.IsZero(),
		Camera:   around.Photo.Media.Camera,
		// Back to the page this photograph is on: the grid carried on from the
		// one before it, so it is the first thing there.
		Back: photosPrefix,
		When: around.Photo.SortAt.Format("2 January 2006, 15:04"),
	}
	if m := around.Photo.Media; m.Width > 0 && m.Height > 0 {
		pv.Dimensions = fmt.Sprintf("%d × %d", m.Width, m.Height)
	}
	if media.CanThumbnail(f.Path, f.Size) {
		pv.Image = thumbURL(f, viewerThumb)
	}
	if p := around.Newer; p != nil {
		if pv.Newer, err = h.viewerHref(r.Context(), tree, *p); err != nil {
			h.fail(w, r, user, err)
			return
		}
		pv.Back = photosPrefix + "?after=" + url.QueryEscape(encodePhotoCursor(p.Cursor()))
	}
	if p := around.Older; p != nil {
		if pv.Older, err = h.viewerHref(r.Context(), tree, *p); err != nil {
			h.fail(w, r, user, err)
			return
		}
	}

	h.render(w, http.StatusOK, pagePhoto, view{Title: pv.Name, User: user, Photo: &pv})
}

// photoByFile sends a caller holding a path in the tree to the one address
// that photograph has here.
//
// A redirect rather than a second page at a second URL: the search results
// list files and have no date to build an address from, and resolving one per
// tile would be a query per cell of a grid. One resolves when one is clicked.
func (h *handler) photoByFile(w http.ResponseWriter, r *http.Request, user string, tree *photos.Tree, p string) {
	clean, err := toPath(p)
	if err != nil {
		h.fail(w, r, user, err)
		return
	}
	f, err := h.files.Stat(r.Context(), user, clean)
	if err != nil {
		h.fail(w, r, user, err)
		return
	}
	around, err := h.photoIndex.PhotoAround(r.Context(), user, f.ID)
	if err != nil {
		h.fail(w, r, user, err)
		return
	}
	target, err := h.viewerHref(r.Context(), tree, around.Photo)
	if err != nil {
		h.fail(w, r, user, err)
		return
	}
	redirectLocal(w, r, target)
}

// viewerHref is the page about a photograph, at the address the mount knows it
// by.
func (h *handler) viewerHref(ctx context.Context, tree *photos.Tree, p db.Photo) (string, error) {
	at, err := tree.PathOf(ctx, p)
	if err != nil {
		return "", err
	}
	return photoHref(at) + "?" + viewParam, nil
}

// tiles lays a page out, with a month's heading on each month's first photo.
//
// headed is the month that already has a heading above this page -- the one
// the page before it ended in, or the month a month's own page is titled with
// -- so a month split across two pages is headed once and a month's page does
// not head itself twice. The zero month heads everything it meets.
func (h *handler) tiles(ctx context.Context, tree *photos.Tree, page []db.Photo, headed db.PhotoMonth) ([]tile, error) {
	current := headed
	out := make([]tile, 0, len(page))
	for _, p := range page {
		at, err := tree.PathOf(ctx, p)
		if err != nil {
			return nil, err
		}
		t := tile{
			Href: photoHref(at) + "?" + viewParam,
			Name: path.Base(p.File.Path),
		}
		if month := db.MonthOf(p.SortAt); month != current {
			t.Heading = monthName(month)
			t.HeadingHref = photoHref(monthPath(month)) + "/"
			current = month
		}
		if media.CanThumbnail(p.File.Path, p.File.Size) {
			t.Thumb = thumbURL(p.File, tileThumb)
		}
		out = append(out, t)
	}
	return out, nil
}

// photoCrumbs is the trail above a year or a month.
func photoCrumbs(year int, month time.Month) []crumb {
	trail := []crumb{{Name: "Photos", Href: photosPrefix}}
	trail = append(trail, crumb{Name: strconv.Itoa(year), Href: photoHref(strconv.Itoa(year)) + "/"})
	if month != 0 {
		m := db.PhotoMonth{Year: year, Month: month}
		trail = append(trail, crumb{Name: monthName(m), Href: photoHref(monthPath(m)) + "/"})
	}
	trail[len(trail)-1].Last = true
	return trail
}

// monthPath is a month's address under the mount, which is the one place that
// spelling is decided.
func monthPath(m db.PhotoMonth) string {
	return photos.Node{Year: m.Year, Month: m.Month}.Path()
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
