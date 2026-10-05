// Command stratus runs the Stratus server: a single-binary personal cloud
// speaking WebDAV, CalDAV and OpenSubsonic over pluggable storage and metadata
// backends.
//
// This file is deliberately thin: flags, wiring and exit codes. Everything
// worth testing lives in internal/app and internal/config.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/C0piIot/stratus-backend/internal/app"
	"github.com/C0piIot/stratus-backend/internal/config"
	"github.com/C0piIot/stratus-backend/internal/nextcloud"
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
	// A subcommand is taken before the flag package sees anything: `import`
	// has flags of its own, and the default FlagSet would refuse them as the
	// server's. The server itself stays the bare `stratus`, so nothing that
	// runs this image today has to change.
	if len(os.Args) > 1 && os.Args[1] == "import" {
		if err := runImport(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "import:", err)
			os.Exit(1)
		}
		return
	}

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

// runImport is `stratus import nextcloud`: the two halves of #24.
//
// The survey is what it does by default, and that is the dry run being the
// default rather than a flag somebody has to remember. The thing worth knowing
// first is whether the database and the bucket still agree -- Nextcloud's own
// well-known failure is that they drift -- and finding that out after the rows
// are written is a data loss event rather than a migration. `--write` is the
// only thing here that writes, and it runs the survey again on its way.
func runImport(args []string) error {
	if len(args) == 0 || args[0] != "nextcloud" {
		return errors.New("usage: stratus import nextcloud --db <path or DSN> [--write]")
	}

	fs := flag.NewFlagSet("stratus import nextcloud", flag.ExitOnError)
	var opts nextcloud.ImportOptions
	var write bool
	fs.StringVar(&opts.Source, "db", "",
		"Nextcloud's database, read-only: a path to its SQLite file, or a postgres:// or mysql:// DSN")
	fs.StringVar(&opts.User, "user", "", "whose files, when the instance has more than one user")
	fs.StringVar(&opts.TablePrefix, "table-prefix", nextcloud.DefaultTablePrefix, "Nextcloud's dbtableprefix")
	fs.StringVar(&opts.ObjectPrefix, "object-prefix", nextcloud.DefaultObjectPrefix,
		"objectstore.arguments.objectPrefix from config.php, which is not in the database")
	fs.IntVar(&opts.Workers, "workers", nextcloud.DefaultWorkers, "how many objects to ask the bucket about at once")
	fs.BoolVar(&write, "write", false, "write the rows; without it this is a survey and touches nothing")
	fs.StringVar(&opts.Into, "into", "", "adopt the library under this path instead of at the root of the tree")
	fs.BoolVar(&opts.ETag, "etag", false,
		"read every object to compute its ETag, which Nextcloud has no hash to give")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if opts.Source == "" {
		return errors.New("--db is required: Nextcloud's database, as a path or a DSN")
	}
	if !write && (opts.Into != "" || opts.ETag) {
		return errors.New("--into and --etag only mean something with --write")
	}

	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return fmt.Errorf("configuration: %w", err)
	}

	// The blob store is this server's, from the same STRATUS_STORAGE_DSN the
	// server runs on: the question being asked is whether that bucket already
	// holds what Nextcloud's database claims.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if write {
		return app.ImportNextcloud(ctx, cfg, opts, os.Stdout)
	}
	return app.SurveyNextcloud(ctx, cfg, opts.Options, os.Stdout)
}
