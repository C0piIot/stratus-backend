package web

import (
	"mime"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"

	"github.com/stratus-private-cloud/stratus-backend/internal/db"
	"github.com/stratus-private-cloud/stratus-backend/internal/media"
	"github.com/stratus-private-cloud/stratus-backend/internal/media/thumb"
)

// filesPrefix is where the tree lives. Every directory has one URL and every
// file has the same one, because to somebody typing it they are the same thing.
const filesPrefix = "/files/"

// listPageSize is how much of a directory one page holds unless the reader says
// otherwise. A folder with a hundred thousand photographs in it is an ordinary
// size for this project, and rendering all of it would be one query, one
// enormous document and, since #135, an offer of a thumbnail per row.
//
// It stopped being the only value in #251 and stayed the default. What it is a
// property of has not changed -- what a browser can lay out, and how many
// pictures it is fair to ask this server for at once -- and that is still not
// an operator's setting. It is the reader's, in their own browser, out of the
// three listRows offers, and the largest of those is where the old paragraph
// still bites: five hundred rows is five hundred thumbnails to offer.
const listPageSize = 100

// listFragment is the part of the page htmx asks for when it extends a
// listing: the rows and the link to the next page, without the document around
// them.
const listFragment = "rows"

// browse answers both halves of "open this": a directory is a page, a file is
// its bytes.
func (h *handler) browse(w http.ResponseWriter, r *http.Request, user string) {
	// Empty for an ordinary request, and what makes the page read-only for a
	// shared one: every link below carries it, and the template drops
	// everything that writes.
	token := r.URL.Query().Get(shareParam)

	// Who the page says is signed in, which for a shared one is nobody. The
	// owner authorises the read and is not on show for it: a visitor with a
	// link has no account here, and a navbar naming somebody would be both a
	// door that does not open and a name they did not mean to publish.
	display := user
	if token != "" {
		display = ""
	}

	p, err := toPath(r.PathValue("path"))
	if err != nil {
		h.fail(w, r, display, err)
		return
	}

	// The root is the one directory with no row of its own -- an empty server
	// has nothing in it and still has a root.
	if p != "" {
		f, statErr := h.files.Stat(r.Context(), user, p)
		if statErr != nil {
			h.fail(w, r, display, statErr)
			return
		}
		if !f.IsDir {
			h.file(w, r, display, f)
			return
		}
	}

	state, err := listingOf(r)
	if err != nil {
		h.badRequest(w, display, "That is not a way to arrange this folder.")
		return
	}
	// Remembered only once it is known to be a folder, and never for a visitor
	// with a link: a share is somebody else's browser, and what they do to
	// somebody else's folder is not a preference of theirs to keep.
	if token == "" {
		rememberListing(w, r, state)
	}

	after, err := parseCursor(state.order().By, r.URL.Query().Get("after"))
	if err != nil {
		h.badRequest(w, display, "That is not a place in this folder to carry on from.")
		return
	}
	children, more, err := h.files.ListPage(r.Context(), user, p, state.order(), after, state.Rows)
	if err != nil {
		h.fail(w, r, display, err)
		return
	}

	root := sharedRoot(r)
	v := view{
		Title:   titleOf(p, root),
		User:    display,
		Shared:  token,
		Notice:  uploaded(r.URL.Query().Get("added")),
		Crumbs:  crumbs(p, token, root),
		Entries: entries(children, h.statesOf(r.Context(), r.URL.Path, children), token),
		Columns: columns(href(p), state, token),
		Rows:    rowChoices(href(p), state, token),
		// Where this page's two forms post: into the directory being listed.
		Here:    href(p),
		Folders: link(folderPrefix, p),
	}
	if more {
		v.NextPage = shared(href(p)+"?"+state.query("after",
			encodeCursor(state.order().By, db.After(children[len(children)-1]))), token)
	}

	// htmx asked for the rows to append; anything else asked for the page. The
	// answer is not cached either way -- render sets no-store on both -- so
	// there is no Vary header to get wrong.
	if r.Header.Get("HX-Request") == "true" {
		h.renderTemplate(w, http.StatusOK, pageFiles, listFragment, v)
		return
	}
	h.render(w, http.StatusOK, pageFiles, v)
}

// download streams a file.
func (h *handler) download(w http.ResponseWriter, r *http.Request, user string, f db.File) {
	body, err := h.files.OpenFile(r.Context(), f)
	if err != nil {
		h.fail(w, r, user, err)
		return
	}
	defer func() { _ = body.Close() }()

	// Inline: what to do with a file is the browser's decision. The filename
	// is still named, so saving it keeps the name it was stored under.
	w.Header().Set("Content-Disposition",
		mime.FormatMediaType("inline", map[string]string{"filename": path.Base(f.Path)}))
	w.Header().Set("Content-Security-Policy", filePolicy(f.MIMEType))
	if f.MIMEType != "" {
		w.Header().Set("Content-Type", f.MIMEType)
	}
	if f.ETag != "" {
		// Quoted here because that is HTTP's framing; internal/files stores the
		// validator itself, without quotes, for every surface to frame its own
		// way.
		w.Header().Set("ETag", strconv.Quote(f.ETag))
	}
	// Private and revalidated: it is one person's file, and the ETag makes
	// asking again cheap.
	w.Header().Set("Cache-Control", "private, no-cache")

	// ServeContent implements RFC 7233 over the seeker internal/files returns,
	// which is where ranges, conditional requests and resuming a download come
	// from -- none of it written here.
	http.ServeContent(w, r, path.Base(f.Path), f.MTime, body)
}

// entry is one row of the listing, with everything already in the shape the
// template prints: no formatting logic in the markup.
type entry struct {
	Name     string
	Href     string
	IsDir    bool
	Size     string
	Modified string
	// Thumb is where a picture of this file lives, and empty when this build
	// cannot make one. The ETag is in the URL so the answer can be cached for a
	// year: a new write takes a new blob key and therefore a new ETag.
	Thumb string
	// Picture says the box beside this row is a picture's -- a photograph or a
	// film, which are the two kinds that have one -- and Film says which mark
	// is drawn in it. Both are the name's answer and not the index's: a
	// listing is a hundred rows and the mark cannot wait for them to be read.
	//
	// They are not "has no thumbnail": the mark is drawn under the picture,
	// so a row whose thumbnail is refused or still arriving shows it too.
	Picture bool
	Film    bool
	// Indexing is what the extractor has made of this file, and empty when it
	// has been read -- which is the ordinary case, and a mark on every row
	// would be noise. It says something during a first pass over a library, a
	// re-index, or an import, which is when somebody is looking.
	Indexing string
	// Info is what is known about this file, and empty on a directory, which
	// has nothing but the row it is. The listing opens it under the row and a
	// browser with no script follows it to a page.
	Info string
	// Where the two things that can be done to it are asked for. Both are
	// pages: a rename needs a name, and a delete takes a folder whole.
	Rename string
	Delete string
	Share  string
}

// entries renders what the port returned, in the order it returned it.
// Directories first and then by path is the ordering ListFilesPage promises,
// and it is the query's job rather than this function's: regrouping a page
// afterwards would only group that page, so a listing scrolled through would
// show folders, then files, then folders again.
func entries(children []db.File, states map[int64]rowState, token string) []entry {
	out := make([]entry, 0, len(children))
	for _, c := range children {
		target := href(c.Path)
		if !c.IsDir && media.IsVideo(c.Path) {
			// A film opens in the player, which is a page that still offers
			// the file itself: see video.go.
			target += "?" + playParam
		}
		e := entry{
			Name:     path.Base(c.Path),
			Href:     shared(target, token),
			IsDir:    c.IsDir,
			Modified: c.MTime.Format("2006-01-02 15:04"),
			Rename:   link(renamePrefix, c.Path),
			Delete:   link(deletePrefix, c.Path),
			Share:    link(sharePrefix, c.Path),
		}
		if !c.IsDir {
			e.Size = humanBytes(c.Size)
			e.Indexing = states[c.ID].Indexing
			e.Info = link(infoPrefix, c.Path)
			e.Film = media.IsVideo(c.Path)
			e.Picture = e.Film || media.IsImage(c.Path)
			// A row that has been tried and refused keeps its mark and is
			// offered no picture: the request would be a 404, and the mark is
			// what the square draws either way (#341).
			if thumb.CanThumbnail(c.Path, c.Size) && !states[c.ID].NoPicture {
				// The thumbnail carries the signature too. Without it a shared
				// listing is a grid of broken images, which is the half of this
				// that fails quietly.
				e.Thumb = shared(link(thumbPrefix, c.Path)+"?size="+strconv.Itoa(listThumb)+
					"&v="+url.QueryEscape(c.ETag), token)
			}
		}
		out = append(out, e)
	}
	return out
}

// filePolicy is the content security policy a file is opened under, and the
// reason every file can be opened at all.
//
// This origin holds the session cookie, so an uploaded HTML page or SVG opened
// here would run as the owner -- and the pages' own policy would not stop it,
// because script-src 'self' is satisfied by a .js uploaded beside the page.
// sandbox is what does: the document gets an origin of its own that matches
// nothing, and no scripts, forms or popups.
//
// Every type gets it except the ones that cannot carry a script, and the
// exception exists for PDF: Chrome will not render one in a sandboxed
// document. A type nobody recorded is sandboxed, since ServeContent then works
// one out from the bytes and it can come out as text/html.
func filePolicy(mimeType string) string {
	mediaType, _, _ := mime.ParseMediaType(mimeType)
	top, _, _ := strings.Cut(mediaType, "/")
	switch {
	case mediaType == "image/svg+xml":
		// An image that can carry a script: sandboxed, below.
	case top == "image", top == "video", top == "audio",
		mediaType == "application/pdf", mediaType == "text/plain":
		return fileContentSecurityPolicy
	}
	return fileContentSecurityPolicy + "; sandbox"
}
