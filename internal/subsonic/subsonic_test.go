package subsonic_test

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/C0piIot/stratus-backend/internal/auth"
	"github.com/C0piIot/stratus-backend/internal/db"
	"github.com/C0piIot/stratus-backend/internal/db/sqlite"
	"github.com/C0piIot/stratus-backend/internal/files"
	"github.com/C0piIot/stratus-backend/internal/media"
	"github.com/C0piIot/stratus-backend/internal/storage/disk"
	"github.com/C0piIot/stratus-backend/internal/subsonic"
)

const (
	prefix = "/rest/"
	// serverVersion is deliberately not the real one: the envelope has to carry
	// the build back, and a hard-coded "1.16.1" would pass a test that used it.
	serverVersion = "9.9.9-test"

	username = "edu"
	password = "an example password"
)

// server is the real handler over the real everything: the real verifier with
// its throttle, a real SQLite database and real blobs on disk. There are no
// fakes here for the reason the WebDAV tests have none -- an adapter tested
// against a fake tests the fake.
func server(t *testing.T) http.Handler {
	return newLibrary(t)
}

// library is a running adapter plus the two halves of putting music into it: a
// file is a blob and a row, and a track is that plus a metadata row.
type library struct {
	http.Handler
	files *files.Service
	meta  *sqlite.Store
	blobs *disk.Store
	art   *media.Thumbs
	// verifier is kept so a test can build a second handler over the same
	// credentials -- the failure cases do, with a library that breaks.
	verifier *auth.Throttle
	// arrived and arrivals are what give each fixture file a distinct arrival
	// time: three uploads inside one millisecond arrive at the same truncated
	// instant, and a listing by arrival would then be decided by its tie-break
	// rather than by the order a test wrote them in. Counted forward from now
	// so that a file is always newer than the directory made to hold it.
	arrived  time.Time
	arrivals int
}

func newLibrary(t *testing.T) *library {
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
	thumbs := media.NewThumbs(blobs, service)
	verifier := auth.NewThrottle(auth.Credentials{Username: username, Password: password}, auth.DefaultThrottle)
	return &library{
		Handler:  subsonic.Handler(prefix, serverVersion, verifier, meta, service, thumbs),
		files:    service,
		meta:     meta,
		blobs:    blobs,
		art:      thumbs,
		verifier: verifier,
		arrived:  time.Now().UTC(),
	}
}

// write stores a file and stamps it with the next arrival time. It is the one
// place the fixtures touch the file layer.
func (l *library) write(t *testing.T, p, body string) db.File {
	t.Helper()
	l.mkdirAll(t, db.ParentOf(p))

	f, err := l.files.Write(t.Context(), username, p, strings.NewReader(body), int64(len(body)), "audio/flac")
	if err != nil {
		t.Fatalf("Write(%q): %v", p, err)
	}

	l.arrivals++
	f.MTime = l.arrived.Add(time.Duration(l.arrivals) * time.Minute)
	if f, err = l.meta.PutFile(t.Context(), f); err != nil {
		t.Fatalf("stamping %q: %v", p, err)
	}
	return f
}

// add stores a file with metadata. The body is the path, so a stream can be
// told apart from any other file in one assertion.
func (l *library) add(t *testing.T, p string, m db.Media) db.File {
	t.Helper()
	return l.index(t, l.write(t, p, p), m)
}

// addSized is add with a body of a chosen length, for the one property that is
// computed from it.
func (l *library) addSized(t *testing.T, p string, m db.Media, size int) db.File {
	t.Helper()
	return l.index(t, l.write(t, p, strings.Repeat("x", size)), m)
}

// addUnindexed is a file the indexer has not reached, which is not music yet.
func (l *library) addUnindexed(t *testing.T, p string) db.File {
	t.Helper()
	return l.write(t, p, p)
}

func (l *library) index(t *testing.T, f db.File, m db.Media) db.File {
	t.Helper()
	if m.Kind == "" {
		m.Kind = db.KindAudio
	}
	m.FileID = f.ID
	m.IndexedAt = time.Now()
	m.Version = 1
	if err := l.meta.PutMedia(t.Context(), m); err != nil {
		t.Fatalf("PutMedia(%q): %v", f.Path, err)
	}
	return f
}

func (l *library) mkdirAll(t *testing.T, dir string) {
	t.Helper()
	if dir == "" {
		return
	}
	var built string
	for seg := range strings.SplitSeq(dir, "/") {
		if built != "" {
			built += "/"
		}
		built += seg
		if _, err := l.files.Mkdir(t.Context(), username, built); err != nil && !errors.Is(err, db.ErrConflict) {
			t.Fatalf("Mkdir(%q): %v", built, err)
		}
	}
}

// song is a plausible set of tags, so a case only states what it is about.
func song(albumArtist, album, title string, trackNo int) db.Media {
	return db.Media{
		DurationMS:  254_600,
		Codec:       "flac",
		AlbumArtist: albumArtist,
		Artist:      albumArtist,
		Album:       album,
		Title:       title,
		TrackNo:     trackNo,
		Year:        1997,
		Genre:       "Electronic",
	}
}

// query is the credentials every request needs, in the password form. c is
// separate from the credentials in the protocol but not in practice: without it
// nothing is authenticated at all.
func query(extra ...string) string {
	q := url.Values{"c": {"stratus-tests"}, "u": {username}, "p": {password}}
	for i := 0; i+1 < len(extra); i += 2 {
		q.Set(extra[i], extra[i+1])
	}
	return q.Encode()
}

// get drives one call. The trailing pairs are request headers, which only the
// range case needs.
func get(t *testing.T, h http.Handler, method, rawQuery string, headers ...string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, request(t, method, rawQuery, headers...))
	return rec
}

func request(t *testing.T, method, rawQuery string, headers ...string) *http.Request {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, prefix+method+"?"+rawQuery, nil)
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	return req
}

// response is the envelope as a client's parser sees it, which is the point of
// going through JSON rather than decoding into the adapter's own types: a wrong
// tag would round-trip through those without a complaint.
func response(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("HTTP %d: every Subsonic answer is a 200, errors included", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Errorf("Content-Type = %q", got)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding %q: %v", rec.Body.String(), err)
	}
	env, ok := body["subsonic-response"].(map[string]any)
	if !ok {
		t.Fatalf("no subsonic-response object in %q", rec.Body.String())
	}
	return env
}

// TestPingIsXMLByDefault pins the whole document, attributes and namespace
// included. It is an exact comparison on purpose: `xml:"status"` instead of
// `xml:"status,attr"` produces a child element, which is valid XML, decodes
// back into this adapter's own structs, and is refused by real clients.
func TestPingIsXMLByDefault(t *testing.T) {
	t.Parallel()

	// No f parameter: the specification's default is XML, so a server that
	// answered JSON here would be unusable by anything that trusts the default.
	rec := get(t, server(t), "ping", query())
	if rec.Code != http.StatusOK {
		t.Fatalf("ping = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/xml; charset=utf-8" {
		t.Errorf("Content-Type = %q", got)
	}

	want := `<?xml version="1.0" encoding="UTF-8"?>` + "\n" +
		`<subsonic-response xmlns="http://subsonic.org/restapi" status="ok" version="1.16.1"` +
		` type="Stratus" serverVersion="9.9.9-test" openSubsonic="true"></subsonic-response>`
	if got := rec.Body.String(); got != want {
		t.Errorf("ping =\n%s\nwant\n%s", got, want)
	}
}

// TestPingJSON is the same envelope in the other serialisation, and the three
// fields OpenSubsonic added to it are what a client reads to decide this is not
// a plain Subsonic server.
func TestPingJSON(t *testing.T) {
	t.Parallel()
	env := response(t, get(t, server(t), "ping", query("f", "json")))

	for _, tt := range []struct{ key, want string }{
		{"status", "ok"},
		{"version", "1.16.1"},
		{"type", "Stratus"},
		{"serverVersion", serverVersion},
	} {
		if got, _ := env[tt.key].(string); got != tt.want {
			t.Errorf("%s = %v, want %q", tt.key, env[tt.key], tt.want)
		}
	}
	// A boolean, not the string "true": the XML form is text and the JSON form
	// is not, and rendering both from one value is where that gets confused.
	if got, ok := env["openSubsonic"].(bool); !ok || !got {
		t.Errorf("openSubsonic = %#v, want true as a boolean", env["openSubsonic"])
	}
}

// TestTheViewSuffixIsTheSameEndpoint is not cosmetic: the specification
// documents /ping and every example in it calls /ping.view, and Feishin sends
// the suffix on everything.
func TestTheViewSuffixIsTheSameEndpoint(t *testing.T) {
	t.Parallel()
	h := server(t)

	plain := get(t, h, "ping", query()).Body.String()
	suffixed := get(t, h, "ping.view", query()).Body.String()
	if plain != suffixed {
		t.Errorf("ping.view =\n%s\nping =\n%s", suffixed, plain)
	}
}

// TestErrorsTravelInsideA200 covers the codes this adapter produces. A client
// that receives a transport error cannot read the reason, which is why the
// protocol puts the status in the body.
func TestErrorsTravelInsideA200(t *testing.T) {
	t.Parallel()
	h := server(t)

	tests := []struct {
		name   string
		method string
		query  url.Values
		want   float64
	}{
		{
			name:   "no client name",
			method: "ping",
			query:  url.Values{"u": {username}, "p": {password}},
			want:   10,
		},
		{
			name:   "no username",
			method: "ping",
			query:  url.Values{"c": {"tests"}, "p": {password}},
			want:   10,
		},
		{
			name:   "no credentials of either kind",
			method: "ping",
			query:  url.Values{"c": {"tests"}, "u": {username}},
			want:   10,
		},
		{
			// 43 exists for exactly this: two mechanisms at once is a client
			// bug, and answering "wrong password" would send it to re-prompt.
			name:   "both a password and a token",
			method: "ping",
			query:  url.Values{"c": {"tests"}, "u": {username}, "p": {password}, "t": {"x"}, "s": {"y"}},
			want:   43,
		},
		{
			name:   "a password that is not valid hex after enc:",
			method: "ping",
			query:  url.Values{"c": {"tests"}, "u": {username}, "p": {"enc:zzzz"}},
			want:   10,
		},
		{
			name:   "the wrong password",
			method: "ping",
			query:  url.Values{"c": {"tests"}, "u": {username}, "p": {"not it"}},
			want:   40,
		},
		{
			name:   "the right password for another user",
			method: "ping",
			query:  url.Values{"c": {"tests"}, "u": {"someone-else"}, "p": {password}},
			want:   40,
		},
		{
			// A method this server does not implement answers an envelope, not
			// an HTML 404: clients probe endpoints to decide which of their own
			// features to offer, and a page they cannot parse tells them
			// nothing.
			name:   "a method that does not exist",
			method: "getPodcasts",
			query:  url.Values{"c": {"tests"}, "u": {username}, "p": {password}},
			want:   70,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tt.query.Set("f", "json")
			env := response(t, get(t, h, tt.method, tt.query.Encode()))

			if got, _ := env["status"].(string); got != "failed" {
				t.Errorf("status = %v, want failed", env["status"])
			}
			apiErr, ok := env["error"].(map[string]any)
			if !ok {
				t.Fatalf("no error object in %v", env)
			}
			if got, _ := apiErr["code"].(float64); got != tt.want {
				t.Errorf("code = %v, want %v", apiErr["code"], tt.want)
			}
			// A code without a message is not a diagnosis, and the operator
			// reading a client's log is the only person who will see this.
			if msg, _ := apiErr["message"].(string); msg == "" {
				t.Error("the error carries no message")
			}
		})
	}
}

// TestTokenAuth is the scheme every current client uses; the password form is
// what the ones that predate it send.
func TestTokenAuth(t *testing.T) {
	t.Parallel()
	h := server(t)
	const salt = "c19b2d"

	tests := []struct {
		name  string
		query url.Values
		want  string
	}{
		{
			name:  "the right token",
			query: url.Values{"c": {"tests"}, "u": {username}, "t": {digest(password, salt)}, "s": {salt}},
			want:  "ok",
		},
		{
			name:  "a wrong token",
			query: url.Values{"c": {"tests"}, "u": {username}, "t": {digest("not it", salt)}, "s": {salt}},
			want:  "failed",
		},
		{
			// A digest over no salt is a constant, and a constant is a password
			// that can be replayed.
			name:  "no salt",
			query: url.Values{"c": {"tests"}, "u": {username}, "t": {digest(password, "")}},
			want:  "failed",
		},
		{
			// enc: is hex, not encryption. The specification says so itself and
			// tells clients not to use it outside testing.
			name:  "the password in the enc: form",
			query: url.Values{"c": {"tests"}, "u": {username}, "p": {"enc:" + hex.EncodeToString([]byte(password))}},
			want:  "ok",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tt.query.Set("f", "json")
			env := response(t, get(t, h, "ping", tt.query.Encode()))
			if got, _ := env["status"].(string); got != tt.want {
				t.Errorf("status = %v, want %s: %v", env["status"], tt.want, env["error"])
			}
		})
	}
}

func digest(pass, salt string) string {
	sum := md5.Sum([]byte(pass + salt))
	return hex.EncodeToString(sum[:])
}

// passwordOnly is a verifier holding nothing a digest can be recomputed from,
// which is the case error 41 exists for. Nothing wires one today -- Credentials
// keeps the password precisely so it can answer a token -- but the protocol
// distinguishes "wrong password" from "this server cannot check a token", and
// the distinction is only worth having if it arrives intact.
type passwordOnly struct{}

func (passwordOnly) Verify(context.Context, string, string) error { return nil }

func TestTokenAuthAgainstAVerifierThatCannotAnswerIt(t *testing.T) {
	t.Parallel()

	throttle := auth.NewThrottle(passwordOnly{}, auth.DefaultThrottle)
	h := subsonic.Handler(prefix, serverVersion, throttle, nil, nil, nil)

	q := url.Values{"c": {"tests"}, "u": {username}, "t": {"whatever"}, "s": {"salt"}, "f": {"json"}}
	if code := errorCode(t, get(t, h, "ping", q.Encode())); code != 41 {
		t.Errorf("code = %v, want 41", code)
	}
}

// TestGetOpenSubsonicExtensionsNeedsNoCredentials is the specification's one
// public endpoint, and it has to be: a client asks what a server supports
// before it has anywhere to send a password.
func TestGetOpenSubsonicExtensionsNeedsNoCredentials(t *testing.T) {
	t.Parallel()

	env := response(t, get(t, server(t), "getOpenSubsonicExtensions", "f=json"))
	if got, _ := env["status"].(string); got != "ok" {
		t.Fatalf("status = %v, want ok: this endpoint takes no credentials", env["status"])
	}
	// Present and empty, which is the protocol's capability signal: absent
	// means "does not support extensions at all", and that is a different
	// claim from "supports none".
	list, ok := env["openSubsonicExtensions"].([]any)
	if !ok {
		t.Fatalf("openSubsonicExtensions = %#v, want an array", env["openSubsonicExtensions"])
	}
	if len(list) != 0 {
		t.Errorf("openSubsonicExtensions = %v, want empty until one is implemented", list)
	}
}

// TestGetLicense answers valid, always. There is nothing to license, and DSub
// will not browse a server that says otherwise.
func TestGetLicense(t *testing.T) {
	t.Parallel()

	env := response(t, get(t, server(t), "getLicense", query("f", "json")))
	lic, ok := env["license"].(map[string]any)
	if !ok {
		t.Fatalf("no license object in %v", env)
	}
	if valid, _ := lic["valid"].(bool); !valid {
		t.Errorf("valid = %#v, want true as a boolean", lic["valid"])
	}
}

// TestGetMusicFoldersIDIsANumber is the one typing trap in the schema:
// MusicFolder/@id is xs:int while every other id in the API is xs:string, so in
// JSON it is 1 and not "1". A strict client parses it as a number and fails on
// the string.
func TestGetMusicFoldersIDIsANumber(t *testing.T) {
	t.Parallel()

	rec := get(t, server(t), "getMusicFolders", query("f", "json"))
	env := response(t, rec)
	folders, ok := env["musicFolders"].(map[string]any)
	if !ok {
		t.Fatalf("no musicFolders object in %v", env)
	}
	list, ok := folders["musicFolder"].([]any)
	if !ok || len(list) != 1 {
		t.Fatalf("musicFolder = %#v, want one folder", folders["musicFolder"])
	}
	folder, _ := list[0].(map[string]any)
	if _, ok := folder["id"].(float64); !ok {
		t.Errorf("id = %#v, want a number", folder["id"])
	}
	if strings.Contains(rec.Body.String(), `"id":"`) {
		t.Errorf("the folder id is quoted: %s", rec.Body.String())
	}
}

// TestGetUserIsHonest is why this endpoint is not optional: it is Feishin's
// connection test, and every client reads the roles to decide what to offer. A
// false role is a feature not shown; a true one it cannot use is an error later.
func TestGetUserIsHonest(t *testing.T) {
	t.Parallel()

	env := response(t, get(t, server(t), "getUser", query("f", "json")))
	u, ok := env["user"].(map[string]any)
	if !ok {
		t.Fatalf("no user object in %v", env)
	}
	if got, _ := u["username"].(string); got != username {
		t.Errorf("username = %v, want %q", u["username"], username)
	}

	// Every role is present, true or false, because absence is not the same
	// claim as false and a client is entitled to read all of them.
	want := map[string]bool{
		"scrobblingEnabled":   false,
		"adminRole":           false,
		"settingsRole":        false,
		"downloadRole":        true,
		"uploadRole":          false,
		"playlistRole":        false,
		"coverArtRole":        false,
		"commentRole":         false,
		"podcastRole":         false,
		"streamRole":          true,
		"jukeboxRole":         false,
		"shareRole":           false,
		"videoConversionRole": false,
	}
	for role, expected := range want {
		got, ok := u[role].(bool)
		if !ok {
			t.Errorf("%s is missing or not a boolean: %#v", role, u[role])
			continue
		}
		if got != expected {
			t.Errorf("%s = %v, want %v", role, got, expected)
		}
	}
}

// TestThrottledLoginIsNotARejection is the distinction the generic code exists
// to preserve here: the credentials were never judged, so answering 40 would
// send a client to re-prompt for a password that may well be right.
func TestThrottledLoginIsNotARejection(t *testing.T) {
	t.Parallel()

	// One free failure and then an hour's wait, which is refused rather than
	// held: it makes the second guess deterministic and instant.
	creds := auth.Credentials{Username: username, Password: password}
	throttle := auth.NewThrottle(creds, auth.ThrottleConfig{Every: time.Hour, Burst: 1, MaxWait: 0})
	h := subsonic.Handler(prefix, serverVersion, throttle, nil, nil, nil)

	wrong := url.Values{"c": {"tests"}, "u": {username}, "p": {"not it"}, "f": {"json"}}.Encode()
	if code := errorCode(t, get(t, h, "ping", wrong)); code != 40 {
		t.Fatalf("the first wrong password = %v, want 40", code)
	}
	if code := errorCode(t, get(t, h, "ping", wrong)); code != 0 {
		t.Errorf("a guess with the bucket empty = %v, want the generic code", code)
	}
}

func errorCode(t *testing.T, rec *httptest.ResponseRecorder) float64 {
	t.Helper()
	env := response(t, rec)
	apiErr, ok := env["error"].(map[string]any)
	if !ok {
		t.Fatalf("no error object in %v", env)
	}
	code, _ := apiErr["code"].(float64)
	return code
}

// TestErrorInXML is the error envelope in the default serialisation, where the
// code and the message are attributes of a nested element. Two shapes to get
// wrong instead of one, and no client complains in a way an author would see.
func TestErrorInXML(t *testing.T) {
	t.Parallel()

	q := url.Values{"c": {"tests"}, "u": {username}, "p": {"not it"}}.Encode()
	rec := get(t, server(t), "ping", q)

	want := `<?xml version="1.0" encoding="UTF-8"?>` + "\n" +
		`<subsonic-response xmlns="http://subsonic.org/restapi" status="failed" version="1.16.1"` +
		` type="Stratus" serverVersion="9.9.9-test" openSubsonic="true">` +
		`<error code="40" message="wrong username or password"></error>` +
		`</subsonic-response>`
	if got := rec.Body.String(); got != want {
		t.Errorf("a failed ping =\n%s\nwant\n%s", got, want)
	}
}

// brokenWriter is a client that hung up mid-response. Both serialisations have
// a branch for that and neither is reachable through httptest, which records
// every write as a success.
type brokenWriter struct {
	header http.Header
	// ok is how many writes succeed before the connection is reported gone.
	// Zero breaks the XML declaration; one lets it through and breaks the
	// document that follows.
	ok int
}

func (b *brokenWriter) Header() http.Header {
	if b.header == nil {
		b.header = http.Header{}
	}
	return b.header
}

func (b *brokenWriter) WriteHeader(int) {}

func (b *brokenWriter) Write(p []byte) (int, error) {
	if b.ok == 0 {
		return 0, errors.New("broken pipe")
	}
	b.ok--
	return len(p), nil
}

func TestAClientThatHangsUpMidResponse(t *testing.T) {
	t.Parallel()
	h := server(t)

	tests := []struct {
		name  string
		query string
		ok    int
	}{
		{name: "the xml declaration fails", query: query(), ok: 0},
		{name: "the xml document fails", query: query(), ok: 1},
		{name: "the json document fails", query: query("f", "json"), ok: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			// The assertion is that the handler returns: there is nothing to
			// send an error to, so a log line is the whole remedy.
			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, prefix+"ping?"+tt.query, nil)
			h.ServeHTTP(&brokenWriter{ok: tt.ok}, req)
		})
	}
}
