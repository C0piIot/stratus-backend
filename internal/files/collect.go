package files

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/C0piIot/stratus-backend/internal/db"
)

// DefaultGrace is how old a blob must be before it is considered garbage.
//
// It is not an optimisation. Writes go blob first and row second, so a blob no
// row points at may be a write still in flight rather than one that failed. An
// hour is far longer than any upload this project expects and far shorter than
// anyone will notice.
const DefaultGrace = time.Hour

// DerivedPrefix is where everything generated from a file lives: thumbnails
// today, transcoded segments later -- which will have to be one object each
// under a leaf name, or packed into one, for the reason DerivedKey gives.
//
// It is a prefix and not a bucket or a table because the blob store already is
// the cache, and a third pluggable seam is what principle 3 forbids. Beyond
// tidiness it earns its keep twice: a lifecycle rule or a backup policy can
// treat derived data differently from originals, and the key carries its
// parent's -- which is what lets one sweep collect both.
const DerivedPrefix = "derived/"

// DerivedGeneration is which generator made the objects this build writes.
//
// Raise it when what comes out of the generator changes -- a rotation that was
// not read, a frame chosen differently, a different encoder -- and every
// derived object made by an older one stops being served and is collected on
// the next sweep. It is media.Version for the other half of internal/media,
// and it exists because that half had no such thing: a thumbnail made before
// the EXIF fix stayed sideways for good, since a derived key is a pure function
// of its parent's and nothing in it could say the generator had moved (#161).
//
// It lives here and not beside the generator because this is where the sweep
// reads it back, which is the same reason the rest of the shape does. Whoever
// changes what a picture looks like has to come here, and internal/media says
// so where the pixels are made.
const DerivedGeneration = 1

// DerivedKey names an object generated from the blob at parent. The shape is
// defined here, next to the sweep that has to read it back, so that a caller
// cannot invent a key nothing will ever collect.
//
// name is one segment, and that is the whole of the key rule this project has:
// no key Stratus writes may be a prefix of another, because S3 holds "a" and
// "a/b" at once and a filesystem cannot. A nested name breaks that on the disk
// backend and breaks parentOf everywhere -- it cuts at the last slash, so the
// object would name a parent no row holds and the sweep would delete it an hour
// later. It panics rather than returning an error because every caller passes a
// constant: a slash here is a bug in this repository, not a condition to handle.
// The generation goes on the front of that segment rather than being a path
// element of its own, so that parentOf still finds the parent by cutting at the
// last slash and callers keep passing the name they already passed.
func DerivedKey(parent, name string) string {
	if strings.Contains(name, "/") {
		panic(fmt.Sprintf("files: derived name %q is not one segment", name))
	}
	return DerivedPrefix + parent + "/" + generationPrefix + strconv.Itoa(DerivedGeneration) + "-" + name
}

// CachePrefix begins the name of a derived object kept only as a cache: made
// to save work, not because anything asked for it to be kept. The sweep
// collects one CacheRetention after it was written, even while its parent
// lives -- a film's re-encoded segments are gigabytes, and a cache that only
// ever grew would fill the disk with films watched once (#50).
const CachePrefix = "cache-"

// CacheRetention is how long a cached derived object is kept after it was
// written. A week: long enough for a film to be finished and watched again,
// short enough that one watched once does not stay for good.
const CacheRetention = 7 * 24 * time.Hour

// cached reports whether a derived key is a cache, by the name after its
// generation.
func cached(key string) bool {
	leaf := key[strings.LastIndexByte(key, '/')+1:]
	_, rest, ok := strings.Cut(leaf, "-")
	return ok && strings.HasPrefix(rest, CachePrefix)
}

// generationPrefix marks the number at the front of a derived leaf. A letter
// and not bare digits, so that it cannot be read as a size.
const generationPrefix = "g"

// staleGeneration reports whether a derived key was written by a generator that
// is no longer this one.
//
// A leaf that names no generation at all is stale too, and deliberately: those
// are the keys written before this existed, which is exactly the set of
// pictures this mechanism was added to replace. Nothing else can be under the
// prefix -- DerivedKey is the only way to make one of these, and it stamps
// every object it names.
func staleGeneration(key string) bool {
	leaf := key[strings.LastIndexByte(key, '/')+1:]
	rest, ok := strings.CutPrefix(leaf, generationPrefix)
	if !ok {
		return true
	}
	digits, _, ok := strings.Cut(rest, "-")
	if !ok {
		return true
	}
	generation, err := strconv.Atoi(digits)
	return err != nil || generation != DerivedGeneration
}

// parentOf undoes DerivedKey: everything between the prefix and the last
// segment is the blob the object was made from.
//
// The two results are independent. derived says the key is ours to reason
// about; parent is empty when it is ours and yet names nothing, which is a key
// this version did not write and cannot judge.
func parentOf(key string) (parent string, derived bool) {
	rest, ok := strings.CutPrefix(key, DerivedPrefix)
	if !ok {
		return "", false
	}
	i := strings.LastIndexByte(rest, '/')
	if i <= 0 {
		return "", true
	}
	return rest[:i], true
}

// dropBlob throws away a blob and everything derived from it, which is what
// giving one up on purpose means (#272): the picture made from a photograph and
// the segments re-encoded from a film are garbage the moment their original is,
// and waiting for the sweep to work that out is waiting a day.
//
// Collect still knows the same rule -- a derived object is garbage when its
// parent is gone -- and still has to, for everything that never came through
// here: a process that died between the two, a generation that moved on, an
// object whose parent went before this existed.
//
// The listing is of one prefix with two or three objects in it, not of the
// store. A caller deleting a thousand rows pays a thousand of them, which is
// the same order as the thousand deletes it was already doing.
func (s *Service) dropBlob(ctx context.Context, key string) error {
	if err := s.blobs.Delete(ctx, key); err != nil {
		return fmt.Errorf("delete blob %q: %w", key, err)
	}
	for info, err := range s.blobs.List(ctx, DerivedPrefix+key+"/") {
		if err != nil {
			return fmt.Errorf("list what was derived from %q: %w", key, err)
		}
		if derr := s.blobs.Delete(ctx, info.Key); derr != nil {
			return fmt.Errorf("delete derived %q: %w", info.Key, derr)
		}
	}
	return nil
}

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
// It is the net and no longer the routine collector (#272). What drops a blob
// on purpose drops it then and there: a write takes its own predecessor, a
// delete takes what it deleted, and both take the pictures made from it. What
// reaches this sweep is what nobody was left to announce -- a write whose row
// never committed, a process killed between the two, a tidy-up the store
// refused, a derived object whose generator has moved on -- plus whatever
// arrived in the store by some other road.
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
	// A blob in the trash is not garbage: it has an owner, a deletion it
	// belongs to and a month to live (#274). The sweep is not who decides to
	// throw it away -- EmptyTrash is, when the month is up.
	for key, err := range s.meta.TrashKeys(ctx) {
		if err != nil {
			return Collected{}, fmt.Errorf("read the trashed keys: %w", err)
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

		// A derived object has no row of its own and never will: it is garbage
		// exactly when the blob it was made from is. Without this rule the
		// sweep would delete every thumbnail an hour after it was made and the
		// lazy path would generate it again, forever -- a treadmill that shows
		// up in a CPU graph and an egress bill rather than as a failure.
		//
		// It is also what catches an overwrite, which leaves the old blob
		// orphaned *and* its thumbnails, filed under a key nothing will look
		// for again.
		var live bool
		switch parent, derived := parentOf(info.Key); {
		case derived && parent == "":
			// Under the derived prefix and naming no parent, so nothing here
			// can say whether it is garbage. Skipped rather than guessed at:
			// deleting what cannot be judged is how a sweep becomes the thing
			// that loses data.
			continue
		case derived:
			_, live = referenced[parent]
			// And a picture whose generator has moved on is garbage even
			// though the file it was made from is still here. Without this the
			// generation in the key would only stop the old object being
			// served, and leave it on the disk for as long as its parent
			// lived -- which is the objection that kept the key a pure
			// function of its parent's in the first place (#161).
			if live && staleGeneration(info.Key) {
				live = false
			}
			// And a cache outlives its welcome a week after it was written,
			// whatever its parent is doing.
			if live && cached(info.Key) && time.Since(info.ModTime) > CacheRetention {
				live = false
			}
		default:
			_, live = referenced[info.Key]
		}
		if live || info.ModTime.After(cutoff) {
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

// DefaultTrashRetention is how long a deletion is kept before it is destroyed
// for good (#274).
//
// A month, which is long enough to notice an accident -- including the kind
// that is only noticed when somebody goes looking for a photograph months
// later, which this does not cover and nothing can. It is a constant and not
// a setting, like the grace and the cache retention beside it: nobody has
// needed another number yet, and the page lets anybody who wants the room back
// have it now.
const DefaultTrashRetention = 30 * 24 * time.Hour

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

// TrashTotals is how much room the trash is holding onto.
func (s *Service) TrashTotals(ctx context.Context, owner string) (db.TrashTotals, error) {
	return s.meta.TrashTotals(ctx, owner)
}

// maxCopies bounds the search for a free name when what was deleted has been
// replaced since. A hundred: a folder that already holds a hundred copies of
// one name is a situation to be told about rather than added to.
const maxCopies = 100

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
		to, err := freeName(ctx, r, owner, from)
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

// freeName is where a restored root can land: its own path, or that path with
// a number beside it when something has taken the name since.
func freeName(ctx context.Context, r db.Repo, owner, target string) (string, error) {
	for n := 1; n <= maxCopies; n++ {
		candidate := db.CopyName(target, n)
		switch _, err := r.FileByPath(ctx, owner, candidate); {
		case errors.Is(err, db.ErrNotFound):
			return candidate, nil
		case err != nil:
			return "", err
		}
	}
	return "", fmt.Errorf("%w: %q is taken, and so are the first %d names beside it",
		db.ErrConflict, target, maxCopies)
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
