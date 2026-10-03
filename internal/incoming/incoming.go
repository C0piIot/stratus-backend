// Package incoming moves what appears in a directory on the machine's own
// filesystem into the library (#257).
//
// It is a door for the machines that cannot speak any of the protocols: a
// scanner dropping PDFs onto an SMB share, an SD card copied in, a cron job
// writing a backup. The directory is the server's own disk and has nothing to
// do with the blob store, which may well be a bucket somewhere else.
//
// Everything goes through internal/files, so an imported file gets its row, its
// validator, its owner and its announcement to the indexer exactly as a WebDAV
// PUT does. This package decides three things that are its own: when a file has
// finished arriving, what it is called when something is already there, and
// what happens when any of it fails.
package incoming

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/C0piIot/stratus-backend/internal/db"
)

// maxCopies bounds the search for a free name. A directory that already holds a
// hundred files called the same thing is a loop somewhere else, and walking it
// on every pass would be this one's contribution to it.
const maxCopies = 100

// Store is the file layer this needs, which is the ordinary one. Narrow on
// purpose: an importer has no business deleting or moving what is already in
// the library.
type Store interface {
	Write(ctx context.Context, owner, path string, body io.Reader, size int64, mimeType string) (db.File, error)
	Stat(ctx context.Context, owner, path string) (db.File, error)
	Mkdir(ctx context.Context, owner, path string) (db.File, error)
}

// Watcher sweeps one directory. It is safe to call Pass from one goroutine and
// State from another, which is what the status page does.
type Watcher struct {
	dir   string
	owner string
	files Store

	mu sync.Mutex
	// sizes is what each file measured on the pass before this one, which is
	// the whole of "has it finished arriving": a file is imported when two
	// passes in a row agree about its size. See Pass.
	sizes map[string]int64
	state State
}

// State is what the last pass did, for the page that reports on it.
type State struct {
	// Waiting is how many files are in the directory but not in the library
	// yet: still being written, or failing.
	Waiting int
	// Imported is the running total since this process started.
	Imported int64
	// LastRun is when the last pass finished, zero before the first one.
	LastRun time.Time
	// LastError is why the last pass did not empty the directory, and empty
	// when it had nothing to complain about. One line, because a page shows one
	// line; the log has every one of them.
	LastError string
}

// New builds the sweep for one directory, filing everything under owner. It
// touches nothing until Pass is called, so a directory that is not there yet is
// a failure of a pass rather than of a startup.
func New(dir, owner string, files Store) *Watcher {
	return &Watcher{dir: dir, owner: owner, files: files, sizes: map[string]int64{}}
}

// Dir is the directory being swept.
func (w *Watcher) Dir() string { return w.dir }

// State returns what the last pass did.
func (w *Watcher) State() State {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.state
}

// Pass imports everything that has finished arriving and reports how many files
// moved.
//
// A file is finished when its size is the same as it was on the previous pass.
// That is the only test available without asking the filesystem for something
// it does not know either: a writer holding a file open at a size it has
// reached is indistinguishable from one that has closed it, and fsnotify would
// say the same thing a minute louder. The cost is that a copy slow enough to
// stall for a whole interval is imported half-written; the gain is no
// dependency and nothing to configure.
//
// It never stops on the first failure. One file nothing can read must not hold
// up the rest of the directory, so an error is recorded, the file is left
// exactly where it is, and the next pass tries it again.
func (w *Watcher) Pass(ctx context.Context) (int, error) {
	before := w.snapshot()
	after := make(map[string]int64, len(before))

	var imported int
	var failure error
	walkErr := filepath.WalkDir(w.dir, func(name string, d fs.DirEntry, err error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		switch {
		case err != nil:
			// A directory that cannot be read is one branch of the sweep, not
			// the sweep. Recorded and stepped over.
			failure = cmp(failure, fmt.Errorf("read %q: %w", name, err))
			return nil
		case name == w.dir:
			return nil
		}

		rel, relErr := filepath.Rel(w.dir, name)
		if relErr != nil {
			failure = cmp(failure, relErr)
			return nil
		}
		rel = filepath.ToSlash(rel)

		// Hidden names are skipped whole, directories included: a half-written
		// download is .part or .crdownload, a Mac leaves .DS_Store beside
		// everything, and neither is a file somebody meant to file.
		if strings.HasPrefix(path.Base(rel), ".") {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		// Directories are not imported. They arrive under the files that are in
		// them, and an empty one is nothing to put in a library. They are left
		// on disk, which is also what makes a scanner that writes into the same
		// folder every day keep working.
		if d.IsDir() {
			return nil
		}
		// Only regular files: a symlink, a socket or a device is not something
		// to read into a blob store.
		if !d.Type().IsRegular() {
			return nil
		}

		info, statErr := d.Info()
		if statErr != nil {
			failure = cmp(failure, statErr)
			return nil
		}

		// Two passes agreeing is what makes it finished. Anything else is
		// remembered at its current size and looked at again next time.
		if was, seen := before[rel]; !seen || was != info.Size() {
			after[rel] = info.Size()
			return nil
		}

		switch err := w.importFile(ctx, name, rel, info.Size()); {
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
			return err
		case err != nil:
			failure = cmp(failure, err)
			// Kept at its size, so the next pass finds it finished and tries
			// again rather than waiting another interval first.
			after[rel] = info.Size()
		default:
			imported++
		}
		return nil
	})
	if walkErr != nil && failure == nil {
		failure = walkErr
	}

	w.record(after, imported, failure)
	return imported, failure
}

// importFile writes one file into the library and then removes it from disk.
//
// In that order, and never the other way round: the copy that matters is the
// one in the blob store, and a file deleted before the write committed would be
// a file nobody has. A write that succeeds and a delete that fails leaves the
// file to be imported again next pass, which arrives beside the first as a
// copy -- the one direction of this that costs a duplicate rather than a loss.
func (w *Watcher) importFile(ctx context.Context, name, rel string, size int64) error {
	target := rel
	if err := db.ValidatePath(target); err != nil {
		return fmt.Errorf("%q is not a path this server can store: %w", rel, err)
	}
	if err := w.mkdirAll(ctx, path.Dir(target)); err != nil {
		return err
	}
	target, err := w.free(ctx, target)
	if err != nil {
		return err
	}

	body, err := os.Open(name) //nolint:gosec // G304: the path comes from walking the configured directory.
	if err != nil {
		return err
	}
	defer func() { _ = body.Close() }()

	// No MIME type: the one thing this knows about the file is less than what
	// internal/files works out from its first bytes.
	if _, err := w.files.Write(ctx, w.owner, target, body, size, ""); err != nil {
		return fmt.Errorf("import %q: %w", rel, err)
	}
	if err := os.Remove(name); err != nil {
		return fmt.Errorf("imported %q and could not remove it: %w", rel, err)
	}
	return nil
}

// mkdirAll makes the directories a mirrored path needs, one level at a time,
// because that is what the file layer offers and what it checks.
func (w *Watcher) mkdirAll(ctx context.Context, dir string) error {
	if dir == "." || dir == "/" || dir == "" {
		return nil
	}
	walked := ""
	for _, segment := range strings.Split(dir, "/") {
		walked = path.Join(walked, segment)
		switch existing, err := w.files.Stat(ctx, w.owner, walked); {
		case err == nil && existing.IsDir:
			continue
		case err == nil:
			return fmt.Errorf("%q is a file in the library and this needs it to be a folder", walked)
		case !errors.Is(err, db.ErrNotFound):
			return err
		}
		if _, err := w.files.Mkdir(ctx, w.owner, walked); err != nil && !errors.Is(err, db.ErrConflict) {
			return err
		}
	}
	return nil
}

// free is the name to file this under: the one it came with, or that name with
// a number before its extension.
//
// Renamed and never replaced. Everywhere else here a write to a path that
// exists replaces what is there, because that is what a PUT means; this is not
// somebody saying "replace that", it is a folder two machines drop files into,
// and a scanner reusing a filename would otherwise quietly destroy the last one.
// The numbering is " (2)" before the extension, which is what /photos/ and
// /playlists/ already disambiguate with, and the extension survives because it
// is what the indexer reads.
func (w *Watcher) free(ctx context.Context, target string) (string, error) {
	for n := 1; n <= maxCopies; n++ {
		candidate := db.CopyName(target, n)
		switch _, err := w.files.Stat(ctx, w.owner, candidate); {
		case errors.Is(err, db.ErrNotFound):
			return candidate, nil
		case err != nil:
			return "", err
		}
	}
	return "", fmt.Errorf("%q is taken, and so are the first %d names beside it", target, maxCopies)
}

func (w *Watcher) snapshot() map[string]int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.sizes
}

func (w *Watcher) record(sizes map[string]int64, imported int, err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.sizes = sizes
	w.state.Waiting = len(sizes)
	w.state.Imported += int64(imported)
	w.state.LastRun = time.Now()
	w.state.LastError = ""
	if err != nil {
		w.state.LastError = err.Error()
	}
}

// cmp keeps the first failure of a pass. The first rather than the last because
// a cascade usually starts with the one worth reading.
func cmp(kept, err error) error {
	if kept != nil {
		return kept
	}
	return err
}
