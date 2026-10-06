package web

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
)

// The service worker, which is what makes this UI installable (#129): a
// manifest says what the app is called and no browser offers to install one
// without a worker that has a fetch handler.
//
// **It is served from the root rather than from /static/**, because a worker
// controls the directory it was served from and one under /static/stratus/
// would control nothing worth controlling. In front of the session like the
// manifest and robots.txt, since a worker that answered a redirect to /login
// would fail to register, and under no-cache, because this is the one file
// that must never be stale: a browser decides there is a new version by
// comparing the bytes.
//
// **What it precaches is written here and not in the file.** Those addresses
// are the ones the layout links and the constants in web.go are where they are
// spelled; a second copy of them in JavaScript is exactly the drift this
// project keeps tests for elsewhere. So the handler writes the version and the
// list as constants in front of the script -- which also means the bytes
// change when the build does, and a browser re-registers without being asked.
//
// **It caches nothing that needs a session.** The worker writes to the cache
// while it installs and never again, so a listing cannot be left behind for
// whoever opens the browser next; `TestTheWorkerCachesOnlyWhileItInstalls` is
// what keeps that true.
const (
	workerPath  = "/sw.js"
	offlinePath = "/offline"
	// workerSource is the file the handler wraps. It is in the static tree
	// because that is where this project's scripts live, and it is not served
	// from there for the scope reason above.
	workerSource = "static/stratus/sw.js"
)

// shell is what the worker precaches: enough for a page to draw itself with no
// network, and nothing that a session would be needed to fetch.
//
// The player's two scripts are deliberately out. They are loaded on one page
// and only for a film that needs them, and a film is not a thing to watch with
// the server unreachable -- the bytes are on the server.
func (h *handler) shell() []string {
	build := "?v=" + url.QueryEscape(h.version)
	return []string{
		offlinePath,
		assetPrefix + "/bootstrap.min.css",
		assetPrefix + "/bootstrap.bundle.min.js",
		htmxPrefix + "/htmx.min.js",
		ownPrefix + "/copy.js" + build,
		ownPrefix + "/dialog.js" + build,
		ownPrefix + "/register.js" + build,
		ownPrefix + "/favicon.svg" + build,
	}
}

func (h *handler) worker(w http.ResponseWriter, _ *http.Request) {
	body, err := staticFS.ReadFile(workerSource)
	if err != nil {
		// The file is embedded in this binary, so this is a build that cannot
		// happen rather than an operator's problem.
		slog.Error("reading the service worker", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	shell, err := json.Marshal(h.shell())
	if err != nil {
		slog.Error("writing the service worker's shell", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = fmt.Fprintf(w, "const VERSION = %q;\nconst OFFLINE = %q;\nconst SHELL = %s;\n",
		h.version, offlinePath, shell)
	_, _ = w.Write(body)
}

// offline is the page a navigation gets when the server cannot be reached. It
// is in front of the session because that is the only state it is ever shown
// in: no network, and therefore nothing to check a cookie against.
func (h *handler) offline(w http.ResponseWriter, _ *http.Request) {
	h.render(w, http.StatusOK, pageOffline, view{Title: "Offline"})
}
