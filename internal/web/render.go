package web

import (
	"bytes"
	"embed"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"

	"github.com/C0piIot/stratus-backend/internal/db"
)

//go:embed templates
var templateFS embed.FS

// staticFS holds Bootstrap 5.3.8 and htmx 2.0.10, byte for byte as published:
// Bootstrap's dist/css/bootstrap.min.css and dist/js/bootstrap.bundle.min.js,
// which are identical to the copies jsDelivr serves, and htmx's dist/htmx.min.js.
//
//	sha256 d85327d99c7a3ee1f9b5d0500d1370acea3ad2db39c163c2f51f232baedbdede  bootstrap.min.css
//	sha256 e4fd49181388c48ec5040bd3fe66f57c29c8e67fcd8502b3354b96ec7ab47cc7  bootstrap.bundle.min.js
//	sha256 71ea67185bfa8c98c39d31717c6fce5d852370fcdfd129db4543774d3145c0de  htmx.min.js
//
// Vendored rather than linked because a self-hosted cloud has to work with no
// outbound network, and unedited because a file that has been touched can no
// longer be checked against the one upstream publishes. The sourceMappingURL
// comment at the end of Bootstrap's two therefore stays: a 404 with the
// developer tools open is cheaper than a copy nobody can verify.
//
//go:embed static
var staticFS embed.FS

const (
	pageLogin  = "login.html"
	pageFiles  = "files.html"
	pageRename = "rename.html"
	pageDelete = "delete.html"
	pageStatus = "status.html"
	pageShare  = "share.html"
	pageShared = "shared.html"
	pageError  = "error.html"
)

// Each page is parsed with the layout into a set of its own. One set for all of
// them cannot work: every page defines the same "content" template, which is
// what lets the layout call it.
var pages = map[string]*template.Template{
	pageLogin:  parse(pageLogin),
	pageFiles:  parse(pageFiles),
	pageRename: parse(pageRename),
	pageDelete: parse(pageDelete),
	pageStatus: parse(pageStatus),
	pageShare:  parse(pageShare),
	pageShared: parse(pageShared),
	pageError:  parse(pageError),
}

func parse(page string) *template.Template {
	return template.Must(template.ParseFS(templateFS, "templates/layout.html", "templates/"+page))
}

// view is what every page is rendered from. One flat struct for four pages
// rather than a type each: the fields that vary are a handful of strings, and a
// hierarchy would only be something to navigate in the templates.
type view struct {
	Title   string
	Assets  string
	Version string
	// BuildDate is the date of the commit this binary was built from, beside
	// the version in the footer.
	BuildDate string
	// HTMX is where the vendored script lives, beside Assets rather than under
	// it: two libraries, two versions, two paths.
	HTMX string
	// Script is this project's own, with the build on the end so a new one is
	// a new URL. See internal/web/static/stratus/copy.js.
	Script string
	// User is who is signed in, and empty when nobody is.
	User string
	// Username is what was typed into the form, so a failed login does not make
	// somebody type it again.
	Username string
	Error    string
	Message  string
	Notice   string
	Next     string
	// Crumbs and Entries are the file listing: the trail back to the root, and
	// one page of what is in this directory. Here and Folders are where its two
	// forms post, and NextPage is where the rest of the listing is -- empty
	// when this page is the end of it.
	Crumbs   []crumb
	Entries  []entry
	Here     string
	Folders  string
	NextPage string
	// Counts and the four fields under it are the status page: how much of the
	// library has been looked at, by which extractor, and how often it checks
	// when there is nothing to do.
	Counts        db.MediaCounts
	Percent       int
	IndexVersion  int
	IndexInterval string
	IndexingOff   bool
	// FreeSpace is what the blob store says is left, already rendered, and
	// empty when it would not say.
	FreeSpace string
	// Shared is the signature this page was reached with, empty for a request
	// that arrived with a session. Every link the page emits carries it, or the
	// second click is a login form.
	Shared string
	// Lives, Link and Expires are the two share pages: what to offer, what came
	// out, and until when.
	Lives   []shareLifetime
	Link    string
	Expires string
	// Name, IsDir, Action and Back are the two pages that act on one thing:
	// what it is called, what it is, where the form posts and where Cancel goes.
	Name   string
	IsDir  bool
	Action string
	Back   string
}

// render writes a whole page or none of it. The buffer is the point: a template
// that failed halfway would otherwise have already sent a 200 and half the
// markup, which a browser renders as a broken page rather than an error.
func (h *handler) render(w http.ResponseWriter, status int, page string, v view) {
	h.renderTemplate(w, status, page, "layout", v)
}

// renderTemplate writes the whole page when name is the layout, and one
// template out of it when it is not -- which is what htmx swaps into a listing
// it is extending. Same handler, same URL, same markup, a piece of it: not the
// private API principle 2 forbids, since nothing here answers in JSON and a
// client that sends no htmx header gets the page.
func (h *handler) renderTemplate(w http.ResponseWriter, status int, page, name string, v view) {
	v.Assets, v.HTMX, v.Version, v.BuildDate = assetPrefix, htmxPrefix, h.version, h.buildDate
	v.Script = ownPrefix + "/copy.js?v=" + url.QueryEscape(h.version)

	var buf bytes.Buffer
	if err := pages[page].ExecuteTemplate(&buf, name, v); err != nil {
		slog.Error("rendering a page", "page", page, "template", name, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// Every page here is somebody's session. A browser that kept one would show
	// it again on the way back after signing out, which on a shared machine is
	// the difference between logging out and appearing to.
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	// Nothing useful to do if the browser hung up mid-page.
	_, _ = buf.WriteTo(w)
}
