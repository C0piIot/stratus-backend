package app_test

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/C0piIot/stratus-backend/internal/app"
	"github.com/C0piIot/stratus-backend/internal/db/sqlite"
	"github.com/C0piIot/stratus-backend/internal/storage/disk"
)

// deps builds real backends, the way every other adapter test here does. close
// shuts one of them down, which is how a dependency outage is produced without
// a stub: a closed store answers, and what it answers is not "not found".
func deps(t *testing.T) (app.Deps, func(which string)) {
	t.Helper()
	dir := t.TempDir()

	blobs, err := disk.New(filepath.Join(dir, "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	meta, err := sqlite.New(t.Context(), filepath.Join(dir, "stratus.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := meta.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}

	closed := map[string]bool{}
	t.Cleanup(func() {
		if !closed["storage"] {
			_ = blobs.Close()
		}
		if !closed["database"] {
			_ = meta.Close()
		}
	})

	return app.Deps{Storage: blobs, Database: meta}, func(which string) {
		t.Helper()
		closed[which] = true
		switch which {
		case "storage":
			if err := blobs.Close(); err != nil {
				t.Fatal(err)
			}
		case "database":
			if err := meta.Close(); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func get(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil))
	return rec
}

func TestReadyzReportsBothDependencies(t *testing.T) {
	t.Parallel()
	d, _ := deps(t)

	rec := get(t, app.New(testConfig(t), "test").Handler(d), "/readyz")
	if rec.Code != http.StatusOK {
		t.Errorf("GET /readyz = %d, want 200", rec.Code)
	}
	if want := "database: ok\nstorage: ok\n"; rec.Body.String() != want {
		t.Errorf("body = %q, want %q", rec.Body.String(), want)
	}
}

// TestReadyzWithNothingConfigured pins the state an install starts in. It is
// not a fault, so it is not a 503: nobody should go hunting for a broken
// database because they have not set a password yet.
func TestReadyzWithNothingConfigured(t *testing.T) {
	t.Parallel()

	rec := get(t, app.New(testConfig(t), "test").Handler(app.Deps{}), "/readyz")
	if rec.Code != http.StatusOK {
		t.Errorf("GET /readyz = %d, want 200", rec.Code)
	}
	if want := "database: not configured\nstorage: not configured\n"; rec.Body.String() != want {
		t.Errorf("body = %q, want %q", rec.Body.String(), want)
	}
}

func TestReadyzWhenADependencyIsGone(t *testing.T) {
	t.Parallel()
	tests := []struct {
		which string
		want  string
	}{
		{which: "database", want: "database: unreachable\nstorage: ok\n"},
		{which: "storage", want: "database: ok\nstorage: unreachable\n"},
	}
	for _, tt := range tests {
		t.Run(tt.which, func(t *testing.T) {
			t.Parallel()
			d, shutdown := deps(t)
			shutdown(tt.which)

			rec := get(t, app.New(testConfig(t), "test").Handler(d), "/readyz")
			if rec.Code != http.StatusServiceUnavailable {
				t.Errorf("GET /readyz with no %s = %d, want 503", tt.which, rec.Code)
			}
			if rec.Body.String() != tt.want {
				t.Errorf("body = %q, want %q", rec.Body.String(), tt.want)
			}
		})
	}
}

// TestHealthzIgnoresDependencies is the whole point of there being two
// endpoints. Restarting the container does not fix a database on another host,
// so liveness must not fail when one goes away -- with restart: unless-stopped
// that would be an endless restart loop over something a restart cannot mend.
func TestHealthzIgnoresDependencies(t *testing.T) {
	t.Parallel()
	d, shutdown := deps(t)
	shutdown("database")
	shutdown("storage")

	rec := get(t, app.New(testConfig(t), "test").Handler(d), "/healthz")
	if rec.Code != http.StatusOK {
		t.Errorf("GET /healthz with both dependencies gone = %d, want 200", rec.Code)
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
