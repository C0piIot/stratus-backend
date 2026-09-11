package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/C0piIot/stratus-backend/internal/storage"
)

// readyTimeout bounds the dependency checks. A database that has gone away
// often does not refuse a connection, it stops answering, and a readiness
// endpoint that hangs is worse than one that says no.
const readyTimeout = 2 * time.Second

// readiness reports whether the dependencies answer. It is deliberately not
// what the container healthcheck asks.
//
// /healthz is liveness: the process is up and serving. Restarting the container
// does not fix a database on another host, so a healthcheck that failed on a
// dependency outage would turn one broken dependency into a restart loop --
// with `restart: unless-stopped`, an endless one.
//
// This endpoint answers the other question, the one an operator actually has:
// is my database reachable, does my bucket answer. It drives nothing. Nothing
// restarts because of it.
//
// It is unauthenticated, like /healthz. What it discloses is whether two
// backends respond, never a name, a DSN or a byte of content -- and an install
// with no credentials has no file surface to protect anyway.
func readiness(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), readyTimeout)
		defer cancel()

		database := checkDatabase(ctx, deps)
		blobs := checkStorage(ctx, deps)

		status := http.StatusOK
		if !database.ok || !blobs.ok {
			status = http.StatusServiceUnavailable
		}

		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(status)
		// Nothing useful to do if the client hung up mid-write.
		_, _ = io.WriteString(w, "database: "+database.state+"\nstorage: "+blobs.state+"\n")
	}
}

// check is one dependency's answer. The reason a failure happened is logged and
// not served: a driver error can carry the host it could not reach, and a DSN
// is never printed verbatim anywhere else either.
type check struct {
	ok    bool
	state string
}

var (
	ready         = check{ok: true, state: "ok"}
	notConfigured = check{ok: true, state: "not configured"}
	unreachable   = check{ok: false, state: "unreachable"}
)

// checkDatabase pings. A store that is not configured reports so and does not
// fail the check: an install without credentials mounts no file surface on
// purpose, and calling that unready would have an operator hunting a fault they
// did not create.
func checkDatabase(ctx context.Context, deps Deps) check {
	if deps.Database == nil {
		return notConfigured
	}
	if err := deps.Database.Ping(ctx); err != nil {
		slog.ErrorContext(ctx, "readiness check failed", "dependency", "database", "err", err)
		return unreachable
	}
	return ready
}

// checkStorage asks for one object it expects to be missing. That is the
// cheapest round trip that proves the backend answers -- a stat on disk, a HEAD
// on S3 -- and unlike the startup probe it writes nothing, which matters for an
// endpoint something polls.
//
// ErrNotFound is the success case. Anything else, including the AccessDenied an
// S3 backend returns for credentials that have stopped working, is a failure.
func checkStorage(ctx context.Context, deps Deps) check {
	if deps.Storage == nil {
		return notConfigured
	}
	switch _, err := deps.Storage.Stat(ctx, probeKey); {
	case err == nil, errors.Is(err, storage.ErrNotFound):
		return ready
	default:
		slog.ErrorContext(ctx, "readiness check failed", "dependency", "storage", "err", err)
		return unreachable
	}
}

// probeTimeout bounds the self-probe used by the container healthcheck. A
// healthcheck that can hang forever is worse than none: Docker would never mark
// the container unhealthy.
const probeTimeout = 3 * time.Second

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
