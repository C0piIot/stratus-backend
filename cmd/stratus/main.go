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

	"github.com/C0piIot/stratus-backend/internal/app"
	"github.com/C0piIot/stratus-backend/internal/config"
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
		slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel})))

		// Signals are a process concern, so they are handled here rather than
		// inside app.Run.
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()

		if err := app.New(cfg, version, buildDate).Run(ctx); err != nil {
			slog.Error("server stopped", "err", err)
			os.Exit(1)
		}
	}
}
