// Command stratus runs the Stratus server: a single-binary personal cloud
// speaking WebDAV, CalDAV and OpenSubsonic over pluggable storage and metadata
// backends.
//
// This file is deliberately thin: flags, wiring and exit codes. Everything
// worth testing lives in internal/app and internal/config.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/C0piIot/stratus-backend/internal/app"
	"github.com/C0piIot/stratus-backend/internal/auth"
	"github.com/C0piIot/stratus-backend/internal/config"
)

// version is overridden at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	// Distroless images ship no shell and no curl, so the binary probes itself
	// for the container healthcheck.
	healthcheck := flag.Bool("healthcheck", false, "probe the local health endpoint and exit")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	// A subcommand rather than a flag: producing the password hash is a
	// different job from running the server, and the setup instructions must
	// not send anyone looking for htpasswd.
	if flag.Arg(0) == "hash-password" {
		if err := hashPassword(os.Stdin, os.Stdout, os.Stderr); err != nil {
			fmt.Fprintln(os.Stderr, "hash-password:", err)
			os.Exit(1)
		}
		return
	}

	if *showVersion {
		fmt.Println("stratus", version)
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

		if err := app.New(cfg, version).Run(ctx); err != nil {
			slog.Error("server stopped", "err", err)
			os.Exit(1)
		}
	}
}

// hashPassword reads one line from in and writes its bcrypt hash to out, for
// pasting into STRATUS_PASSWORD_HASH.
//
// Reading a line rather than everything means both of these work:
//
//	docker run --rm -i stratus hash-password        # typed, or piped
//	printf %s "$PASSWORD" | stratus hash-password
func hashPassword(in io.Reader, out, errOut io.Writer) error {
	if f, ok := in.(*os.File); ok {
		if fi, err := f.Stat(); err == nil && fi.Mode()&os.ModeCharDevice != 0 {
			// No terminal echo control without a dependency, so say so rather
			// than let someone type a password onto a shared screen.
			// Warning only: if it cannot be written, the hash below still can.
			_, _ = fmt.Fprintln(errOut, "reading the password from the terminal; it will be visible.")
			_, _ = fmt.Fprintf(errOut, "to avoid that: read -rs PASSWORD; printf %%s \"$PASSWORD\" | stratus hash-password\n")
		}
	}

	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	// Only the line ending is stripped: a password may legitimately end in a
	// space, and trimming it would hash something the user never typed.
	password := strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")

	hash, err := auth.Hash(password)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(out, hash)
	return err
}
