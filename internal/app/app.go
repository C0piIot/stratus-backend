// Package app is the composition root: it wires configuration into an HTTP
// server and owns the process lifecycle. Nothing else imports it.
package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/C0piIot/stratus-backend/internal/config"
)

// probeTimeout bounds the self-probe used by the container healthcheck. A
// healthcheck that can hang forever is worse than none: Docker would never mark
// the container unhealthy.
const probeTimeout = 3 * time.Second

// shutdownTimeout bounds how long in-flight requests get to finish.
const shutdownTimeout = 15 * time.Second

// probeFile is written and removed at startup to prove the data dir is writable.
const probeFile = ".stratus-write-probe"

// App holds the wired application. Construction is pure: no I/O happens until
// Run, so Handler can be exercised from tests without touching the filesystem.
type App struct {
	cfg     config.Config
	version string
}

// New wires an App. It performs no I/O.
func New(cfg config.Config, version string) *App {
	return &App{cfg: cfg, version: version}
}

// Handler builds the HTTP routes. Separate from Run so every protocol surface
// can be tested through httptest without binding a port.
func (a *App) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		// Nothing useful to do if the client hung up mid-write.
		_, _ = io.WriteString(w, "ok\n")
	})
	return mux
}

// Server applies the timeout policy. Separate from Run so the policy itself can
// be asserted -- see TestServerTimeouts.
func (a *App) Server() *http.Server {
	return &http.Server{
		Addr:              a.cfg.Addr,
		Handler:           a.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		// WriteTimeout is deliberately left at zero: media streaming responses
		// are long-lived and a blanket write deadline would cut them off
		// mid-file. Slowloris is covered by ReadHeaderTimeout instead.
	}
}

// Run serves until ctx is cancelled, then drains in-flight requests. Signal
// handling belongs to the caller, which keeps Run testable with a plain
// cancellable context.
func (a *App) Run(ctx context.Context) error {
	// Fail fast and loudly: a data dir the process cannot write to is the most
	// likely misconfiguration, and finding out on the first upload is too late.
	if err := EnsureDataDir(a.cfg.DataDir); err != nil {
		return err
	}

	srv := a.Server()
	errc := make(chan error, 1)
	go func() {
		// uid/gid are logged so the container smoke tests can assert the
		// *runtime* user: distroless has no shell to run `id` in.
		slog.Info("stratus listening",
			"version", a.version, "addr", srv.Addr, "data_dir", a.cfg.DataDir,
			"uid", os.Getuid(), "gid", os.Getgid())
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
	}()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		slog.Info("shutting down")
		// A fresh context on purpose, not an oversight: ctx is already cancelled
		// -- that is why we are here -- so deriving from it would abort every
		// in-flight request immediately and defeat the graceful drain.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		return srv.Shutdown(shutdownCtx) //nolint:contextcheck // deliberately not the cancelled parent
	}
}

// EnsureDataDir creates the data directory if needed and verifies the process
// can actually write to it.
func EnsureDataDir(dir string) error {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("create data dir %s: %w", dir, err)
	}
	probe := filepath.Join(dir, probeFile)
	// The path is built from operator configuration, not from request input.
	// internal/storage, which will serve request-derived paths, must never
	// suppress G304 -- that is where it earns its keep.
	//nolint:gosec // operator-supplied config path, not user input
	f, err := os.OpenFile(probe, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("data dir %s is not writable as uid %d/gid %d: %w",
			dir, os.Getuid(), os.Getgid(), err)
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Remove(probe)
}

// HealthURL turns a listen address into a URL reachable from inside the same
// container. A wildcard bind address is not dialable, so it maps to loopback.
func HealthURL(listenAddr string) (string, error) {
	host, port, err := net.SplitHostPort(listenAddr)
	if err != nil {
		return "", err
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port) + "/healthz", nil
}

// Probe performs the container healthcheck against a locally bound listener.
func Probe(listenAddr string) error {
	url, err := HealthURL(listenAddr)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s returned %s", url, resp.Status)
	}
	return nil
}
