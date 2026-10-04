// Surveying somebody else's instance, before anything is adopted from it.
package app

import (
	"context"
	"io"

	"github.com/C0piIot/stratus-backend/internal/config"
	"github.com/C0piIot/stratus-backend/internal/nextcloud"
)

// SurveyNextcloud reports what adopting a Nextcloud instance into this
// server's blob store would find.
//
// A package-level function and not a method, like Probe: it is a second entry
// point into the same configuration rather than a thing the server does. It
// opens the blob store because that is the composition root's job -- the
// survey takes the port, and knowing which backend is behind it belongs here.
//
// The metadata database is deliberately not opened. Nothing is written, and an
// import that has not been decided on yet has no business migrating a schema.
func SurveyNextcloud(ctx context.Context, cfg config.Config, opts nextcloud.Options, out io.Writer) error {
	store, err := openStorage(ctx, cfg.Storage)
	if err != nil {
		return err
	}
	if closer, ok := store.(io.Closer); ok {
		defer func() { _ = closer.Close() }()
	}

	report, err := nextcloud.Survey(ctx, store, opts)
	if err != nil {
		return err
	}
	return report.Render(out)
}
