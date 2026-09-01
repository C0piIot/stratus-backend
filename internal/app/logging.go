package app

import (
	"log/slog"
	"net/http"
	"time"
)

// logRequests wraps h so that every request leaves exactly one line.
//
// The startup path is well instrumented and then the server used to go silent
// for the rest of its life, which is no help at all when the report is "my sync
// client says it failed": the 401 or the 409 that caused it was invisible.
//
// One middleware around the whole mux rather than one per adapter, so the
// surfaces that do not exist yet are covered the day they are mounted.
func logRequests(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		rec := &recorder{ResponseWriter: w, status: http.StatusOK}

		h.ServeHTTP(rec, r)

		// The container healthcheck asks every thirty seconds, which at info
		// level is three thousand lines a day of nothing happening.
		level := slog.LevelInfo
		switch {
		case r.URL.Path == "/healthz":
			level = slog.LevelDebug
		case rec.status >= http.StatusInternalServerError:
			level = slog.LevelError
		}

		// No headers and no query string: one carries the credentials and the
		// other is where a token would end up if a protocol ever put one there.
		slog.Log(r.Context(), level, "request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"bytes", rec.written,
			"duration", time.Since(started),
			"remote", r.RemoteAddr)
	})
}

// recorder remembers what the handler answered.
type recorder struct {
	http.ResponseWriter
	status  int
	written int64
}

func (r *recorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func (r *recorder) Write(b []byte) (int, error) {
	n, err := r.ResponseWriter.Write(b)
	r.written += int64(n)
	return n, err
}

// Unwrap keeps http.ResponseController working through the wrapper, which is
// what a streaming response needs to flush and what a future upgrade would need
// to hijack.
func (r *recorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }
