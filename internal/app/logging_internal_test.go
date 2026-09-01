package app

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// capture drives the middleware and returns the line it logged, or nothing when
// it logged below the handler's level.
func capture(t *testing.T, level slog.Level, target string, h http.Handler) map[string]any {
	t.Helper()
	var buf bytes.Buffer

	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: level})))
	t.Cleanup(func() { slog.SetDefault(previous) })

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, target, nil)
	req.RemoteAddr = "192.0.2.10:54321"
	logRequests(h).ServeHTTP(httptest.NewRecorder(), req)

	if buf.Len() == 0 {
		return nil
	}
	var line map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &line); err != nil {
		t.Fatalf("the log line is not JSON: %v\n%s", err, buf.String())
	}
	return line
}

func TestLogRequests(t *testing.T) {
	line := capture(t, slog.LevelInfo, "/dav/photo.jpg", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte("nope"))
	}))
	if line == nil {
		t.Fatal("nothing was logged")
	}

	if line["method"] != "GET" || line["path"] != "/dav/photo.jpg" {
		t.Errorf("got %v", line)
	}
	// The status is the whole point: a 409 nobody can see is a support ticket.
	if line["status"] != float64(http.StatusConflict) {
		t.Errorf("status = %v, want 409", line["status"])
	}
	if line["bytes"] != float64(4) {
		t.Errorf("bytes = %v, want 4", line["bytes"])
	}
	if line["remote"] != "192.0.2.10:54321" {
		t.Errorf("remote = %v", line["remote"])
	}
	if line["duration"] == nil {
		t.Error("no duration")
	}
}

// TestLogRequestsDefaultsToOK covers a handler that writes a body without ever
// calling WriteHeader, which is what http.ServeContent does on the happy path.
func TestLogRequestsDefaultsToOK(t *testing.T) {
	line := capture(t, slog.LevelInfo, "/dav/notes.txt", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("hello"))
	}))
	if line["status"] != float64(http.StatusOK) {
		t.Errorf("status = %v, want 200", line["status"])
	}
}

// TestHealthzIsQuiet: the container asks every thirty seconds, and at info
// level that is three thousand lines a day of nothing happening.
func TestHealthzIsQuiet(t *testing.T) {
	ok := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})

	if line := capture(t, slog.LevelInfo, "/healthz", ok); line != nil {
		t.Errorf("the healthcheck logged at info: %v", line)
	}
	if line := capture(t, slog.LevelDebug, "/healthz", ok); line == nil {
		t.Error("the healthcheck logged nothing even at debug, so it cannot be traced when it matters")
	}
}

func TestServerErrorsLogAsErrors(t *testing.T) {
	line := capture(t, slog.LevelError, "/dav/x", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	if line == nil {
		t.Fatal("a 500 was not logged at error level")
	}
	if line["level"] != "ERROR" {
		t.Errorf("level = %v, want ERROR", line["level"])
	}
}

// TestNoCredentialsInTheLog is the assertion that keeps this from becoming the
// leak it is meant to help debug.
func TestNoCredentialsInTheLog(t *testing.T) {
	var buf bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/dav/x?token=super-secret", nil)
	req.SetBasicAuth("edu", "an example password")
	logRequests(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).ServeHTTP(httptest.NewRecorder(), req)

	for _, secret := range []string{"an example password", "super-secret", "Authorization"} {
		if strings.Contains(buf.String(), secret) {
			t.Errorf("the log line contains %q: %s", secret, buf.String())
		}
	}
}
