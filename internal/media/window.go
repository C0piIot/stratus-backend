package media

import (
	"errors"
	"fmt"
	"io"
	"os"
)

// Looking at a film without downloading it.
//
// #145 made the rule -- nothing large is copied to be read -- and left a hole
// behind it: a recording over the bound had no thumbnail at all, because a
// decoder opens files and the only way to give it one was to fetch the lot. In
// a personal cloud the large files are exactly the films.
//
// A decoder does not need the file. It needs the headers and the first frames,
// and both are a few megabytes read over ranges. What that costs is measured in
// #149, on sixty seconds of 720p cut to its first two megabytes: Matroska gives
// a frame, a faststart MP4 gives a frame, an MP4 with its moov atom at the end
// gives nothing -- and the same file with its head and its moov written into a
// sparse file gives a frame while occupying 2.1 MB of the 6.2 it claims.

// windowHead is how much of the beginning is enough. A second of 4K at 50
// megabits is about six megabytes, so this holds the first few seconds of
// anything a camera produces -- which is where the frame comes from -- and is
// three orders of magnitude less than the film.
const windowHead = 16 << 20

// window builds a local file holding the parts of a recording a decoder has to
// read, and nothing else.
//
// The file is as long as the original and almost entirely a hole: the length is
// set first and the pieces are written where they belong, so the offsets inside
// the container still point at the right places. What is not written is never
// read, which is the whole point -- on a bucket it is never even requested.
func window(r io.ReadSeeker, size int64, name, dir string, head int64) (string, func(), error) {
	pieces, err := windowPieces(r, size, name, head)
	if err != nil {
		return "", nil, err
	}

	tmp, err := os.CreateTemp(dir, "window-*")
	if err != nil {
		return "", nil, fmt.Errorf("window %q: %w", name, err)
	}
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
	}

	// The apparent length first, so everything written below lands at the
	// offset the container expects.
	if err := tmp.Truncate(size); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("window %q: %w", name, err)
	}

	for _, piece := range pieces {
		if err := copyPiece(tmp, r, piece); err != nil {
			cleanup()
			return "", nil, fmt.Errorf("window %q: %w", name, err)
		}
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("window %q: %w", name, err)
	}
	return tmp.Name(), func() { _ = os.Remove(tmp.Name()) }, nil
}

// piece is a range of the original that has to be present locally.
type piece struct{ at, length int64 }

// windowPieces decides what a decoder needs out of this container.
//
// The head is always one of them: it carries the first frames, and in a
// faststart MP4 or any Matroska it carries the headers too. An MP4 that keeps
// its moov atom at the end -- which is every file a camera writes -- needs that
// as well, and where it is comes from the reader in mp4.go without reading
// anything on the way.
// The head length is an argument rather than the constant so that a test can
// ask for a small one: a fixture that fits inside sixteen megabytes would prove
// nothing about a film that does not.
func windowPieces(r io.ReadSeeker, size int64, name string, headLen int64) ([]piece, error) {
	head := piece{at: 0, length: min(size, headLen)}

	switch {
	case matroska(name):
		return []piece{head}, nil

	case isobmff(name):
		at, length, err := findMoov(r, size)
		if err != nil {
			return nil, err
		}
		if at+length <= head.length {
			// Already inside the head, which is what faststart means.
			return []piece{head}, nil
		}
		return []piece{head, {at: at, length: length}}, nil

	default:
		// A container this cannot take a window of. The caller refuses rather
		// than downloading it, which is the rule.
		return nil, errNotRead
	}
}

func copyPiece(dst *os.File, src io.ReadSeeker, p piece) error {
	if _, err := src.Seek(p.at, io.SeekStart); err != nil {
		return err
	}
	if _, err := dst.Seek(p.at, io.SeekStart); err != nil {
		return err
	}
	// CopyN and not Copy: what is being avoided here is reading past the piece.
	// A short read is not a failure -- a piece can run to the end of the file.
	if _, err := io.CopyN(dst, src, p.length); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

// windowable reports whether a file too large to copy can still be looked at
// through a window.
func windowable(name string) bool { return isobmff(name) || matroska(name) }
