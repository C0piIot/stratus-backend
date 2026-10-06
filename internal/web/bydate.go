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
	"github.com/C0piIot/stratus-backend/internal/timeline"
)

// What a camera made, by date (#211, #279, #215): the photographs at /photos/
// and the videos at /videos/, read from the index rather than the tree, so
// where a file was put does not matter -- which is the whole difference
// between these and /files/.
//
// **One address, both protocols.** These are the same URLs the WebDAV mount
// answers for, and the composition root sends a browser's methods here and a
// client's there. So `/photos/` is the grid, `/photos/2024/` is a year and
// `/photos/2024/06/` is a month, and a file's own address serves its bytes
// exactly as it does under /files/ -- the page about it is the same URL with a
// query, because one URL per thing is what the rest of this adapter promises.
//
// **Two libraries, one set of handlers.** A recording and a photograph are the
// same tree over the same column; what differs is the kind asked for, the
// words on the page, and what the page about one file is: a viewer for a
// photograph, the player #50 built for a video. That is the `library` below,
// and it is the whole of the difference.
//
// The names in those addresses are internal/timeline's, shared with the mount:
// two files in a month can share a filename, and a link that guessed which one
// is which would 404 against the collection behind it.
const (
	photosPrefix = "/photos/"
	videosPrefix = "/videos/"
	// viewParam turns a photograph's own address into the page about it. A
	// video's is playParam, which is the spelling a film already had at its
	// own URL under /files/.
	viewParam = "view"
	// fileParam is how a caller holding a path in the tree reaches the page
	// about a file without working out the date address itself: see byFile.
	fileParam = "file"
)

// library is one of the two trees by date. Everything that differs between the
// photographs and the videos is in here.
type library struct {
	prefix string
	kind   db.Kind
	// title heads the grid, and empty is what it says when there is nothing in
	// it yet.
	title, empty string
	// leaf is the query that opens the page about one file, and viewer says
	// whether that page is this package's viewer or the film player.
	leaf   string
	viewer bool
}

var photoLibrary = library{
	prefix: photosPrefix, kind: db.KindImage, title: "Photos",
	empty: "No photos yet. Every image in the library shows up here once it has been read, however it arrived and wherever it was put",
	leaf:  viewParam, viewer: true,
}

var videoLibrary = library{
	prefix: videosPrefix, kind: db.KindVideo, title: "Videos",
	empty: "No videos yet. Every recording and every film shows up here once it has been read, however it arrived and wherever it was put",
	leaf:  playParam,
}

// gridPageSize is how many tiles one page of a grid holds, for the reason
// listPageSize is a hundred: a phone's camera roll is tens of thousands.
const gridPageSize = 100

// Sizes on the thumbnail ladder: a grid cell, and a photograph filling a
// screen. The large one is also what makes a HEIC viewable in a browser that
// cannot decode one, which is every browser but Safari.
const (
	tileThumb   = 300
	viewerThumb = 1200
)

// tilesFragment is the part of a grid htmx asks for when it extends it.
const tilesFragment = "tiles"

// tile is one cell of the grid. Heading is set on the first photo of a month,
// and is what the template draws a month's title from; HeadingHref is that
// month's own page, which is how the grid is walked into.
type tile struct {
	Heading     string
	HeadingHref string
	Href        string
	Thumb       string
	Name        string
	// Duration is a video's, rendered, and empty for anything without one.
	Duration string
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

// href is the URL of a place in this library's tree.
func (l library) href(p string) string { return link(l.prefix, p) }

// leafHref is the page about one file: the viewer, or the player.
func (l library) leafHref(p string) string { return l.href(p) + "?" + l.leaf }

// photoByFileHref is the viewer, for a caller that has a path in the tree and
// not a date. Used by the search results, whose rows are files.
func photoByFileHref(p string) string {
	return photosPrefix + "?" + fileParam + "=" + url.QueryEscape(p)
}

// byDate is every address under one library's prefix, split by what is at it.
//
// Resolution is internal/timeline's, which is what makes a 404 here and a 404
// from the mount the same 404.
func (h *handler) byDate(l library) func(http.ResponseWriter, *http.Request, string) {
	return func(w http.ResponseWriter, r *http.Request, user string) { h.serveByDate(w, r, user, l) }
}

func (h *handler) serveByDate(w http.ResponseWriter, r *http.Request, user string, l library) {
	raw := strings.Trim(r.PathValue("path"), "/")
	tree := timeline.New(h.captures, user, l.kind)

	if raw == "" {
		if f := r.URL.Query().Get(fileParam); f != "" {
			h.byFile(w, r, user, l, tree, f)
			return
		}
		h.grid(w, r, user, l, tree)
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
		h.year(w, r, user, l, tree, n.Year)
	case n.Dir:
		h.month(w, r, user, l, tree, db.Month{Year: n.Year, Month: n.Month})
	case r.URL.Query().Has(l.leaf) && l.viewer:
		h.viewer(w, r, user, l, tree, n)
	case r.URL.Query().Has(l.leaf) && h.video.Media != nil:
		// A video's page is the player #50 built, at the video's own address
		// rather than the file's: one URL per thing, here too. With no video
		// wiring there is no player, and the file itself is what is offered --
		// the same fallback h.file makes.
		h.play(w, r, user, n.Capture.File, l.href(n.Path()), l.href(monthPath(db.MonthOf(n.Capture.SortAt)))+"/")
	default:
		// A file's own address is the file, like it is under /files/. This is
		// also the GET a WebDAV client makes.
		h.download(w, r, user, n.Capture.File)
	}
}

// photoGrid is the whole library, newest first, grouped by month.
func (h *handler) grid(w http.ResponseWriter, r *http.Request, user string, l library, tree *timeline.Tree) {
	after, err := parseCaptureCursor(r.URL.Query().Get("after"))
	if err != nil {
		h.badRequest(w, user, "That is not a place in the gallery to carry on from.")
		return
	}

	// One more than a page, so the last page knows it is the last rather than
	// offering a link to an empty one.
	page, err := h.captures.Timeline(r.Context(), user,
		db.CaptureFilter{Kind: l.kind, After: after, Limit: gridPageSize + 1})
	if err != nil {
		h.fail(w, r, user, err)
		return
	}
	more := len(page) > gridPageSize
	if more {
		page = page[:gridPageSize]
	}

	var next string
	if more {
		next = l.prefix + "?after=" + url.QueryEscape(encodeCaptureCursor(page[len(page)-1].Cursor()))
	}

	// A fragment is appended below the month the page before it ended in, so
	// it is told that month and does not head it again. A whole page starts
	// with nothing above it -- the viewer's way back lands mid-month -- so its
	// first photo is always headed.
	var headed db.Month
	if r.Header.Get("HX-Request") == "true" && !after.AtStart() {
		headed = db.MonthOf(after.At)
	}
	cells, err := h.tiles(r.Context(), l, tree, page, headed)
	if err != nil {
		h.fail(w, r, user, err)
		return
	}
	if r.Header.Get("HX-Request") == "true" {
		h.renderTemplate(w, http.StatusOK, pagePhotos, tilesFragment,
			view{Tiles: cells, NextPage: next})
		return
	}
	h.render(w, http.StatusOK, pagePhotos,
		view{Title: l.title, User: user, Gallery: l.title, Tiles: cells, NextPage: next, Empty: l.empty})
}

// photoYear is the months of one year.
func (h *handler) year(w http.ResponseWriter, r *http.Request, user string, l library, tree *timeline.Tree, year int) {
	months, err := tree.Months(r.Context())
	if err != nil {
		h.fail(w, r, user, err)
		return
	}
	var links []crumb
	for _, m := range months {
		if m.Year == year {
			links = append(links, crumb{Name: monthName(m), Href: l.href(monthPath(m)) + "/"})
		}
	}
	h.render(w, http.StatusOK, pageMonths, view{
		Title: strconv.Itoa(year), User: user, Gallery: l.title,
		Crumbs: l.crumbs(year, 0),
		Months: links,
	})
}

// photoMonth is one month's photographs, paged like the grid because a month
// of a camera roll is thousands.
func (h *handler) month(w http.ResponseWriter, r *http.Request, user string, l library, tree *timeline.Tree, m db.Month) {
	after, err := parseCaptureCursor(r.URL.Query().Get("after"))
	if err != nil {
		h.badRequest(w, user, "That is not a place in the gallery to carry on from.")
		return
	}

	page, err := h.captures.Timeline(r.Context(), user,
		db.CaptureFilter{Kind: l.kind, From: m.Start(), To: m.End(), After: after, Limit: gridPageSize + 1})
	if err != nil {
		h.fail(w, r, user, err)
		return
	}
	more := len(page) > gridPageSize
	if more {
		page = page[:gridPageSize]
	}

	var next string
	if more {
		next = l.href(monthPath(m)) + "/?after=" + url.QueryEscape(encodeCaptureCursor(page[len(page)-1].Cursor()))
	}
	// The page is already titled with this month, so no cell heads it again.
	cells, err := h.tiles(r.Context(), l, tree, page, m)
	if err != nil {
		h.fail(w, r, user, err)
		return
	}
	h.render(w, http.StatusOK, pagePhotos, view{
		Title: monthName(m), User: user, Gallery: l.title,
		Crumbs:   l.crumbs(m.Year, m.Month),
		Tiles:    cells,
		NextPage: next,
	})
}

// photoViewer is one photograph and the ones either side of it.
func (h *handler) viewer(w http.ResponseWriter, r *http.Request, user string, l library, tree *timeline.Tree, n timeline.Node) {
	around, err := h.captures.Around(r.Context(), user, l.kind, n.Capture.File.ID)
	if err != nil {
		h.fail(w, r, user, err)
		return
	}

	f := around.Capture.File
	pv := photoView{
		Name:     n.Name,
		Original: href(f.Path),
		Arrived:  around.Capture.Media.TakenAt.IsZero(),
		Camera:   around.Capture.Media.Camera,
		// Back to the page this photograph is on: the grid carried on from the
		// one before it, so it is the first thing there.
		Back: l.prefix,
		When: around.Capture.SortAt.Format("2 January 2006, 15:04"),
	}
	if m := around.Capture.Media; m.Width > 0 && m.Height > 0 {
		pv.Dimensions = fmt.Sprintf("%d × %d", m.Width, m.Height)
	}
	if media.CanThumbnail(f.Path, f.Size) {
		pv.Image = thumbURL(f, viewerThumb)
	}
	if p := around.Newer; p != nil {
		if pv.Newer, err = h.leafHref(r.Context(), l, tree, *p); err != nil {
			h.fail(w, r, user, err)
			return
		}
		pv.Back = l.prefix + "?after=" + url.QueryEscape(encodeCaptureCursor(p.Cursor()))
	}
	if p := around.Older; p != nil {
		if pv.Older, err = h.leafHref(r.Context(), l, tree, *p); err != nil {
			h.fail(w, r, user, err)
			return
		}
	}

	h.render(w, http.StatusOK, pagePhoto, view{Title: pv.Name, User: user, Gallery: l.title, Photo: &pv})
}

// photoByFile sends a caller holding a path in the tree to the one address
// that photograph has here.
//
// A redirect rather than a second page at a second URL: the search results
// list files and have no date to build an address from, and resolving one per
// tile would be a query per cell of a grid. One resolves when one is clicked.
func (h *handler) byFile(w http.ResponseWriter, r *http.Request, user string, l library, tree *timeline.Tree, p string) {
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
	around, err := h.captures.Around(r.Context(), user, l.kind, f.ID)
	if err != nil {
		h.fail(w, r, user, err)
		return
	}
	target, err := h.leafHref(r.Context(), l, tree, around.Capture)
	if err != nil {
		h.fail(w, r, user, err)
		return
	}
	redirectLocal(w, r, target)
}

// viewerHref is the page about a photograph, at the address the mount knows it
// by.
func (h *handler) leafHref(ctx context.Context, l library, tree *timeline.Tree, p db.Capture) (string, error) {
	at, err := tree.PathOf(ctx, p)
	if err != nil {
		return "", err
	}
	return l.leafHref(at), nil
}

// tiles lays a page out, with a month's heading on each month's first photo.
//
// headed is the month that already has a heading above this page -- the one
// the page before it ended in, or the month a month's own page is titled with
// -- so a month split across two pages is headed once and a month's page does
// not head itself twice. The zero month heads everything it meets.
func (h *handler) tiles(ctx context.Context, l library, tree *timeline.Tree, page []db.Capture, headed db.Month) ([]tile, error) {
	current := headed
	out := make([]tile, 0, len(page))
	for _, p := range page {
		at, err := tree.PathOf(ctx, p)
		if err != nil {
			return nil, err
		}
		t := tile{
			Href:     l.leafHref(at),
			Name:     path.Base(p.File.Path),
			Duration: clock(p.Media.DurationMS),
		}
		if month := db.MonthOf(p.SortAt); month != current {
			t.Heading = monthName(month)
			t.HeadingHref = l.href(monthPath(month)) + "/"
			current = month
		}
		if media.CanThumbnail(p.File.Path, p.File.Size) {
			t.Thumb = thumbURL(p.File, tileThumb)
		}
		out = append(out, t)
	}
	return out, nil
}

// crumbs is the trail above a year or a month.
func (l library) crumbs(year int, month time.Month) []crumb {
	trail := []crumb{{Name: l.title, Href: l.prefix}}
	trail = append(trail, crumb{Name: strconv.Itoa(year), Href: l.href(strconv.Itoa(year)) + "/"})
	if month != 0 {
		m := db.Month{Year: year, Month: month}
		trail = append(trail, crumb{Name: monthName(m), Href: l.href(monthPath(m)) + "/"})
	}
	trail[len(trail)-1].Last = true
	return trail
}

// monthPath is a month's address under the mount, which is the one place that
// spelling is decided.
func monthPath(m db.Month) string {
	return timeline.Node{Year: m.Year, Month: m.Month}.Path()
}

// thumbURL is a picture of f at px on the ladder. The ETag is in it so the URL
// changes exactly when the picture does, which is what lets /thumb/ be cached
// for a year.
func thumbURL(f db.File, px int) string {
	return link(thumbPrefix, f.Path) + "?size=" + strconv.Itoa(px) + "&v=" + url.QueryEscape(f.ETag)
}

func monthName(m db.Month) string {
	return time.Date(m.Year, m.Month, 1, 0, 0, 0, 0, time.UTC).Format("January 2006")
}

// encodeCaptureCursor and parseCaptureCursor carry a place in a grid through
// a URL: the time in milliseconds and the file id, which is the whole of the
// ordering. In the clear, like the listing's cursor: it says nothing a page
// did not already show.
func encodeCaptureCursor(c db.CaptureCursor) string {
	return strconv.FormatInt(c.At.UnixMilli(), 10) + "." + strconv.FormatInt(c.FileID, 10)
}

func parseCaptureCursor(v string) (db.CaptureCursor, error) {
	if v == "" {
		return db.CaptureCursor{}, nil
	}
	at, id, ok := strings.Cut(v, ".")
	ms, err := strconv.ParseInt(at, 10, 64)
	if !ok || err != nil {
		return db.CaptureCursor{}, errors.New("photo cursor: no time")
	}
	fileID, err := strconv.ParseInt(id, 10, 64)
	if err != nil || fileID <= 0 {
		return db.CaptureCursor{}, errors.New("photo cursor: no file")
	}
	return db.CaptureCursor{At: time.UnixMilli(ms).UTC(), FileID: fileID}, nil
}
