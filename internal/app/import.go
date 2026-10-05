// Adopting somebody else's instance, once the survey has said it can be.
package app

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/C0piIot/stratus-backend/internal/config"
	"github.com/C0piIot/stratus-backend/internal/files"
	"github.com/C0piIot/stratus-backend/internal/nextcloud"
)

// ImportNextcloud adopts a Nextcloud library into this server, writing rows
// and not a single byte.
//
// The second entry point beside SurveyNextcloud, and it runs that survey
// first rather than trusting somebody to have run it: what the survey answers
// is whether the database and the bucket still agree, and importing when they
// do not writes rows pointing at objects that are not there. The survey is
// also where encryption and a multibucket instance are refused.
//
// Unlike the survey this does open the metadata database, and migrates it the
// way the server does -- same lock, same refusal of a schema written by a
// newer build. Inserting into a schema that is not there, or into one this
// binary does not know, is worse than not starting.
func ImportNextcloud(ctx context.Context, cfg config.Config, opts nextcloud.ImportOptions, out io.Writer) error {
	owner := cfg.Username
	if owner == "" {
		return errors.New("STRATUS_USERNAME is not set: an import needs the user to file the library under")
	}

	store, err := openStorage(ctx, cfg.Storage)
	if err != nil {
		return err
	}
	if closer, ok := store.(io.Closer); ok {
		defer func() { _ = closer.Close() }()
	}

	survey, err := nextcloud.Survey(ctx, store, opts.Options)
	if err != nil {
		return err
	}
	if rerr := survey.Render(out); rerr != nil {
		return rerr
	}
	if !survey.Adoptable() {
		return errors.New("this library cannot be adopted as it stands: see the survey above")
	}

	database, err := openDatabase(ctx, cfg.Database)
	if err != nil {
		return err
	}
	defer func() { _ = database.Close() }()
	if merr := database.Migrate(ctx); merr != nil {
		return fmt.Errorf("migrate: %w", merr)
	}

	// No watcher: the indexer is not running in this process, and it does not
	// need telling. What it reads is a query over the file rows, so the rows
	// this writes are picked up by the server's next pass on their own.
	report, err := nextcloud.Import(ctx, store, files.New(store, database), owner, opts)
	if err != nil {
		return err
	}
	return report.Render(out)
}
