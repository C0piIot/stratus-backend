package app_test

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/C0piIot/stratus-backend/internal/app"
	"github.com/C0piIot/stratus-backend/internal/config"
)

// testConfig is the default configuration, for the tests that only care about
// the HTTP surface.
func testConfig(t *testing.T) config.Config {
	t.Helper()
	cfg, err := config.Load(nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return cfg
}

// runConfig is a configuration Run can actually start from: a data directory of
// its own, and with it the default file storage DSN underneath.
func runConfig(t *testing.T, vars map[string]string) config.Config {
	t.Helper()
	if _, ok := vars["STRATUS_DATA_DIR"]; !ok {
		vars["STRATUS_DATA_DIR"] = filepath.Join(t.TempDir(), "data")
	}
	vars["STRATUS_ADDR"] = freeAddr(t)
	// Off unless a test asks for it: the toolchain container has no ffprobe, and
	// requiring it is the point rather than an accident. The end-to-end path is
	// asserted by the smoke tests, inside the image that does have it.
	if _, ok := vars["STRATUS_INDEX_INTERVAL"]; !ok {
		vars["STRATUS_INDEX_INTERVAL"] = "0"
	}

	cfg, err := config.Load(func(key string) string { return vars[key] })
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return cfg
}

// TestMain silences the startup log line so test output stays readable.
func TestMain(m *testing.M) {
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	os.Exit(m.Run())
}

func TestHandlerHealthz(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		method     string
		path       string
		wantStatus int
		wantBody   string
	}{
		{name: "get returns ok", method: http.MethodGet, path: "/healthz", wantStatus: http.StatusOK, wantBody: "ok\n"},
		{name: "head is routed too", method: http.MethodHead, path: "/healthz", wantStatus: http.StatusOK},
		{name: "post is rejected", method: http.MethodPost, path: "/healthz", wantStatus: http.StatusMethodNotAllowed},
		{name: "delete is rejected", method: http.MethodDelete, path: "/healthz", wantStatus: http.StatusMethodNotAllowed},
		{name: "unknown path is not found", method: http.MethodGet, path: "/nope", wantStatus: http.StatusNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rec := httptest.NewRecorder()
			req := httptest.NewRequestWithContext(t.Context(), tt.method, tt.path, nil)
			app.New(testConfig(t), "test").Handler(app.Deps{}).ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			if tt.wantBody != "" && rec.Body.String() != tt.wantBody {
				t.Errorf("body = %q, want %q", rec.Body.String(), tt.wantBody)
			}
			if tt.wantStatus == http.StatusOK {
				if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
					t.Errorf("Content-Type = %q, want text/plain", ct)
				}
			}
		})
	}
}

// TestServerTimeouts pins the timeout policy. WriteTimeout must stay zero:
// media streaming responses are long-lived and a write deadline would truncate
// them mid-file. If someone "hardens" this by adding one, this test fails and
// explains why.
func TestServerTimeouts(t *testing.T) {
	t.Parallel()
	srv := app.New(config.Config{Addr: ":8080"}, "test").Server(app.Deps{})

	if srv.WriteTimeout != 0 {
		t.Errorf("WriteTimeout = %v, must stay 0 so media streams are not truncated", srv.WriteTimeout)
	}
	if srv.ReadHeaderTimeout <= 0 {
		t.Error("ReadHeaderTimeout must be set, it is the Slowloris guard")
	}
	if srv.IdleTimeout <= 0 {
		t.Error("IdleTimeout must be set")
	}
	if srv.Addr != ":8080" {
		t.Errorf("Addr = %q", srv.Addr)
	}
	if srv.Handler == nil {
		t.Error("Handler must be wired")
	}
}

func TestRunShutsDownCleanly(t *testing.T) {
	t.Parallel()
	if err := runToShutdown(t, runConfig(t, map[string]string{})); err != nil {
		t.Errorf("Run returned %v, want nil on graceful shutdown", err)
	}
}

// startupWait bounds how long a healthy server may take to be serving. Generous
// on purpose: it only ever costs time when something is broken.
const startupWait = 20 * time.Second

// freeAddr reserves a port and hands it back. Binding :0 would be tidier, but
// then the test cannot know where to knock.
func freeAddr(t *testing.T) string {
	t.Helper()
	var lc net.ListenConfig
	l, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a port: %v", err)
	}
	addr := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatalf("release the port: %v", err)
	}
	return addr
}

// runToShutdown starts Run, waits until it is actually serving, and stops it.
// It returns whatever Run returned, so the happy path and the refusals go
// through the same helper.
//
// It waits on the health endpoint rather than on a timer. A sleep long enough
// for a loaded CI runner is too long everywhere else, and one that is too short
// cancels the context in the middle of the startup probe -- which is exactly
// how this helper failed the first time it ran under -race.
func runToShutdown(t *testing.T, cfg config.Config) error {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- app.New(cfg, "test").Run(ctx) }()

	deadline := time.Now().Add(startupWait)
	for time.Now().Before(deadline) {
		select {
		case err := <-done:
			cancel()
			return err // refused before it ever listened
		default:
		}
		if app.Probe(cfg.Addr) == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	cancel()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after the context was cancelled")
		return nil
	}
}

// TestDepsCloseTolerantOfAPartialStart covers what a refused startup leaves
// behind: open fails halfway, and the deferred Close has to cope with a Deps
// where only one half was ever assigned.
func TestDepsCloseTolerantOfAPartialStart(t *testing.T) {
	t.Parallel()
	if err := (app.Deps{}).Close(); err != nil {
		t.Errorf("closing a zero Deps = %v, want nil", err)
	}
}

// liveServer starts the whole application over real backends and returns its
// base URL, which is the only way to test that a surface is actually wired:
// authentication, prefix, storage and database all at once.
func liveServer(t *testing.T, vars map[string]string) (string, func()) {
	t.Helper()
	cfg := runConfig(t, vars)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- app.New(cfg, "test").Run(ctx) }()

	deadline := time.Now().Add(startupWait)
	for time.Now().Before(deadline) {
		if app.Probe(cfg.Addr) == nil {
			break
		}
		select {
		case err := <-done:
			cancel()
			t.Fatalf("the server refused to start: %v", err)
		default:
		}
		time.Sleep(10 * time.Millisecond)
	}

	return "http://" + cfg.Addr, func() {
		cancel()
		<-done
	}
}

func TestWebDAVIsWiredAndAuthenticated(t *testing.T) {
	t.Parallel()
	const password = "an example password"
	base, stop := liveServer(t, map[string]string{
		"STRATUS_USERNAME": "edu",
		"STRATUS_PASSWORD": password,
	})
	defer stop()

	// Without credentials the surface exists and refuses.
	resp, err := http.Get(base + "/dav/") //nolint:noctx // the request context adds nothing here
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated PROPFIND = %d, want 401", resp.StatusCode)
	}
	if resp.Header.Get("WWW-Authenticate") == "" {
		t.Error("no challenge, so a client has nothing to answer")
	}

	// With them, a file survives a round trip through storage and the database.
	put, err := http.NewRequestWithContext(t.Context(), http.MethodPut, base+"/dav/notes.txt", strings.NewReader("hello"))
	if err != nil {
		t.Fatal(err)
	}
	put.SetBasicAuth("edu", password)
	if resp, err = http.DefaultClient.Do(put); err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("PUT = %d, want 201", resp.StatusCode)
	}

	get, err := http.NewRequestWithContext(t.Context(), http.MethodGet, base+"/dav/notes.txt", nil)
	if err != nil {
		t.Fatal(err)
	}
	get.SetBasicAuth("edu", password)
	resp, err = http.DefaultClient.Do(get)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "hello" {
		t.Errorf("GET returned %q", body)
	}
}

// TestWebDAVIsNotMountedWithoutCredentials is the fail-closed case: an install
// nobody has configured must not be a file server.
func TestWebDAVIsNotMountedWithoutCredentials(t *testing.T) {
	t.Parallel()
	base, stop := liveServer(t, map[string]string{})
	defer stop()

	resp, err := http.Get(base + "/dav/notes.txt") //nolint:noctx // as above
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET with no credentials configured = %d, want 404: the surface should not exist", resp.StatusCode)
	}
}

// TestSubsonicIsWired asserts the three things only the composition root can
// get wrong: the prefix, the version in the envelope, and that the surface
// authenticates from the query string instead of behind auth.Basic.
func TestSubsonicIsWired(t *testing.T) {
	t.Parallel()
	const password = "an example password"
	base, stop := liveServer(t, map[string]string{
		"STRATUS_USERNAME": "edu",
		"STRATUS_PASSWORD": password,
	})
	defer stop()

	// .view because that is what clients send, and the prefix is theirs rather
	// than ours: they append /rest/<method> to the URL an operator typed in.
	ok := answer(t, base+"/rest/ping.view?c=stratus-tests&u=edu&p="+url.QueryEscape(password))
	if !strings.Contains(ok, `status="ok"`) {
		t.Errorf("ping = %s", ok)
	}
	// The build reaches the envelope. A client reads it to decide whether to
	// ask again what this server supports.
	if !strings.Contains(ok, `serverVersion="test"`) {
		t.Errorf("the envelope does not carry the version: %s", ok)
	}

	// No credentials is an envelope with a code, not a 401 with a challenge:
	// this surface has no header to authenticate and no realm to name.
	refused := answer(t, base+"/rest/ping.view?c=stratus-tests")
	if !strings.Contains(refused, `status="failed"`) || !strings.Contains(refused, `code="10"`) {
		t.Errorf("an unauthenticated ping = %s", refused)
	}
	wrong := answer(t, base+"/rest/ping.view?c=stratus-tests&u=edu&p=not+it")
	if !strings.Contains(wrong, `code="40"`) {
		t.Errorf("a wrong password = %s", wrong)
	}
}

// TestSubsonicSharesTheRateLimitWithWebDAV is why app.Handler builds one
// verifier and hands it to both surfaces. Two throttles would be two budgets
// for guesses at the same single password, and the second one would be reached
// by changing a URL.
//
// The assertion is the delay rather than a refusal: the throttle answers the
// free failures immediately and holds the next, so a guess that arrives on the
// other surface having to wait is the shared counter, observable.
func TestSubsonicSharesTheRateLimitWithWebDAV(t *testing.T) {
	t.Parallel()
	base, stop := liveServer(t, map[string]string{
		"STRATUS_USERNAME": "edu",
		"STRATUS_PASSWORD": "an example password",
	})
	defer stop()

	// Spend the burst on WebDAV. auth.DefaultThrottle answers three failures
	// without waiting, so these are instant.
	for i := range 3 {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, base+"/dav/", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.SetBasicAuth("edu", "not it")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("failed Basic login %d = %d, want 401", i, resp.StatusCode)
		}
	}

	// The fourth guess arrives on the other surface, and finds the bucket empty.
	start := time.Now()
	body := answer(t, base+"/rest/ping.view?c=stratus-tests&u=edu&p=not+it")
	elapsed := time.Since(start)

	if !strings.Contains(body, `code="40"`) {
		t.Errorf("a wrong password = %s", body)
	}
	// Half of the configured second, so the assertion is about there being a
	// wait at all rather than about its exact length.
	if elapsed < 500*time.Millisecond {
		t.Errorf("the guess was answered in %v: the surfaces have separate rate limits", elapsed)
	}
}

// TestSubsonicIsNotMountedWithoutCredentials is TestWebDAVIsNotMountedWithout-
// Credentials for the other surface, and the same rule: an install nobody has
// configured is not a music server either.
func TestSubsonicIsNotMountedWithoutCredentials(t *testing.T) {
	t.Parallel()
	base, stop := liveServer(t, map[string]string{})
	defer stop()

	resp, err := http.Get(base + "/rest/ping.view") //nolint:noctx // as above
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("ping with no credentials configured = %d, want 404: the surface should not exist", resp.StatusCode)
	}
}

// TestWebUIIsWired asserts the one thing about this surface that only the
// composition root can get wrong. It is mounted at the root, so it answers
// everything the other patterns did not claim -- which is what makes a page out
// of a stray URL, and what would swallow /healthz if the routing were wrong.
func TestWebUIIsWired(t *testing.T) {
	t.Parallel()
	base, stop := liveServer(t, map[string]string{
		"STRATUS_USERNAME": "edu",
		"STRATUS_PASSWORD": "an example password",
	})
	defer stop()

	// The root wants a session, and says where the browser was going.
	resp := request(t, http.MethodGet, base+"/")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusSeeOther {
		t.Errorf("GET / = %d, want 303 to the login form", resp.StatusCode)
	}
	if got := resp.Header.Get("Location"); got != "/login?next=%2F" {
		t.Errorf("Location = %q", got)
	}

	// The form is served from the binary, Bootstrap and all.
	form := request(t, http.MethodGet, base+"/login")
	defer func() { _ = form.Body.Close() }()
	body, err := io.ReadAll(form.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `name="password"`) {
		t.Error("GET /login served no login form")
	}
	css := request(t, http.MethodGet, base+"/static/bootstrap-5.3.8/bootstrap.min.css")
	defer func() { _ = css.Body.Close() }()
	if css.StatusCode != http.StatusOK {
		t.Errorf("the stylesheet the form links = %d, want 200", css.StatusCode)
	}

	// And the endpoints registered before it still win, which is the assertion
	// that a catch-all deserves.
	health := request(t, http.MethodGet, base+"/healthz")
	defer func() { _ = health.Body.Close() }()
	if health.StatusCode != http.StatusOK {
		t.Errorf("GET /healthz behind the UI = %d, want 200", health.StatusCode)
	}
}

// TestWebUIIsNotMountedWithoutCredentials, for the reason the other two
// surfaces are not: there is nobody to sign in as.
func TestWebUIIsNotMountedWithoutCredentials(t *testing.T) {
	t.Parallel()
	base, stop := liveServer(t, map[string]string{})
	defer stop()

	resp := request(t, http.MethodGet, base+"/login")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET /login with no credentials configured = %d, want 404", resp.StatusCode)
	}
}

// request is a bare GET that does not follow redirects: where the server sends
// a browser is half of what these tests are about.
func request(t *testing.T, method, target string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, target, nil)
	if err != nil {
		t.Fatal(err)
	}
	client := http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// answer is a GET whose body is the whole response, which is what a Subsonic
// answer is.
func answer(t *testing.T, target string) string {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, target, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	// Every Subsonic answer is a 200, errors included.
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200", target, resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}
