package files

import (
	"context"
	"errors"
	"fmt"

	"github.com/C0piIot/stratus-backend/internal/db"
)

// Adopt writes rows for blobs that are already in the store under keys this
// server did not choose (#24).
//
// It is the one door in this package that does not touch the blob store at
// all, and that is what it is for: a Nextcloud bucket already holds the bytes
// under `urn:oid:<fileid>`, so adopting it is a batch of inserts and not a
// copy of a hundred gigabytes. The invariant the rest of the package exists to
// keep -- blob first, row second -- is already satisfied by whoever wrote the
// bucket, which is why there is nothing here to order.
//
// It can do that only because a blob key is opaque. Nothing derives a path, a
// kind or an extension from one; see newBlobKey, which says so where the key
// is made.
//
// **A path that is already taken is skipped and counted, never replaced.** An
// import is not a write: the row that is there was put there by somebody, and
// a second run of an interrupted import must be able to finish rather than
// overwrite what the first one did. Missing parent directories are created on
// the way, like a restore, so the caller does not have to hand them over in
// any order.
//
// The caller batches. One transaction per call, and a library of a hundred
// thousand files in a single one would hold a write lock for as long as the
// import runs.
func (s *Service) Adopt(ctx context.Context, owner string, rows []db.File) (adopted, skipped int, err error) {
	if owner == "" {
		return 0, 0, fmt.Errorf("%w: an import needs an owner to file things under", db.ErrConflict)
	}

	err = s.meta.Tx(ctx, func(r db.Repo) error {
		adopted, skipped = 0, 0
		for _, f := range rows {
			if verr := db.ValidatePath(f.Path); verr != nil {
				return verr
			}
			f.OwnerID = owner

			switch _, ferr := r.FileByPath(ctx, owner, f.Path); {
			case ferr == nil:
				skipped++
				continue
			case !errors.Is(ferr, db.ErrNotFound):
				return ferr
			}

			// restoreParents is both halves of requireParent and the making of
			// what is missing, which is what a tree arriving from somewhere
			// else needs: it refuses a parent that is a file and builds one
			// that is not there.
			if perr := restoreParents(ctx, r, owner, f.Path); perr != nil {
				return perr
			}

			if f.IsDir {
				if _, derr := r.CreateDir(ctx, owner, f.Path); derr != nil {
					return derr
				}
			} else if _, perr := r.PutFile(ctx, f); perr != nil {
				return perr
			}
			adopted++
		}
		return nil
	})
	if err != nil {
		return 0, 0, err
	}
	return adopted, skipped, nil
}
