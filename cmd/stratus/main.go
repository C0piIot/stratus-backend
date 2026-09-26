// Command stratus runs the Stratus server: a single-binary personal cloud
// speaking WebDAV, CalDAV and OpenSubsonic over pluggable storage and metadata
// backends.
//
// This file is deliberately thin: flags, wiring and exit codes. Everything
// worth testing lives in internal/app and internal/config.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/C0piIot/stratus-backend/internal/app"
	"github.com/C0piIot/stratus-backend/internal/config"
	"github.com/C0piIot/stratus-backend/internal/report"
)

// version and buildDate are overridden at build time with
// -ldflags "-X main.version=... -X main.buildDate=...".
//
// The timestamp is the commit's, not the clock's, so two builds of the same
// source are still the same bytes -- and because what somebody reading it wants
// to know is how old the code is, not when a runner happened to compile it. UTC
// and to the second: a footer that says only the day cannot tell two builds of
// the same afternoon apart, which is exactly when somebody is asking.
var (
	version   = "dev"
	buildDate = "unknown"
)

func main() {
	// Distroless images ship no shell and no curl, so the binary probes itself
	// for the container healthcheck.
	healthcheck := flag.Bool("healthcheck", false, "probe the local health endpoint and exit")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Printf("stratus %s (built %s)\n", version, buildDate)
		return
	}

	cfg, err := config.Load(os.Getenv)
	if err != nil {
		fmt.Fprintln(os.Stderr, "configuration:", err)
		os.Exit(1)
	}

	switch {
	case *healthcheck:
		if err := app.Probe(cfg.Addr); err != nil {
			fmt.Fprintln(os.Stderr, "healthcheck:", err)
			os.Exit(1)
		}
	default:
		var handler slog.Handler = slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel})
		flush := func() {}
		if cfg.SentryDSN != "" {
			client, err := report.NewClient(cfg.SentryDSN.Reveal(), version)
			if err != nil {
				fmt.Fprintln(os.Stderr, "error reporting:", err)
				os.Exit(1)
			}
			handler = report.New(handler, client)
			// Events go out in the background, so the last ones -- the error
			// that is stopping the process, above all -- need waiting for.
			flush = func() { client.Flush(5 * time.Second) }
		}
		slog.SetDefault(slog.New(handler))

		// Signals are a process concern, so they are handled here rather than
		// inside app.Run.
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()

		if err := app.New(cfg, version, buildDate).Run(ctx); err != nil {
			slog.Error("server stopped", "err", err)
			flush()
			os.Exit(1)
		}
		flush()
	}
}
