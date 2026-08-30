package app_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/C0piIot/stratus-backend/internal/app"
	"github.com/C0piIot/stratus-backend/internal/auth"
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
	vars["STRATUS_ADDR"] = "127.0.0.1:0"

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
			app.New(testConfig(t), "test").Handler().ServeHTTP(rec, req)

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
	srv := app.New(config.Config{Addr: ":8080"}, "test").Server()

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
	cfg := runConfig(t, map[string]string{})
	ctx, cancel := context.WithCancel(t.Context())

	done := make(chan error, 1)
	go func() { done <- app.New(cfg, "test").Run(ctx) }()

	// Give the listener a moment to bind, then ask it to stop.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run returned %v, want nil on graceful shutdown", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after the context was cancelled")
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
		srv := httptest.NewServer(app.New(testConfig(t), "test").Handler())
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
		srv := httptest.NewServer(app.New(testConfig(t), "test").Handler())
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

// runToShutdown starts Run, waits for it to bind, and stops it. It returns
// whatever Run returned, so both the happy path and the refusals go through the
// same helper.
func runToShutdown(t *testing.T, cfg config.Config) error {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- app.New(cfg, "test").Run(ctx) }()

	select {
	case err := <-done:
		cancel()
		return err // refused before it ever listened
	case <-time.After(200 * time.Millisecond):
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
			vars: map[string]string{"STRATUS_PASSWORD_HASH": "$2a$10$N9qo8uLOickgx2ZMRZoMyeIjZAgcfl7p92ldGxad68LJZdL17lhWy"},
			want: "STRATUS_USERNAME",
		},
		{
			name: "a username with no hash",
			vars: map[string]string{"STRATUS_USERNAME": "edu"},
			want: "STRATUS_PASSWORD_HASH",
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

// TestRunRejectsAHashItCouldNeverVerify is the fail-fast case that matters
// most: a hash pasted with a byte missing would otherwise look fine until the
// first login.
func TestRunRejectsAHashItCouldNeverVerify(t *testing.T) {
	t.Parallel()
	err := runToShutdown(t, runConfig(t, map[string]string{
		"STRATUS_USERNAME":      "edu",
		"STRATUS_PASSWORD_HASH": "hunter2",
	}))
	if err == nil {
		t.Fatal("Run = nil, want a refusal")
	}
	if !strings.Contains(err.Error(), "hash-password") {
		t.Errorf("the error should say how to produce a hash, got %v", err)
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Errorf("the error echoes the configured value: %v", err)
	}
}

func TestRunAcceptsWholeCredentials(t *testing.T) {
	t.Parallel()
	hash, err := auth.Hash("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if err := runToShutdown(t, runConfig(t, map[string]string{
		"STRATUS_USERNAME":      "edu",
		"STRATUS_PASSWORD_HASH": hash,
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
