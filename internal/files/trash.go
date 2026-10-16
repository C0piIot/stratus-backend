package files

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/C0piIot/stratus-backend/internal/db"
)

// The trash (#274, #275): a deletion is rows moved to a table of their own
// and no bytes touched, and this is the half of the service that moves them
// back, destroys them, and empties what the month is up on. It was the back
// half of collect.go until #367, which is where the sweep reads it from --
// EmptyTrash runs before Collect so that what leaves the trash is garbage the
// same pass can take.

// Emptied is what one pass of the trash did, for the log line that follows it.
type Emptied struct {
	Batches int
	Files   int
	Bytes   int64
}

// EmptyTrash destroys every deletion older than before.
//
// A batch at a time, because a batch is what expires at once -- every row in
// one carries the same moment -- and because a pass that stops halfway has
// then finished whole deletions rather than half of one.
func (s *Service) EmptyTrash(ctx context.Context, before time.Time) (Emptied, error) {
	batches := map[string]string{}
	for t, err := range s.meta.ExpiredTrash(ctx, before) {
		if err != nil {
			return Emptied{}, err
		}
		batches[t.Batch] = t.OwnerID
	}

	var done Emptied
	for batch, owner := range batches {
		files, bytes, err := s.DestroyTrashed(ctx, owner, batch)
		done.Batches++
		done.Files += files
		done.Bytes += bytes
		if err != nil {
			return done, err
		}
	}
	return done, nil
}

// DestroyTrashed throws a deletion away for good: the rows first and the bytes
// after, which is the order every delete here follows. The other way round, a
// crash in between would leave the trash showing something whose bytes are
// already gone -- and #275 would put a broken file back.
//
// Nothing is reported for a batch that is not there. Pressing a button twice
// is not an error, and neither is this pass racing the one in the background.
func (s *Service) DestroyTrashed(ctx context.Context, owner, batch string) (int, int64, error) {
	var rows []db.Trashed
	err := s.meta.Tx(ctx, func(r db.Repo) error {
		var rerr error
		if rows, rerr = r.TrashedIn(ctx, owner, batch); rerr != nil {
			return rerr
		}
		return r.DeleteTrash(ctx, owner, batch)
	})
	if err != nil {
		return 0, 0, err
	}

	var files int
	var bytes int64
	for _, t := range rows {
		if t.IsDir {
			continue
		}
		if derr := s.dropBlob(ctx, t.BlobKey); derr != nil {
			return files, bytes, derr
		}
		files++
		bytes += t.Size
	}
	return files, bytes, nil
}

// Trash is a page of what has been deleted, newest first.
func (s *Service) Trash(ctx context.Context, owner string, after db.TrashCursor, limit int) ([]db.TrashBatch, error) {
	return s.meta.TrashBatches(ctx, owner, after, limit)
}

// Trashed is what one deletion holds, in path order, so the root of what was
// deleted is the first row.
func (s *Service) Trashed(ctx context.Context, owner, batch string) ([]db.Trashed, error) {
	return s.meta.TrashedIn(ctx, owner, batch)
}

// TrashTotals is how much room the trash is holding onto.
func (s *Service) TrashTotals(ctx context.Context, owner string) (db.TrashTotals, error) {
	return s.meta.TrashTotals(ctx, owner)
}

// Restore puts a deletion back where it came from, and answers with the row it
// landed at -- which is not always the one it left from (#275).
//
// Three things can have happened to the tree since, and each is decided here
// rather than discovered:
//
//   - **The path is taken.** It lands beside what is there, " (2)" before the
//     extension, which is the rule the import folder and the generated mounts
//     already use. Not over it: restoring is not a PUT, and whatever has the
//     name now is not a mistake. A folder that moves takes its contents with
//     it, since every row under it is rewritten to the new root.
//   - **The parent is gone**, deleted after this was. The directories on the
//     way are recreated, because they were part of the same tree.
//   - **An ancestor is a file now.** There is nowhere to put the tree, and
//     nothing here is going to delete somebody's file to make room: it is a
//     conflict and the page says which path.
//
// Everything is one transaction, so a restore either lands whole or not at
// all. What does not come back is the extracted metadata: it went with the
// cascade when the row left files, so the indexer reads these again -- which
// is why each one is announced to it on the way out.
func (s *Service) Restore(ctx context.Context, owner, batch string) (db.File, error) {
	var root db.File
	var restored []db.File

	if owner == "" {
		// The rule lives here and not in the template that does not draw the
		// button: a blob nobody can account for has no path to be put back at,
		// because the path was in the index that went wrong (#276).
		return db.File{}, fmt.Errorf("%w: nothing is known about where these belonged", db.ErrConflict)
	}

	err := s.meta.Tx(ctx, func(r db.Repo) error {
		rows, err := r.TrashedIn(ctx, owner, batch)
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			return fmt.Errorf("%w: no deletion %q", db.ErrNotFound, batch)
		}

		// Ordered by path, so the first row is the root of what was deleted:
		// a prefix sorts before everything under it.
		from := rows[0].Path
		to, err := freeName(ctx, func(p string) (db.File, error) { return r.FileByPath(ctx, owner, p) }, from)
		if err != nil {
			return err
		}
		if perr := restoreParents(ctx, r, owner, to); perr != nil {
			return perr
		}

		restored = restored[:0]
		for _, t := range rows {
			row := t.File
			row.ID = 0
			row.Path = to + strings.TrimPrefix(t.Path, from)
			if row.IsDir {
				if _, derr := r.CreateDir(ctx, owner, row.Path); derr != nil {
					return derr
				}
				continue
			}
			stored, perr := r.PutFile(ctx, row)
			if perr != nil {
				return perr
			}
			restored = append(restored, stored)
		}

		root, err = r.FileByPath(ctx, owner, to)
		if err != nil {
			return err
		}
		return r.DeleteTrash(ctx, owner, batch)
	})
	if err != nil {
		return db.File{}, err
	}

	// The indexer never saw these: the media rows went with the cascade. Told
	// rather than left for the hourly query, which is the same bargain a write
	// makes -- and a burst larger than the channel holds says so and brings
	// the query forward anyway.
	for _, f := range restored {
		s.written(f)
	}
	return root, nil
}

// restoreParents recreates the directories above a restored path, which were
// part of the same tree until somebody deleted them too.
//
// A file in the way is a conflict and not something to work around: making
// room would mean deleting it, and this function exists to put things back
// rather than to take them away.
func restoreParents(ctx context.Context, r db.Repo, owner, target string) error {
	parent := db.ParentOf(target)
	if parent == "" {
		return nil
	}
	switch f, err := r.FileByPath(ctx, owner, parent); {
	case err == nil && f.IsDir:
		return nil
	case err == nil:
		return fmt.Errorf("%w: %q is a file now", db.ErrConflict, parent)
	case !errors.Is(err, db.ErrNotFound):
		return err
	}

	if err := restoreParents(ctx, r, owner, parent); err != nil {
		return err
	}
	_, err := r.CreateDir(ctx, owner, parent)
	return err
}
