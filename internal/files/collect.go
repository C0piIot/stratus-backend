package files

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/stratus-private-cloud/stratus-backend/internal/db"
	"github.com/stratus-private-cloud/stratus-backend/internal/storage"
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
//
// The note saying a file could not be turned into a picture is the second user,
// and it is here for the other half of the same property: what expires is
// attempted again, which is what keeps a refusal from outliving the build that
// made it (#338). So the retention is read in two ways -- how long a cache is
// worth keeping, and how long a verdict is worth trusting.
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
//
// Deleted and Trashed are two different fates and are counted apart: what was
// destroyed is gone and its Bytes are free, while what was moved to the trash
// is still there and still costs the same room (#276).
type Collected struct {
	Scanned int
	Deleted int
	Trashed int
	Bytes   int64
}

// Collect finds the blobs no row points at: the originals go to the trash and
// what was derived from them is destroyed.
//
// It is the net and no longer the routine collector (#272). What drops a blob
// on purpose drops it then and there: a write takes its own predecessor, a
// delete takes what it deleted, and both take the pictures made from it. What
// reaches this sweep is what nobody was left to announce -- a write whose row
// never committed, a process killed between the two, a tidy-up the store
// refused, a derived object whose generator has moved on -- plus whatever
// arrived in the store by some other road.
//
// **An original is moved and not destroyed** (#276). The alternative was a
// threshold: refuse the pass when too much of the store looks unreferenced,
// which is a heuristic with a number nobody can justify, and which cannot
// catch the likely case -- a database restored from last week, where only the
// last week looks like garbage. Keeping the bytes for a month is the honest
// version of the same worry. The name is not kept with them, because there is
// none: it was in the index that went wrong, and a blob key is opaque by
// design (#24). What the month buys is not a button, it is time.
//
// **What was derived is destroyed all the same**, because it can be made
// again: a thumbnail kept for a month against a picture that regenerates on
// the first request is noise with a bill attached.
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
	// A blob in the trash is not garbage: it has a deletion it belongs to and
	// a month to live (#274). The sweep is not who decides to throw it away --
	// EmptyTrash is, when the month is up. It is also what keeps this pass
	// from finding the same orphan again tomorrow.
	for key, err := range s.meta.TrashKeys(ctx) {
		if err != nil {
			return Collected{}, fmt.Errorf("read the trashed keys: %w", err)
		}
		referenced[key] = struct{}{}
	}

	// Counted and buffered rather than decided object by object, and that is
	// not tidiness. An original that goes to the trash stops being garbage in
	// this same pass, so whether its thumbnails survive would otherwise depend
	// on the order the store happened to list them in. Deciding at the end
	// means the derived half is judged against what is referenced *plus* what
	// this pass just rescued. It also means a listing that fails halfway
	// deletes nothing, where before it left half a store swept.
	var done Collected
	var orphans []db.File
	var derivedKeys []storage.ObjectInfo
	cutoff := time.Now().Add(-olderThan)

	for info, err := range s.blobs.List(ctx, "") {
		if err != nil {
			return Collected{}, fmt.Errorf("list the blob store: %w", err)
		}
		// The database's own backups (#398). Each is a file with a row now,
		// but a database put back from one has no row for the backups taken
		// after it -- AdoptBackups files them on the next start, and this is
		// what keeps them until then. A store holding only those is a fresh
		// install rather than an index pointed somewhere new.
		if strings.HasPrefix(info.Key, BackupPrefix) {
			continue
		}
		done.Scanned++

		// An index with no rows at all, against a store with objects in it, is
		// far more likely to be a database pointed somewhere new than a library
		// somebody emptied. Refusing turns a catastrophe into a log line --
		// and it stays even with the trash behind it, because moving a whole
		// library into the trash is not an answer either.
		if len(referenced) == 0 {
			return done, ErrEmptyIndex
		}
		if info.ModTime.After(cutoff) {
			continue
		}

		parent, derived := parentOf(info.Key)
		switch {
		case derived && parent == "":
			// Under the derived prefix and naming no parent, so nothing here
			// can say whether it is garbage. Skipped rather than guessed at:
			// deleting what cannot be judged is how a sweep becomes the thing
			// that loses data.
			continue
		case derived:
			derivedKeys = append(derivedKeys, info)
		default:
			if _, live := referenced[info.Key]; !live {
				orphans = append(orphans, db.File{
					BlobKey: info.Key,
					Size:    info.Size,
					MTime:   info.ModTime.UTC().Truncate(db.TimePrecision),
				})
			}
		}
	}

	// The originals first, so that what they were made into is judged against
	// a trash that already holds them.
	if len(orphans) > 0 {
		if err := s.meta.Trash(ctx, rand.Text(), orphans, time.Now().UTC()); err != nil {
			return done, fmt.Errorf("move %d unaccounted blobs to the trash: %w", len(orphans), err)
		}
		for _, o := range orphans {
			referenced[o.BlobKey] = struct{}{}
		}
		done.Trashed = len(orphans)
	}

	for _, info := range derivedKeys {
		parent, _ := parentOf(info.Key)
		if _, live := referenced[parent]; live {
			// And a picture whose generator has moved on is garbage even
			// though the file it was made from is still here. Without this the
			// generation in the key would only stop the old object being
			// served, and leave it on the disk for as long as its parent
			// lived -- which is the objection that kept the key a pure
			// function of its parent's in the first place (#161).
			//
			// A cache outlives its welcome a week after it was written,
			// whatever its parent is doing.
			stale := staleGeneration(info.Key)
			expired := cached(info.Key) && time.Since(info.ModTime) > CacheRetention
			if !stale && !expired {
				continue
			}
		}
		if err := s.blobs.Delete(ctx, info.Key); err != nil {
			return done, fmt.Errorf("delete %q: %w", info.Key, err)
		}
		done.Deleted++
		done.Bytes += info.Size
	}
	return done, nil
}
