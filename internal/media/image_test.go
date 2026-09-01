package media

import (
	"bytes"
	"encoding/binary"
	"testing"
	"time"
)

// A JPEG assembled here rather than committed as a binary. It is more code than
// checking in a photo, and it is worth it: the bytes that matter are visible,
// the file is tiny, and nobody has to wonder what is inside it.
//
// Layout, all offsets relative to the start of the TIFF header:
//
//	  8  IFD0            4 entries, 54 bytes
//	 62  IFD0 data       Make, Model
//	 82  Exif IFD        3 entries, 42 bytes
//	124  Exif IFD data   DateTimeOriginal
const (
	ifd0Offset     = 8
	ifd0DataOffset = 62
	exifOffset     = 82
	exifDataOffset = 124

	makeOffset  = ifd0DataOffset
	modelOffset = ifd0DataOffset + 6
)

const (
	typeASCII = 2
	typeShort = 3
	typeLong  = 4
)

func exifJPEG(t *testing.T) []byte {
	t.Helper()

	tiff := &bytes.Buffer{}
	tiff.WriteString("II")                                  // little endian
	_ = binary.Write(tiff, binary.LittleEndian, uint16(42)) // the answer, per the TIFF spec
	_ = binary.Write(tiff, binary.LittleEndian, uint32(ifd0Offset))

	// IFD0: make, model, orientation, and a pointer to the Exif IFD.
	_ = binary.Write(tiff, binary.LittleEndian, uint16(4))
	writeEntry(tiff, 0x010F, typeASCII, 6, makeOffset)   // Make
	writeEntry(tiff, 0x0110, typeASCII, 14, modelOffset) // Model
	writeEntry(tiff, 0x0112, typeShort, 1, 6)            // Orientation: rotate 90
	writeEntry(tiff, 0x8769, typeLong, 1, exifOffset)    // Exif IFD pointer
	_ = binary.Write(tiff, binary.LittleEndian, uint32(0))

	tiff.WriteString("Apple\x00")
	tiff.WriteString("iPhone 15 Pro\x00")

	// Exif IFD: when it was taken and how big it is.
	_ = binary.Write(tiff, binary.LittleEndian, uint16(3))
	writeEntry(tiff, 0x9003, typeASCII, 20, exifDataOffset) // DateTimeOriginal
	writeEntry(tiff, 0xA002, typeLong, 1, 4032)             // PixelXDimension
	writeEntry(tiff, 0xA003, typeLong, 1, 3024)             // PixelYDimension
	_ = binary.Write(tiff, binary.LittleEndian, uint32(0))

	tiff.WriteString("2024:06:01 12:30:15\x00")

	payload := append([]byte("Exif\x00\x00"), tiff.Bytes()...)

	jpeg := &bytes.Buffer{}
	jpeg.Write([]byte{0xFF, 0xD8})                                   // SOI
	jpeg.Write([]byte{0xFF, 0xE1})                                   // APP1
	_ = binary.Write(jpeg, binary.BigEndian, uint16(len(payload)+2)) // JPEG lengths are big endian and include themselves
	jpeg.Write(payload)
	jpeg.Write([]byte{0xFF, 0xD9}) // EOI
	return jpeg.Bytes()
}

// writeEntry writes one IFD entry. Values of four bytes or fewer live in the
// entry; anything longer is an offset into the data that follows the IFD.
func writeEntry(w *bytes.Buffer, tag, typ uint16, count, value uint32) {
	_ = binary.Write(w, binary.LittleEndian, tag)
	_ = binary.Write(w, binary.LittleEndian, typ)
	_ = binary.Write(w, binary.LittleEndian, count)
	if typ == typeShort && count == 1 {
		// A SHORT sits in the first two bytes of the field, not the last.
		_ = binary.Write(w, binary.LittleEndian, uint16(value))
		_ = binary.Write(w, binary.LittleEndian, uint16(0))
		return
	}
	_ = binary.Write(w, binary.LittleEndian, value)
}

func TestExtractImage(t *testing.T) {
	t.Parallel()
	got, err := extractImage(bytes.NewReader(exifJPEG(t)))
	if err != nil {
		t.Fatalf("extractImage: %v", err)
	}

	want := time.Date(2024, 6, 1, 12, 30, 15, 0, time.UTC)
	if !got.TakenAt.Equal(want) {
		t.Errorf("TakenAt = %v, want %v", got.TakenAt, want)
	}
	if got.Width != 4032 || got.Height != 3024 {
		t.Errorf("dimensions = %dx%d, want 4032x3024", got.Width, got.Height)
	}
	if got.Orientation != 6 {
		t.Errorf("Orientation = %d, want 6", got.Orientation)
	}
	if got.Camera != "Apple iPhone 15 Pro" {
		t.Errorf("Camera = %q", got.Camera)
	}
}

// TestExtractImageWithoutExif is the ordinary case for a screenshot: no
// metadata is not a failure, it is a file we now know nothing more about.
func TestExtractImageWithoutExif(t *testing.T) {
	t.Parallel()
	plain := []byte{0xFF, 0xD8, 0xFF, 0xD9}

	got, err := extractImage(bytes.NewReader(plain))
	if err != nil {
		t.Fatalf("extractImage on a JPEG with no exif = %v, want no error", err)
	}
	if !got.TakenAt.IsZero() || got.Camera != "" {
		t.Errorf("got %+v, want nothing but the kind", got)
	}
}

func TestCamera(t *testing.T) {
	t.Parallel()
	tests := []struct {
		make, model, want string
	}{
		{"Apple", "iPhone 15 Pro", "Apple iPhone 15 Pro"},
		// Canon writes the make into the model, and "Canon Canon EOS R6" is how
		// that looks if nobody checks.
		{"Canon", "Canon EOS R6", "Canon EOS R6"},
		{"", "Pixel 8", "Pixel 8"},
		{"NIKON CORPORATION", "", "NIKON CORPORATION"},
		{"", "", ""},
	}
	for _, tt := range tests {
		if got := camera(tt.make, tt.model); got != tt.want {
			t.Errorf("camera(%q, %q) = %q, want %q", tt.make, tt.model, got, tt.want)
		}
	}
}
