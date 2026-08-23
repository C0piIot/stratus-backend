package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Tests touching the environment cannot call t.Parallel: t.Setenv panics in a
// parallel test. Everything else runs parallel.

// unsetEnv removes a variable for the duration of the test and restores it
// afterwards. There is no t.Unsetenv, and a bare os.Unsetenv would leak into
// whatever test runs next -- which -shuffle=on makes unpredictable.
//
// t.Setenv already registers restoration of the prior state, including "was
// not set at all", so unsetting right after it borrows that cleanup instead of
// hand-rolling one. Doing it by hand would need os.Setenv in a Cleanup func,
// where t.Setenv cannot legally be called.
func unsetEnv(t *testing.T, key string) {
	t.Helper()
	t.Setenv(key, "")
	if err := os.Unsetenv(key); err != nil {
		t.Fatalf("unset %s: %v", key, err)
	}
}

func TestEnv(t *testing.T) {
	tests := []struct {
		name  string
		set   bool
		value string
		want  string
	}{
		{name: "unset falls back", set: false, want: "fallback"},
		{name: "set is used", set: true, value: "actual", want: "actual"},
		// Pinning current behaviour: an empty value counts as unset. Callers
		// rely on this, so a change here should break a test on purpose.
		{name: "empty counts as unset", set: true, value: "", want: "fallback"},
		{name: "whitespace is preserved", set: true, value: " ", want: " "},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			const key = "STRATUS_TEST_ENV"
			unsetEnv(t, key)
			if tt.set {
				t.Setenv(key, tt.value)
			}
			if got := env(key, "fallback"); got != tt.want {
				t.Errorf("env() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestLogLevel(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  string
	}{
		{name: "default", value: "", want: "INFO"},
		{name: "debug", value: "debug", want: "DEBUG"},
		{name: "uppercase", value: "WARN", want: "WARN"},
		{name: "mixed case", value: "ErRoR", want: "ERROR"},
		{name: "garbage falls back to info", value: "nonsense", want: "INFO"},
		{name: "numeric is rejected", value: "3", want: "INFO"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.value != "" {
				t.Setenv("STRATUS_LOG_LEVEL", tt.value)
			} else {
				unsetEnv(t, "STRATUS_LOG_LEVEL")
			}
			if got := logLevel().String(); got != tt.want {
				t.Errorf("logLevel() = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestAddrAndDataDir(t *testing.T) {
	t.Run("defaults", func(t *testing.T) {
		unsetEnv(t, "STRATUS_ADDR")
		unsetEnv(t, "STRATUS_DATA_DIR")
		if got := addr(); got != ":8080" {
			t.Errorf("addr() = %q, want :8080", got)
		}
		if got := dataDir(); got != "/data" {
			t.Errorf("dataDir() = %q, want /data", got)
		}
	})
	t.Run("overridden", func(t *testing.T) {
		t.Setenv("STRATUS_ADDR", "127.0.0.1:9999")
		t.Setenv("STRATUS_DATA_DIR", "/srv/stratus")
		if got := addr(); got != "127.0.0.1:9999" {
			t.Errorf("addr() = %q", got)
		}
		if got := dataDir(); got != "/srv/stratus" {
			t.Errorf("dataDir() = %q", got)
		}
	})
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
			got, err := healthURL(tt.addr)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("healthURL(%q) = %q, want error", tt.addr, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("healthURL(%q): %v", tt.addr, err)
			}
			if got != tt.want {
				t.Errorf("healthURL(%q) = %q, want %q", tt.addr, got, tt.want)
			}
		})
	}
}

func TestHealthzHandler(t *testing.T) {
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
			newMux().ServeHTTP(rec, req)

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

// TestNewServerTimeouts pins the timeout policy. WriteTimeout must stay zero:
// media streaming responses are long-lived and a write deadline would truncate
// them mid-file. If someone "hardens" this by adding one, this test fails and
// explains why.
func TestNewServerTimeouts(t *testing.T) {
	t.Parallel()
	srv := newServer(":8080", newMux())

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

func TestProbe(t *testing.T) {
	t.Parallel()

	t.Run("healthy server", func(t *testing.T) {
		t.Parallel()
		srv := httptest.NewServer(newMux())
		defer srv.Close()
		if err := probe(strings.TrimPrefix(srv.URL, "http://")); err != nil {
			t.Errorf("probe: %v", err)
		}
	})

	t.Run("unhealthy status is an error", func(t *testing.T) {
		t.Parallel()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer srv.Close()
		err := probe(strings.TrimPrefix(srv.URL, "http://"))
		if err == nil {
			t.Fatal("probe succeeded against a 500")
		}
		if !strings.Contains(err.Error(), "500") {
			t.Errorf("error should mention the status, got %v", err)
		}
	})

	t.Run("nothing listening is an error", func(t *testing.T) {
		t.Parallel()
		// Bind then immediately close to get a port nothing is listening on.
		srv := httptest.NewServer(newMux())
		hostPort := strings.TrimPrefix(srv.URL, "http://")
		srv.Close()
		if err := probe(hostPort); err == nil {
			t.Error("probe succeeded with nothing listening")
		}
	})

	t.Run("bad address is an error", func(t *testing.T) {
		t.Parallel()
		if err := probe("not an address"); err == nil {
			t.Error("probe succeeded on a malformed address")
		}
	})
}

func TestEnsureDataDir(t *testing.T) {
	t.Parallel()

	t.Run("creates a missing directory", func(t *testing.T) {
		t.Parallel()
		dir := filepath.Join(t.TempDir(), "data")
		if err := ensureDataDir(dir); err != nil {
			t.Fatalf("ensureDataDir: %v", err)
		}
		if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
			t.Fatalf("directory not created: %v", err)
		}
	})

	t.Run("creates missing parents", func(t *testing.T) {
		t.Parallel()
		dir := filepath.Join(t.TempDir(), "a", "b", "c")
		if err := ensureDataDir(dir); err != nil {
			t.Fatalf("ensureDataDir: %v", err)
		}
	})

	t.Run("is idempotent", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		for i := range 3 {
			if err := ensureDataDir(dir); err != nil {
				t.Fatalf("call %d: %v", i, err)
			}
		}
	})

	t.Run("leaves no probe file behind", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		if err := ensureDataDir(dir); err != nil {
			t.Fatalf("ensureDataDir: %v", err)
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
		if err := ensureDataDir(dir); err != nil {
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
		if err := ensureDataDir(file); err == nil {
			t.Error("a regular file should not be accepted as the data dir")
		}
	})

	// The regression test for the bug that shipped: a data dir the process
	// cannot write to must fail at startup, not on the first upload.
	t.Run("unwritable directory fails with uid in the message", func(t *testing.T) {
		t.Parallel()
		if os.Geteuid() == 0 {
			t.Skip("root ignores mode bits, so this cannot fail as root")
		}
		dir := filepath.Join(t.TempDir(), "readonly")
		if err := os.Mkdir(dir, 0o500); err != nil {
			t.Fatal(err)
		}
		err := ensureDataDir(dir)
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

func TestProbeTimeoutIsBounded(t *testing.T) {
	t.Parallel()
	// A healthcheck that can hang forever is worse than no healthcheck: Docker
	// would never mark the container unhealthy.
	if probeTimeout <= 0 || probeTimeout > 10*time.Second {
		t.Errorf("probeTimeout = %v, want a small positive bound", probeTimeout)
	}
}
