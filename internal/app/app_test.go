package app_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/C0piIot/stratus-backend/internal/app"
	"github.com/C0piIot/stratus-backend/internal/config"
	"github.com/C0piIot/stratus-backend/internal/db/sqlite"
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

func TestRunRejectsUnwritableDataDir(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("root ignores mode bits, so this cannot fail as root")
	}
	dir := filepath.Join(t.TempDir(), "readonly")
	if err := os.Mkdir(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	// Port 0 would still bind; the point is that Run must refuse before it does.
	err := app.New(runConfig(t, map[string]string{"STRATUS_DATA_DIR": dir}), "test").Run(t.Context())
	if err == nil {
		t.Fatal("Run must refuse to start on an unwritable data dir")
	}
	if !strings.Contains(err.Error(), "not writable") {
		t.Errorf("error should say it is not writable, got %v", err)
	}
}

func TestHealthURL(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		addr    string
		want    string
		wantErr bool
	}{
		{name: "port only maps to loopback", addr: ":8080", want: "http://127.0.0.1:8080/healthz"},
		{name: "ipv4 wildcard maps to loopback", addr: "0.0.0.0:8080", want: "http://127.0.0.1:8080/healthz"},
		{name: "ipv6 wildcard maps to loopback", addr: "[::]:8080", want: "http://127.0.0.1:8080/healthz"},
		{name: "explicit host is kept", addr: "127.0.0.1:9000", want: "http://127.0.0.1:9000/healthz"},
		{name: "ipv6 literal is bracketed", addr: "[::1]:9000", want: "http://[::1]:9000/healthz"},
		{name: "hostname is kept", addr: "stratus.local:80", want: "http://stratus.local:80/healthz"},
		{name: "missing port is an error", addr: "127.0.0.1", wantErr: true},
		{name: "garbage is an error", addr: "not an address", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := app.HealthURL(tt.addr)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("HealthURL(%q) = %q, want error", tt.addr, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("HealthURL(%q): %v", tt.addr, err)
			}
			if got != tt.want {
				t.Errorf("HealthURL(%q) = %q, want %q", tt.addr, got, tt.want)
			}
		})
	}
}

func TestProbe(t *testing.T) {
	t.Parallel()

	t.Run("healthy server", func(t *testing.T) {
		t.Parallel()
		srv := httptest.NewServer(app.New(testConfig(t), "test").Handler(app.Deps{}))
		defer srv.Close()
		if err := app.Probe(strings.TrimPrefix(srv.URL, "http://")); err != nil {
			t.Errorf("Probe: %v", err)
		}
	})

	t.Run("unhealthy status is an error", func(t *testing.T) {
		t.Parallel()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer srv.Close()
		err := app.Probe(strings.TrimPrefix(srv.URL, "http://"))
		if err == nil {
			t.Fatal("Probe succeeded against a 500")
		}
		if !strings.Contains(err.Error(), "500") {
			t.Errorf("error should mention the status, got %v", err)
		}
	})

	t.Run("nothing listening is an error", func(t *testing.T) {
		t.Parallel()
		// Bind then immediately close to get a port nothing is listening on.
		srv := httptest.NewServer(app.New(testConfig(t), "test").Handler(app.Deps{}))
		hostPort := strings.TrimPrefix(srv.URL, "http://")
		srv.Close()
		if err := app.Probe(hostPort); err == nil {
			t.Error("Probe succeeded with nothing listening")
		}
	})

	t.Run("bad address is an error", func(t *testing.T) {
		t.Parallel()
		if err := app.Probe("not an address"); err == nil {
			t.Error("Probe succeeded on a malformed address")
		}
	})
}

func TestEnsureDataDir(t *testing.T) {
	t.Parallel()

	t.Run("creates a missing directory", func(t *testing.T) {
		t.Parallel()
		dir := filepath.Join(t.TempDir(), "data")
		if err := app.EnsureDataDir(dir); err != nil {
			t.Fatalf("EnsureDataDir: %v", err)
		}
		if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
			t.Fatalf("directory not created: %v", err)
		}
	})

	t.Run("creates missing parents", func(t *testing.T) {
		t.Parallel()
		if err := app.EnsureDataDir(filepath.Join(t.TempDir(), "a", "b", "c")); err != nil {
			t.Fatalf("EnsureDataDir: %v", err)
		}
	})

	t.Run("is idempotent", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		for i := range 3 {
			if err := app.EnsureDataDir(dir); err != nil {
				t.Fatalf("call %d: %v", i, err)
			}
		}
	})

	t.Run("leaves no probe file behind", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		if err := app.EnsureDataDir(dir); err != nil {
			t.Fatalf("EnsureDataDir: %v", err)
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 0 {
			t.Errorf("directory should be empty, found %d entries", len(entries))
		}
	})

	t.Run("overwrites a stale probe file", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		stale := filepath.Join(dir, ".stratus-write-probe")
		if err := os.WriteFile(stale, []byte("left over from a crash"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := app.EnsureDataDir(dir); err != nil {
			t.Fatalf("a stale probe should not be fatal: %v", err)
		}
		if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
			t.Error("stale probe should have been removed")
		}
	})

	t.Run("path is a file", func(t *testing.T) {
		t.Parallel()
		file := filepath.Join(t.TempDir(), "notadir")
		if err := os.WriteFile(file, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := app.EnsureDataDir(file); err == nil {
			t.Error("a regular file should not be accepted as the data dir")
		}
	})

	// The regression test for the bug that shipped: a data dir the process
	// cannot write to must fail at startup, not on the first upload.
	t.Run("unwritable directory names the uid", func(t *testing.T) {
		t.Parallel()
		if os.Geteuid() == 0 {
			t.Skip("root ignores mode bits, so this cannot fail as root")
		}
		dir := filepath.Join(t.TempDir(), "readonly")
		if err := os.Mkdir(dir, 0o500); err != nil {
			t.Fatal(err)
		}
		err := app.EnsureDataDir(dir)
		if err == nil {
			t.Fatal("an unwritable data dir must be an error")
		}
		if !strings.Contains(err.Error(), "not writable") {
			t.Errorf("error should say it is not writable, got %v", err)
		}
		if !strings.Contains(err.Error(), "uid") {
			t.Errorf("error should name the uid so the operator can fix it, got %v", err)
		}
	})
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

func TestRunRejectsHalfSetCredentials(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		vars map[string]string
		want string
	}{
		{
			name: "a hash with no username",
			vars: map[string]string{"STRATUS_PASSWORD": "an example password"},
			want: "STRATUS_USERNAME",
		},
		{
			name: "a username with no hash",
			vars: map[string]string{"STRATUS_USERNAME": "edu"},
			want: "STRATUS_PASSWORD",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := runToShutdown(t, runConfig(t, tt.vars))
			if err == nil {
				t.Fatal("Run = nil, want a refusal")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error should name %s, got %v", tt.want, err)
			}
		})
	}
}

func TestRunAcceptsWholeCredentials(t *testing.T) {
	t.Parallel()
	if err := runToShutdown(t, runConfig(t, map[string]string{
		"STRATUS_USERNAME": "edu",
		"STRATUS_PASSWORD": "an example password",
	})); err != nil {
		t.Errorf("Run = %v, want a clean start and shutdown", err)
	}
}

// TestRunProbesTheBlobStore checks the probe both ran and cleaned up after
// itself: an install that starts with a stray object in the store would be
// reporting one on every listing forever.
func TestRunProbesTheBlobStore(t *testing.T) {
	t.Parallel()
	dataDir := filepath.Join(t.TempDir(), "data")
	cfg := runConfig(t, map[string]string{"STRATUS_DATA_DIR": dataDir})

	if err := runToShutdown(t, cfg); err != nil {
		t.Fatalf("Run = %v, want a clean start", err)
	}

	entries, err := os.ReadDir(cfg.Storage.Dir)
	if err != nil {
		t.Fatalf("the blob directory was never created: %v", err)
	}
	for _, e := range entries {
		if e.Name() != ".tmp" {
			t.Errorf("the probe left %q behind", e.Name())
		}
	}
}

func TestRunRejectsAnUnreachableS3(t *testing.T) {
	t.Parallel()
	// Port 1 refuses immediately, so this is the "credentials or endpoint are
	// wrong" path without waiting on a timeout.
	cfg := runConfig(t, map[string]string{
		"STRATUS_STORAGE_DSN": "s3://key:secret@127.0.0.1:1/bucket?tls=false",
	})
	if err := runToShutdown(t, cfg); err == nil {
		t.Fatal("Run = nil, want a refusal: the server must not come up without its storage")
	}
}

// TestRunRejectsAnUnknownScheme covers a Config built by hand rather than
// parsed, which is the only way an empty scheme can reach the composition root.
func TestRunRejectsAnUnknownScheme(t *testing.T) {
	t.Parallel()
	cfg := runConfig(t, map[string]string{})
	cfg.Storage = config.StorageDSN{Scheme: "ftp"}

	err := runToShutdown(t, cfg)
	if err == nil || !strings.Contains(err.Error(), "ftp") {
		t.Errorf("Run = %v, want it to name the unsupported scheme", err)
	}
}

// TestRunRejectsAnUnwritableBlobDir is the storage-seam counterpart of the data
// directory check: the two are separate paths once a DSN can point elsewhere.
func TestRunRejectsAnUnwritableBlobDir(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("root ignores mode bits, so this cannot fail as root")
	}
	parent := filepath.Join(t.TempDir(), "readonly")
	if err := os.Mkdir(parent, 0o500); err != nil {
		t.Fatal(err)
	}
	cfg := runConfig(t, map[string]string{"STRATUS_STORAGE_DSN": "file://" + filepath.Join(parent, "blobs")})

	if err := runToShutdown(t, cfg); err == nil {
		t.Fatal("Run = nil, want a refusal on a blob directory it cannot create")
	}
}

func TestRunRejectsAnUnreachableDatabase(t *testing.T) {
	t.Parallel()
	cfg := runConfig(t, map[string]string{
		"STRATUS_DB_DSN": "postgres://nobody:secret@127.0.0.1:1/stratus?sslmode=disable",
	})
	err := runToShutdown(t, cfg)
	if err == nil {
		t.Fatal("Run = nil, want a refusal: the server must not come up without its database")
	}
	if strings.Contains(err.Error(), "secret") {
		t.Errorf("the error leaks the password: %v", err)
	}
}

// TestRunRejectsAnUnknownDatabaseScheme covers a Config built by hand, the only
// way an empty scheme reaches the composition root.
func TestRunRejectsAnUnknownDatabaseScheme(t *testing.T) {
	t.Parallel()
	cfg := runConfig(t, map[string]string{})
	cfg.Database = config.DatabaseDSN{Scheme: "mysql"}

	err := runToShutdown(t, cfg)
	if err == nil || !strings.Contains(err.Error(), "mysql") {
		t.Errorf("Run = %v, want it to name the unsupported scheme", err)
	}
}

// TestRunMigratesTheDatabase is the startup contract: by the time the server
// serves a request, the schema is there.
func TestRunMigratesTheDatabase(t *testing.T) {
	t.Parallel()
	dataDir := filepath.Join(t.TempDir(), "data")
	cfg := runConfig(t, map[string]string{"STRATUS_DATA_DIR": dataDir})

	if err := runToShutdown(t, cfg); err != nil {
		t.Fatalf("Run = %v, want a clean start", err)
	}

	store, err := sqlite.New(t.Context(), cfg.Database.Path)
	if err != nil {
		t.Fatalf("the database file was never created: %v", err)
	}
	defer func() { _ = store.Close() }()

	// A working query is the proof the migration ran; an empty listing is what
	// a fresh install should answer.
	files, err := store.ListFiles(t.Context(), "owner", "")
	if err != nil {
		t.Fatalf("the schema is not usable after startup: %v", err)
	}
	if len(files) != 0 {
		t.Errorf("a fresh database holds %d files, want none", len(files))
	}
}

// TestRunRestartsOnAnExistingDatabase is the ordinary case that would be easy
// to break: the second start must find its schema already applied and carry on.
func TestRunRestartsOnAnExistingDatabase(t *testing.T) {
	t.Parallel()
	cfg := runConfig(t, map[string]string{})

	for i := range 2 {
		if err := runToShutdown(t, cfg); err != nil {
			t.Fatalf("start %d: %v", i+1, err)
		}
	}
}

// TestRunRejectsAnUnusableDatabasePath covers the other half of openDatabase:
// a path SQLite cannot open at all.
func TestRunRejectsAnUnusableDatabasePath(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "not-a-file.db")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	cfg := runConfig(t, map[string]string{"STRATUS_DB_DSN": "sqlite://" + dir})

	if err := runToShutdown(t, cfg); err == nil {
		t.Fatal("Run = nil, want a refusal: a directory is not a database")
	}
}

// TestRunAgainstPostgres wires the whole chain against a real server:
// STRATUS_DB_DSN, the composition root, the driver and the migration. It skips
// without one, and `make cover` runs it with one, which is how the coverage
// floor notices if this stops running.
func TestRunAgainstPostgres(t *testing.T) {
	t.Parallel()
	dsn := os.Getenv("STRATUS_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("STRATUS_TEST_POSTGRES_DSN is not set; `make test-db` or `make cover` set it")
	}

	if err := runToShutdown(t, runConfig(t, map[string]string{"STRATUS_DB_DSN": dsn})); err != nil {
		t.Errorf("Run against PostgreSQL = %v, want a clean start and shutdown", err)
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

// TestRunRejectsAReadOnlyDatabase covers the other startup failure the database
// can have: it opens, and then the schema cannot be written. A restore that got
// the file ownership wrong looks exactly like this.
func TestRunRejectsAReadOnlyDatabase(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("root ignores mode bits, so this cannot fail as root")
	}
	path := filepath.Join(t.TempDir(), "stratus.db")
	if err := os.WriteFile(path, nil, 0o400); err != nil {
		t.Fatal(err)
	}
	cfg := runConfig(t, map[string]string{"STRATUS_DB_DSN": "sqlite://" + path})

	err := runToShutdown(t, cfg)
	if err == nil {
		t.Fatal("Run = nil, want a refusal on a database it cannot write to")
	}
	// It fails at open rather than at migrate, because the pragmas the driver
	// sets are themselves writes. Fine, and worth pinning: the point is that it
	// stops now and says why, rather than on the first upload.
	if !strings.Contains(err.Error(), "readonly") {
		t.Errorf("the error should name the read-only database, got %v", err)
	}
}

// davServer starts the whole application over real backends and returns its
// base URL, which is the only way to test that the surface is actually wired:
// authentication, prefix, storage and database all at once.
func davServer(t *testing.T, vars map[string]string) (string, func()) {
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
	base, stop := davServer(t, map[string]string{
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
	base, stop := davServer(t, map[string]string{})
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
