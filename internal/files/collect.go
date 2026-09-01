package files

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// DefaultGrace is how old a blob must be before it is considered garbage.
//
// It is not an optimisation. Writes go blob first and row second, so a blob no
// row points at may be a write still in flight rather than one that failed. An
// hour is far longer than any upload this project expects and far shorter than
// anyone will notice.
const DefaultGrace = time.Hour

// ErrEmptyIndex is returned when the database references no blobs at all and
// the store is not empty. See the check in Collect.
var ErrEmptyIndex = errors.New("files: the index is empty and the blob store is not")

// Collected is what one pass did, for the log line that follows it.
type Collected struct {
	Scanned int
	Deleted int
	Bytes   int64
}

// Collect deletes blobs no row points at.
//
// There is a leak to collect because a write takes a fresh key every time, so
// that a failed overwrite cannot destroy the content it was replacing. What that
// buys in safety it pays for here: every overwrite leaves the previous blob
// behind, and so does every write whose row never committed.
func (s *Service) Collect(ctx context.Context, olderThan time.Duration) (Collected, error) {
	// The database is read first and the store second, and the order matters: a
	// row written between the two would otherwise have its blob listed as
	// unreferenced and deleted.
	// The whole set in memory: about forty bytes per file, so a hundred
	// thousand photos is four megabytes. Fine for a personal cloud, and the
	// limit worth knowing -- the alternative is paging the listing and asking
	// the database in batches, which is more machinery than this is worth.
	referenced := make(map[string]struct{})
	for key, err := range s.meta.BlobKeys(ctx) {
		if err != nil {
			return Collected{}, fmt.Errorf("read the referenced keys: %w", err)
		}
		referenced[key] = struct{}{}
	}

	var done Collected
	cutoff := time.Now().Add(-olderThan)

	for info, err := range s.blobs.List(ctx, "") {
		if err != nil {
			return done, fmt.Errorf("list the blob store: %w", err)
		}
		done.Scanned++

		// An index with no rows at all, against a store with objects in it, is
		// far more likely to be a database pointed somewhere new than a library
		// somebody emptied. Refusing turns a catastrophe into a log line.
		if len(referenced) == 0 {
			return done, ErrEmptyIndex
		}

		if _, live := referenced[info.Key]; live || info.ModTime.After(cutoff) {
			continue
		}
		if err := s.blobs.Delete(ctx, info.Key); err != nil {
			return done, fmt.Errorf("delete %q: %w", info.Key, err)
		}
		done.Deleted++
		done.Bytes += info.Size
	}
	return done, nil
}
