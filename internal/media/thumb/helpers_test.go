package thumb

import (
	"bytes"
	"encoding/binary"
	"errors"
	"image"
	"image/jpeg"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stratus-private-cloud/stratus-backend/internal/db"
	"github.com/stratus-private-cloud/stratus-backend/internal/files"
)

const owner = "edu"

// The fixtures are internal/media's, read from where that package keeps them
// rather than copied: a film is a film to both halves.
const (
	phone           = "moov-last.mp4"
	faststart       = "moov-first.mp4"
	matroskaFixture = "clip.mkv"
)

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("..", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func write(t *testing.T, s *files.Service, path string, body []byte, mimeType string) db.File {
	t.Helper()
	// The tree invariant applies here too: a file needs its directory, and
	// every directory above it. Made one segment at a time and tolerant of one
	// that is already there, so a case can write two files into one folder.
	var built string
	for seg := range strings.SplitSeq(db.ParentOf(path), "/") {
		if seg == "" {
			continue
		}
		if built != "" {
			built += "/"
		}
		built += seg
		if _, err := s.Mkdir(t.Context(), owner, built); err != nil && !errors.Is(err, db.ErrConflict) {
			t.Fatalf("Mkdir(%q): %v", built, err)
		}
	}
	f, err := s.Write(t.Context(), owner, path, bytes.NewReader(body), int64(len(body)), mimeType)
	if err != nil {
		t.Fatalf("Write(%q): %v", path, err)
	}
	return f
}

// exifJPEGWithPixels is a picture that decodes with an orientation tag in
// front of it, which is all this side reads of EXIF: the indexer's tests build
// the whole directory, and this is the one entry out of it.
func exifJPEGWithPixels(t *testing.T, orientation, width, height int) []byte {
	t.Helper()

	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, image.NewRGBA(image.Rect(0, 0, width, height)), nil); err != nil {
		t.Fatal(err)
	}

	// A TIFF header and one directory holding the one entry.
	tiff := &bytes.Buffer{}
	tiff.WriteString("II")                                  // little endian
	_ = binary.Write(tiff, binary.LittleEndian, uint16(42)) // the answer, per the TIFF spec
	_ = binary.Write(tiff, binary.LittleEndian, uint32(8))  // IFD0 follows the header
	_ = binary.Write(tiff, binary.LittleEndian, uint16(1))
	_ = binary.Write(tiff, binary.LittleEndian, uint16(0x0112)) // Orientation
	_ = binary.Write(tiff, binary.LittleEndian, uint16(3))      // SHORT
	_ = binary.Write(tiff, binary.LittleEndian, uint32(1))
	_ = binary.Write(tiff, binary.LittleEndian, uint16(orientation))
	_ = binary.Write(tiff, binary.LittleEndian, uint16(0)) // the value field is four bytes
	_ = binary.Write(tiff, binary.LittleEndian, uint32(0)) // no next directory

	payload := append([]byte("Exif\x00\x00"), tiff.Bytes()...)
	segment := &bytes.Buffer{}
	segment.Write([]byte{0xFF, 0xE1})                                   // APP1
	_ = binary.Write(segment, binary.BigEndian, uint16(len(payload)+2)) // lengths are big endian and include themselves
	segment.Write(payload)

	// After the start-of-image marker and before everything the encoder wrote,
	// which is where a segment goes.
	body := encoded.Bytes()
	out := append([]byte{}, body[:2]...)
	out = append(out, segment.Bytes()...)
	return append(out, body[2:]...)
}
