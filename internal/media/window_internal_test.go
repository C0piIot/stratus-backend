package media

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// A Window has to be two things at once: enough for a decoder, and much less
// than the file. The first is checked with the real binary, because "enough for
// a decoder" is not a claim Go can make about itself.
//
// The head is one kilobyte in most of these. The fixtures are seven, so a
// Window of the real sixteen megabytes would hold every one of them whole and
// prove nothing about the film this exists for.

// TestWindowReadsAlmostNothing is the property that makes this worth having: a
// film whose moov atom is at the end costs its beginning plus that atom, and
// not the film.
func TestWindowReadsAlmostNothing(t *testing.T) {
	t.Parallel()

	const head = 1024
	body := readFixture(t, phone)
	at, length, err := findMoov(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatal(err)
	}

	counted := &countingReader{inner: bytes.NewReader(body)}
	local, cleanup, err := Window(counted, int64(len(body)), phone, t.TempDir(), head)
	if err != nil {
		t.Fatalf("Window: %v", err)
	}
	defer cleanup()

	// The head, the atom, and the handful of box headers walked to find it.
	// Nowhere near the file, which is what the frames would have cost.
	if want := head + length + 512; counted.read > want {
		t.Errorf("read %d bytes, want no more than %d of a %d-byte file",
			counted.read, want, len(body))
	}
	if counted.read < head+length {
		t.Errorf("read %d bytes, which is not even the head and the atom", counted.read)
	}

	// And the file claims the original's length, because every offset inside a
	// container is an offset into the whole thing.
	info, err := os.Stat(local)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != int64(len(body)) {
		t.Errorf("the Window is %d bytes long, want the original's %d", info.Size(), len(body))
	}
	if at == 0 {
		t.Error("the fixture has its moov atom at the front; this case needs the other kind")
	}
}

// TestWindowPieces is the decision itself: what has to be present for each
// container, and the refusal for the ones where there is no Window to take.
func TestWindowPieces(t *testing.T) {
	t.Parallel()

	const head = 1024

	t.Run("matroska is its beginning", func(t *testing.T) {
		t.Parallel()
		body := readFixture(t, matroskaFixture)
		pieces, err := windowPieces(bytes.NewReader(body), int64(len(body)), matroskaFixture, head)
		if err != nil {
			t.Fatal(err)
		}
		if len(pieces) != 1 || pieces[0].at != 0 || pieces[0].length != head {
			t.Errorf("pieces = %+v, want the head alone", pieces)
		}
	})

	t.Run("an mp4 with its headers at the end needs them too", func(t *testing.T) {
		t.Parallel()
		body := readFixture(t, phone)
		pieces, err := windowPieces(bytes.NewReader(body), int64(len(body)), phone, head)
		if err != nil {
			t.Fatal(err)
		}
		if len(pieces) != 2 {
			t.Fatalf("pieces = %+v, want the head and the moov atom", pieces)
		}
		if pieces[1].at <= head || pieces[1].length == 0 {
			t.Errorf("the second piece is %+v, want the atom's own range", pieces[1])
		}
	})

	t.Run("a faststart mp4 is its beginning", func(t *testing.T) {
		t.Parallel()
		body := readFixture(t, faststart)
		// A head large enough to hold the atom, which is what faststart means
		// and what a real sixteen-megabyte Window gives every such file.
		pieces, err := windowPieces(bytes.NewReader(body), int64(len(body)), faststart, WindowHead)
		if err != nil {
			t.Fatal(err)
		}
		if len(pieces) != 1 {
			t.Errorf("pieces = %+v, want the head alone: the atom is already inside it", pieces)
		}
	})

	t.Run("there is no Window onto an avi", func(t *testing.T) {
		t.Parallel()
		if _, err := windowPieces(bytes.NewReader(nil), 1<<30, "film.avi", head); err == nil {
			t.Error("a container with no Window was given one")
		}
	})

	t.Run("nor onto an mp4 with no atom in it", func(t *testing.T) {
		t.Parallel()
		if _, err := windowPieces(bytes.NewReader([]byte("not an mp4")), 10, "broken.mp4", head); err == nil {
			t.Error("a file with no moov atom was given a Window")
		}
	})
}

// TestWindowWhenNothingWorks: a Window that cannot be built is a thumbnail that
// does not exist, not a half-written file left in the data directory.
func TestWindowWhenNothingWorks(t *testing.T) {
	t.Parallel()

	t.Run("nowhere to put it", func(t *testing.T) {
		t.Parallel()
		body := readFixture(t, matroskaFixture)
		nowhere := filepath.Join(t.TempDir(), "not", "there")
		if _, _, err := Window(bytes.NewReader(body), int64(len(body)), matroskaFixture, nowhere, 1024); err == nil {
			t.Error("a Window was built in a directory that does not exist")
		}
	})

	t.Run("a store that will not answer", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		if _, _, err := Window(stuckReader{}, 1<<30, matroskaFixture, dir, 1024); err == nil {
			t.Error("a Window was built out of a reader that refuses to read")
		}
		// And nothing is left behind: the temporary file goes with the failure.
		left, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(left) != 0 {
			t.Errorf("%d files left in the Spool directory after a failure", len(left))
		}
	})
}
