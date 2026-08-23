// Command stratus runs the Stratus server: a single-binary personal cloud
// speaking WebDAV, CalDAV and OpenSubsonic over pluggable storage and metadata
// backends.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// version is overridden at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	// Distroless images ship no shell and no curl, so the binary probes itself
	// for the container healthcheck.
	healthcheck := flag.Bool("healthcheck", false, "probe the local health endpoint and exit")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	switch {
	case *showVersion:
		fmt.Println("stratus", version)
	case *healthcheck:
		if err := probe(addr()); err != nil {
			fmt.Fprintln(os.Stderr, "healthcheck:", err)
			os.Exit(1)
		}
	default:
		if err := run(); err != nil {
			slog.Error("server stopped", "err", err)
			os.Exit(1)
		}
	}
}

func run() error {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: logLevel()})))

	// Fail fast and loudly: a data dir the container cannot write to is the most
	// likely misconfiguration, and finding out on the first upload is too late.
	if err := ensureDataDir(dataDir()); err != nil {
		return err
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintln(w, "ok")
	})

	srv := &http.Server{
		Addr:              addr(),
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		// No WriteTimeout on purpose: media streaming responses are long-lived
		// and a blanket write deadline would cut them off mid-file.
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errc := make(chan error, 1)
	go func() {
		slog.Info("stratus listening", "version", version, "addr", srv.Addr, "data_dir", dataDir())
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
	}()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		slog.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
}

// ensureDataDir creates the data directory if needed and verifies the process
// can actually write to it.
func ensureDataDir(dir string) error {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("create data dir %s: %w", dir, err)
	}
	probe := filepath.Join(dir, ".stratus-write-probe")
	f, err := os.OpenFile(probe, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
	if err != nil {
		return fmt.Errorf("data dir %s is not writable as uid %d/gid %d: %w", dir, os.Getuid(), os.Getgid(), err)
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Remove(probe)
}

// probe performs the container healthcheck against a locally bound listener.
func probe(listenAddr string) error {
	host, port, err := net.SplitHostPort(listenAddr)
	if err != nil {
		return err
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	url := "http://" + net.JoinHostPort(host, port) + "/healthz"

	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s returned %s", url, resp.Status)
	}
	return nil
}

func addr() string    { return env("STRATUS_ADDR", ":8080") }
func dataDir() string { return env("STRATUS_DATA_DIR", "/data") }

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func logLevel() slog.Level {
	var l slog.Level
	if err := l.UnmarshalText([]byte(strings.ToUpper(env("STRATUS_LOG_LEVEL", "INFO")))); err != nil {
		return slog.LevelInfo
	}
	return l
}
