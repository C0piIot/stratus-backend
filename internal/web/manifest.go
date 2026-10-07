package web

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/url"
)

// The web app manifest (#129): what makes an icon added to a home screen say
// Stratus rather than carry the first letter of a URL, and the first of the
// two things a browser reads before it will offer to install anything.
//
// **It is answered in front of the session**, like robots.txt and for a
// sharper reason than that one: a browser fetches a manifest with credentials
// omitted unless the link element asks otherwise, so behind the login this
// would answer a redirect to /login and the browser would quietly have no
// manifest at all -- the same shape of failure the content policy keeps
// producing, with nothing on the server to see. There is nothing in it to
// protect: a name, two icons and six pictures of this UI. The same is true of
// everything it names, which is why those live under /static/ with the rest of
// what the pages are made of.
//
// **It is JSON and it is not an API.** The format is the W3C's and the client
// is the browser itself, which is the standing robots.txt has; no page here
// asks it for anything, and nothing of ours parses one.
//
// **It does not make the UI installable on its own, and that is where it
// leaves #129.** Chrome asks for a service worker with a fetch handler before
// it offers to install, and what that worker should do with a request it
// cannot answer is the decision that issue is labelled for. What a manifest
// alone buys is the icon, the name and the colours when somebody adds the page
// to a home screen by hand -- which on iOS is the only way there is, since
// Safari offers no prompt and reads no manifest for the icon. That is what the
// apple-touch-icon in the layout is for.
//
// The photographs inside the screenshots are the demo bundle's, which is CC0
// (scripts/demo/CREDITS.md).

const (
	manifestPath = "/manifest.webmanifest"
	// manifestType is the registered one. A browser accepts application/json
	// as well, which is exactly why it is worth getting right: the wrong one
	// still works and says nothing.
	manifestType = "application/manifest+json"
	// themeColour is the bar's own. The UI does not follow the system's dark
	// mode -- there is no data-bs-theme on it -- so a colour that did would be
	// the browser's chrome disagreeing with the page under it.
	themeColour = "#ffffff"
)

// manifestIcon is one icon in it. Both of ours are declared "any maskable"
// because the source is drawn for it: brand/app-icon.svg is full bleed, with
// the mark inside Android's 66% safe circle, so a launcher that masks it and
// one that does not are both served by the same picture rather than by two.
type manifestIcon struct {
	Src     string `json:"src"`
	Sizes   string `json:"sizes"`
	Type    string `json:"type"`
	Purpose string `json:"purpose"`
}

// manifestShot is one screenshot. The form factor is what decides where it is
// shown: a browser installing on a desktop offers the wide ones and a phone
// the narrow ones, and a set with only one kind in it is a set half of them
// ignore. The label is read out rather than drawn.
type manifestShot struct {
	Src        string `json:"src"`
	Sizes      string `json:"sizes"`
	Type       string `json:"type"`
	FormFactor string `json:"form_factor"`
	Label      string `json:"label"`
}

// manifestShortcut is one of the four libraries, which is what a long press on
// the icon offers on Android and a right click on it on a desktop.
//
// **No icons on them, and that is a decision rather than an omission**: an icon
// per shortcut is a drawing this project does not have, and the same one four
// times says less than none -- a launcher with nothing to draw falls back to
// the app's own icon, which is what four copies of it would have been.
type manifestShortcut struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	URL         string `json:"url"`
}

// manifestShareTarget is the phone's share sheet pointed at this server
// (#312). The field is the one the upload form already uses, so both doors
// take the same multipart; what it accepts is wide because a share sheet is
// where everything on a phone ends up, and the handler is what decides.
type manifestShareTarget struct {
	Action  string `json:"action"`
	Method  string `json:"method"`
	Enctype string `json:"enctype"`
	Params  struct {
		Files []manifestShareFile `json:"files"`
	} `json:"params"`
}

type manifestShareFile struct {
	Name   string   `json:"name"`
	Accept []string `json:"accept"`
}

// shareTarget is that member, built once because nothing in it varies.
func shareTarget() manifestShareTarget {
	t := manifestShareTarget{
		Action:  shareTargetPath,
		Method:  http.MethodPost,
		Enctype: "multipart/form-data",
	}
	t.Params.Files = []manifestShareFile{
		{Name: shareField, Accept: []string{"image/*", "video/*", "audio/*", "*/*"}},
	}
	return t
}

// manifestLaunch is how a second launch is handled: `navigate-existing` means
// the window that is already open goes to the new address rather than a second
// one appearing beside it. One window is what a file browser should be, and it
// is also what a share arriving while the app is open should land in.
type manifestLaunch struct {
	ClientMode string `json:"client_mode"`
}

type webManifest struct {
	ID              string              `json:"id"`
	Name            string              `json:"name"`
	ShortName       string              `json:"short_name"`
	Description     string              `json:"description"`
	Lang            string              `json:"lang"`
	Dir             string              `json:"dir"`
	StartURL        string              `json:"start_url"`
	Scope           string              `json:"scope"`
	Display         string              `json:"display"`
	DisplayOverride []string            `json:"display_override"`
	Orientation     string              `json:"orientation"`
	Categories      []string            `json:"categories"`
	ThemeColor      string              `json:"theme_color"`
	BackgroundColor string              `json:"background_color"`
	LaunchHandler   manifestLaunch      `json:"launch_handler"`
	Icons           []manifestIcon      `json:"icons"`
	Screenshots     []manifestShot      `json:"screenshots"`
	Shortcuts       []manifestShortcut  `json:"shortcuts"`
	ShareTarget     manifestShareTarget `json:"share_target"`
}

// shortcuts are the bar's own Gallery menu said a second time, where a
// launcher can read it (#311). Four, because four is what Android shows.
var shortcuts = []manifestShortcut{
	{"Files", "Browse the tree", "/files/"},
	{"Photos", "Photographs by the month they were taken", "/photos/"},
	{"Videos", "Films by the month they were taken", "/videos/"},
	{"Music", "Artists and their albums", "/music/"},
}

// screenshots are JPEG rather than PNG, which is a decision about the size of
// this binary: a picture of a photo library is a photograph, and the gallery
// one is 1.1 MB as a PNG against 151 KB here. Six of them as PNG would have
// moved the budget in scripts/smoke.sh on their own.
var screenshots = []manifestShot{
	{"/static/stratus/screens/files-wide.jpg", "1280x800", "image/jpeg", "wide", "A folder, with what is in it"},
	{"/static/stratus/screens/photos-wide.jpg", "1280x800", "image/jpeg", "wide", "Photographs by the month they were taken"},
	{"/static/stratus/screens/music-wide.jpg", "1280x800", "image/jpeg", "wide", "An album, read from the tags"},
	{"/static/stratus/screens/files-narrow.jpg", "412x915", "image/jpeg", "narrow", "A folder, with what is in it"},
	{"/static/stratus/screens/photos-narrow.jpg", "412x915", "image/jpeg", "narrow", "Photographs by the month they were taken"},
	{"/static/stratus/screens/music-narrow.jpg", "412x915", "image/jpeg", "narrow", "An album, read from the tags"},
}

// manifest answers it, built here rather than served as a file so that every
// address in it carries the build: these pictures sit under the immutable
// cache header with no version in their paths, exactly as the favicon and the
// scripts do.
func (h *handler) manifest(w http.ResponseWriter, _ *http.Request) {
	build := "?v=" + url.QueryEscape(h.version)
	m := webManifest{
		ID:          "/",
		Name:        "Stratus",
		ShortName:   "Stratus",
		Description: "A self-hosted personal cloud for photos, files, music and video.",
		// The root, which sends a browser to the tree or to the login by
		// whether it has a session -- the same answer typing the address gives.
		StartURL: "/",
		Scope:    "/",
		Display:  "standalone",
		// **The order is the decision in here, not the list.** A browser takes
		// the first mode it supports, so `standalone` first means the window
		// this UI is laid out for -- and the two behind it are declared rather
		// than used. `window-controls-overlay` hands the title bar's strip to
		// the page, and the right-hand end of that strip is where the system's
		// own buttons sit *and* where this bar keeps its menus; honouring it
		// means reading `env(titlebar-area-*)` from a stylesheet this project
		// does not have (see CLAUDE.md). `tabbed` is the same shape of
		// promise. Moving either in front of `standalone` is a layout change
		// and a conversation, not a line in a manifest.
		DisplayOverride: []string{"standalone", "window-controls-overlay", "tabbed", "browser"},
		// It is a file browser: it reads both ways round and claims neither.
		Orientation: "any",
		// From the registry the specification keeps. What this is, in the words
		// a store would file it under.
		Categories: []string{"photo", "music", "productivity", "utilities"},
		// The UI is written in English, left to right, and says so where an
		// installer can read it rather than only in the html element.
		Lang:            "en",
		Dir:             "ltr",
		ThemeColor:      themeColour,
		BackgroundColor: themeColour,
		// One window: a second launch, or a share arriving while it is open,
		// goes to the window that is already there.
		LaunchHandler: manifestLaunch{ClientMode: "navigate-existing"},
		Icons: []manifestIcon{
			{ownPrefix + "/icon-192.png" + build, "192x192", "image/png", "any maskable"},
			{ownPrefix + "/icon-512.png" + build, "512x512", "image/png", "any maskable"},
		},
	}
	for _, s := range screenshots {
		s.Src += build
		m.Screenshots = append(m.Screenshots, s)
	}
	m.Shortcuts = shortcuts
	m.ShareTarget = shareTarget()

	w.Header().Set("Content-Type", manifestType)
	// An hour. It is read on the way to an install and then rarely, and what it
	// points at carries the build, so a new binary is a new set of addresses
	// whatever a cache is holding of this.
	w.Header().Set("Cache-Control", "public, max-age=3600")
	if err := json.NewEncoder(w).Encode(m); err != nil {
		slog.Error("writing the manifest", "err", err)
	}
}
