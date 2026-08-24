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
	"github.com/C0piIot/stratus-backend/internal/config"
)

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
			app.New(config.Load(nil), "test").Handler().ServeHTTP(rec, req)

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
	cfg := config.Config{Addr: "127.0.0.1:0", DataDir: filepath.Join(t.TempDir(), "data")}
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
	err := app.New(config.Config{Addr: "127.0.0.1:0", DataDir: dir}, "test").Run(t.Context())
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
		srv := httptest.NewServer(app.New(config.Load(nil), "test").Handler())
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
		srv := httptest.NewServer(app.New(config.Load(nil), "test").Handler())
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
