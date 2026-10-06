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

// staticFS holds Bootstrap 5.3.8, htmx 2.0.10 and hls.js 1.7.3, byte for byte
// as published: Bootstrap's dist/css/bootstrap.min.css and
// dist/js/bootstrap.bundle.min.js, which are identical to the copies jsDelivr
// serves, htmx's dist/htmx.min.js, and hls.js's dist/hls.light.min.js, which is
// the copy inside the npm tarball whose integrity npm publishes.
//
//	sha256 d85327d99c7a3ee1f9b5d0500d1370acea3ad2db39c163c2f51f232baedbdede  bootstrap.min.css
//	sha256 e4fd49181388c48ec5040bd3fe66f57c29c8e67fcd8502b3354b96ec7ab47cc7  bootstrap.bundle.min.js
//	sha256 71ea67185bfa8c98c39d31717c6fce5d852370fcdfd129db4543774d3145c0de  htmx.min.js
//	sha256 0251332c00a216a35d7d6919044d60da82b78beb3bd50ac6c662ae74a2f5474b  hls.light.min.js
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
	pageSearch = "search.html"
	// pageRows and pageTile are not pages: they are the listing's rows and one
	// cell of a grid of photographs, each of them shown by two pages.
	pageRows      = "rows.html"
	pageTile      = "tile.html"
	pageRename    = "rename.html"
	pageInfo      = "info.html"
	pageDelete    = "delete.html"
	pageStatus    = "status.html"
	pageTrash     = "trash.html"
	pageDestroy   = "destroy.html"
	pageShare     = "share.html"
	pageShared    = "shared.html"
	pageError     = "error.html"
	pageOffline   = "offline.html"
	pagePhotos    = "photos.html"
	pagePhoto     = "photo.html"
	pageMonths    = "months.html"
	pagePlay      = "play.html"
	pageArtists   = "artists.html"
	pageArtist    = "artist.html"
	pageAlbum     = "album.html"
	pageCrumbs    = "crumbs.html"
	pagePlaylists = "playlists.html"
	pagePlaylist  = "playlist.html"
)

// Each page is parsed with the layout into a set of its own. One set for all of
// them cannot work: every page defines the same "content" template, which is
// what lets the layout call it.
var pages = map[string]*template.Template{
	pageLogin:     parse(pageLogin),
	pageFiles:     parse(pageFiles, pageRows, pageCrumbs),
	pageSearch:    parse(pageSearch, pageRows, pageTile),
	pageRename:    parse(pageRename),
	pageInfo:      parse(pageInfo),
	pageDelete:    parse(pageDelete),
	pageStatus:    parse(pageStatus),
	pageTrash:     parse(pageTrash),
	pageDestroy:   parse(pageDestroy),
	pageShare:     parse(pageShare),
	pageShared:    parse(pageShared),
	pageError:     parse(pageError),
	pageOffline:   parse(pageOffline),
	pagePhotos:    parse(pagePhotos, pageTile, pageCrumbs),
	pagePhoto:     parse(pagePhoto),
	pageMonths:    parse(pageMonths, pageCrumbs),
	pagePlay:      parse(pagePlay),
	pageArtists:   parse(pageArtists),
	pageArtist:    parse(pageArtist),
	pageAlbum:     parse(pageAlbum),
	pagePlaylists: parse(pagePlaylists),
	pagePlaylist:  parse(pagePlaylist),
}

// parse builds one page's template set: the layout, the page, and whatever
// partials it shares with another page. A set per page because every page
// defines "content", so one set for all of them would keep the last.
func parse(page string, partials ...string) *template.Template {
	files := make([]string, 0, 2+len(partials))
	files = append(files, "templates/layout.html", "templates/"+page)
	for _, p := range partials {
		files = append(files, "templates/"+p)
	}
	return template.Must(template.ParseFS(templateFS, files...))
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
	// Favicon carries the build on the end for the same reason the scripts do:
	// it sits under the immutable cache header with no version in its path.
	Favicon string
	// AppleIcon is the same picture as the manifest's, in the one place a
	// manifest does not reach: iOS takes a home screen icon from this link and
	// from nothing else.
	AppleIcon string
	// Scripts are this project's own, with the build on the end so a new one is
	// a new URL. Three of them, on every page: copy.js, dialog.js and the one
	// that registers the service worker.
	Scripts []string
	// User is who is signed in, and empty when nobody is.
	User string
	// Gallery names the library this page belongs to, which the bar's menu
	// wears instead of "Gallery" so that it says where you are. Empty
	// everywhere else, including the player, which is reached from a listing
	// as often as from the videos.
	Gallery string
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
	// Columns and Rows are the listing's own controls: one link per ordering,
	// and one per page size on offer.
	Columns []column
	Rows    []rowChoice
	// Query is what was typed into the search box, on every page so the box
	// keeps it, and the five fields under it are a result: a bucket of rows
	// and the link to the rest of each. Only is the bucket a result page was
	// narrowed to, empty when it shows them all.
	Query        string
	Found        []foundTrack
	FoundArtists []foundArtist
	FoundAlbums  []foundAlbum
	MoreTracks   string
	MorePhotos   string
	MoreArtists  string
	MoreAlbums   string
	Only         string
	// Counts and the three fields under it are the status page: how much of the
	// library has been looked at, and by which extractor.
	Counts       db.MediaCounts
	Percent      int
	IndexVersion int
	IndexingOff  bool
	// FreeSpace is what the blob store says is left, already rendered, and
	// empty when it would not say.
	FreeSpace string
	// Incoming is the import folder, and nil when none is configured.
	Incoming *importsView
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
	// Info is what is known about one file: the page, and the row a listing
	// opens under it.
	Info *infoView
	// Tiles is one page of a grid by date, and Photo the one photograph the
	// viewer shows. Months is a year's, on the page between the two, and Empty
	// is what a grid with nothing in it says -- which differs by library.
	Tiles  []tile
	Photo  *photoView
	Months []crumb
	Empty  string
	// Film is the player page, and PlayerScripts the two scripts it loads
	// when it needs HLS: hls.js and this project's own that starts it.
	Film          *filmView
	PlayerScripts []string

	Artists []artistRow
	Albums  []albumRow
	Album   *albumView
	// Playlists is the list of them and Playlist the one being shown.
	Playlists []playlistRow
	Playlist  *playlistView

	// Deletions is a page of the trash, TrashSize what it is holding onto in
	// all, and Kept how many days a deletion is given.
	Deletions []deletion
	// Unaccounted is the other kind of trash row: blobs the sweep found that
	// no row claims, which have no path and therefore no way back.
	Unaccounted []deletion
	TrashSize   string
	// Unclaimed is what those are holding, rendered, and empty when there are
	// none -- which is what a healthy server looks like.
	Unclaimed string
	Kept      int
}

// Shows reports whether a result page is showing one of its buckets: all of
// them, or the one it was narrowed to. A method rather than a field per bucket,
// because five of those in the template is five chances to spell one wrong.
func (v view) Shows(bucket string) bool { return v.Only == "" || v.Only == bucket }

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
	build := "?v=" + url.QueryEscape(h.version)
	v.Scripts = []string{
		ownPrefix + "/copy.js" + build,
		ownPrefix + "/dialog.js" + build,
		ownPrefix + "/register.js" + build,
	}
	v.Favicon = ownPrefix + "/favicon.svg" + build
	v.AppleIcon = ownPrefix + "/apple-touch-icon.png" + build

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
