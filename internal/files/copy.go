package files

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/C0piIot/stratus-backend/internal/db"
)

// Copying a file, or a directory with everything under it.
//
// A collection answered 501 until now, and what kept it there was not the
// recursion: it was what a copy that fails halfway leaves behind. RFC 4918 9.8.5
// wants a multistatus naming the members that failed, and the honest way to not
// need one is to have no half-copied tree to report (#43).
//
// So this is all or nothing, and the thing that buys it is the ordering this
// package already lives by. Blobs are written first and rows second, and a blob
// no row points at is collectable garbage (#17) -- so a tree copy writes every
// blob, commits every row in one transaction, and a failure anywhere leaves a
// tree that was never touched and a handful of orphans the sweep takes. The
// bytes move outside the transaction, which is also what keeps a write
// transaction from being held open for the length of a copy.
//
// The blob is not shared between the two rows, tempting as one insert is:
// Remove deletes blobs as soon as its transaction commits, so removing either
// copy would delete bytes the other still points at. That needs reference
// counting, or a delete that leaves its blobs to the sweep, and both are bigger
// than this.

// ErrNoSpace is a copy that would not fit in what the store says is left.
//
// A refusal before anything is written, not a failure partway: a disk filled by
// a copy takes the database on it down too, so the worst moment to discover
// there was no room is thirty gigabytes in. WebDAV answers 507 to it.
var ErrNoSpace = errors.New("files: not enough room")

// Copy copies a file, or a directory and everything under it when recursive,
// and reports whether the destination is new.
//
// Not recursive on a directory copies the directory and nothing in it, which is
// what Depth: 0 means in RFC 4918 9.8.3 and the one case a client can ask for
// that costs nothing.
func (s *Service) Copy(ctx context.Context, owner, from, to string, recursive bool) (created bool, err error) {
	// The same two refusals a rename has, and for the same reasons: copying
	// something onto itself, and copying a tree into its own subdirectory.
	if verr := db.ValidateMove(from, to); verr != nil {
		return false, verr
	}

	source, err := s.meta.FileByPath(ctx, owner, from)
	if err != nil {
		return false, err
	}

	_, statErr := s.meta.FileByPath(ctx, owner, to)
	switch {
	case statErr == nil:
		created = false
	case errors.Is(statErr, db.ErrNotFound):
		created = true
	default:
		return false, statErr
	}

	plan, err := s.planCopy(ctx, owner, source, to, recursive)
	if err != nil {
		return false, err
	}
	if rerr := s.roomFor(ctx, plan); rerr != nil {
		return false, rerr
	}

	rows, err := s.writeCopies(ctx, owner, plan)
	if err != nil {
		return false, err
	}

	orphaned, err := s.commitCopies(ctx, owner, to, rows)
	if err != nil {
		// Nothing was committed, so every blob written above belongs to
		// nobody. Best effort, and #17 collects what this misses.
		for _, row := range rows {
			if !row.IsDir {
				_ = s.blobs.Delete(ctx, row.BlobKey)
			}
		}
		return false, err
	}

	// What the destination held before, now that the rows naming it are gone.
	for _, key := range orphaned {
		if derr := s.blobs.Delete(ctx, key); derr != nil {
			return created, fmt.Errorf("delete blob %q: %w", key, derr)
		}
	}
	for _, row := range rows {
		if !row.IsDir {
			s.written(row)
		}
	}
	return created, nil
}

// copyStep is one row to make: a directory to create, or a file to read from
// source and write under path.
type copyStep struct {
	source db.File
	path   string
}

// planCopy lists what the copy will do, parents before children, which is the
// order requireParent needs and the order a depth-first walk produces.
func (s *Service) planCopy(ctx context.Context, owner string, source db.File, to string, recursive bool) ([]copyStep, error) {
	plan := []copyStep{{source: source, path: to}}
	if !source.IsDir || !recursive {
		return plan, nil
	}

	children, err := s.meta.ListFiles(ctx, owner, source.Path)
	if err != nil {
		return nil, err
	}
	for _, child := range children {
		// The child keeps its name under the new parent.
		under := to + "/" + child.Path[strings.LastIndexByte(child.Path, '/')+1:]
		deeper, err := s.planCopy(ctx, owner, child, under, true)
		if err != nil {
			return nil, err
		}
		plan = append(plan, deeper...)
	}
	return plan, nil
}

// roomFor refuses a copy the store says will not fit.
//
// A best answer and not a reservation -- another process can fill the disk
// between here and the last write -- but it is the difference between a refusal
// and a server that filled its own disk finding out.
func (s *Service) roomFor(ctx context.Context, plan []copyStep) error {
	var need int64
	for _, step := range plan {
		need += step.source.Size
	}

	free, err := s.blobs.FreeSpace(ctx)
	if err != nil {
		return err
	}
	if need > free {
		return fmt.Errorf("%w: the copy needs %d bytes and the store has %d", ErrNoSpace, need, free)
	}
	return nil
}

// writeCopies moves the bytes and returns the rows that will describe them,
// none of them committed yet.
func (s *Service) writeCopies(ctx context.Context, owner string, plan []copyStep) ([]db.File, error) {
	rows := make([]db.File, 0, len(plan))
	for _, step := range plan {
		if step.source.IsDir {
			rows = append(rows, db.File{OwnerID: owner, Path: step.path, IsDir: true})
			continue
		}

		row, err := s.copyOne(ctx, owner, step)
		if err != nil {
			for _, written := range rows {
				if !written.IsDir {
					_ = s.blobs.Delete(ctx, written.BlobKey)
				}
			}
			return nil, err
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// copyOne reads one file and writes its bytes under a key of their own.
func (s *Service) copyOne(ctx context.Context, owner string, step copyStep) (db.File, error) {
	body, err := s.OpenFile(ctx, step.source)
	if err != nil {
		return db.File{}, err
	}
	defer func() { _ = body.Close() }()

	return s.storeBlob(ctx, owner, step.path, body, step.source.Size, step.source.MIMEType)
}

// commitCopies is the only transaction, and it is the last thing that happens:
// what the destination held is removed and the new rows are inserted together,
// so there is no moment at which the tree is half one thing and half the other.
//
// It returns the blobs the replaced destination left behind, for the caller to
// delete once the transaction has actually committed.
func (s *Service) commitCopies(ctx context.Context, owner, to string, rows []db.File) ([]string, error) {
	var orphaned []string

	err := s.meta.Tx(ctx, func(r db.Repo) error {
		switch _, err := r.FileByPath(ctx, owner, to); {
		case err == nil:
			// RFC 4918 9.8.4: an overwrite replaces the destination, it does
			// not merge into it.
			keys, rerr := removeTree(ctx, r, owner, to)
			if rerr != nil {
				return rerr
			}
			orphaned = keys
		case !errors.Is(err, db.ErrNotFound):
			return err
		}

		for i, row := range rows {
			if perr := s.requireParent(ctx, r, owner, row.Path); perr != nil {
				return perr
			}
			if row.IsDir {
				if _, cerr := r.CreateDir(ctx, owner, row.Path); cerr != nil {
					return cerr
				}
				continue
			}
			stored, perr := r.PutFile(ctx, row)
			if perr != nil {
				return perr
			}
			rows[i] = stored
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return orphaned, nil
}
