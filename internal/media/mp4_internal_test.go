package media

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/C0piIot/stratus-backend/internal/db"
)

// The fixtures are a second of 320x240 h264 from ffmpeg's testsrc, in the three
// layouts that matter: the moov box in front (what -movflags +faststart writes,
// and what a file served over HTTP wants), the moov box behind the frames
// (what every camera writes, and the case a stream cannot read at all), and one
// carrying a display matrix (what a phone writes when it is held upright).
const (
	faststart = "moov-first.mp4"
	phone     = "moov-last.mp4"
	rotated   = "portrait.mp4"
)

func TestProbeVideoReadsBothLayouts(t *testing.T) {
	t.Parallel()

	for _, name := range []string{faststart, phone} {
		got := probe(t, name)

		if got.Kind != db.KindVideo {
			t.Errorf("%s: kind = %q", name, got.Kind)
		}
		if got.DurationMS != 1000 {
			t.Errorf("%s: duration = %dms, want 1000", name, got.DurationMS)
		}
		if got.Width != 320 || got.Height != 240 {
			t.Errorf("%s: %dx%d, want 320x240", name, got.Width, got.Height)
		}
		if got.Codec != "h264" {
			t.Errorf("%s: codec = %q, want the name ffprobe uses", name, got.Codec)
		}
		// The creation time is in the file, in a format whose epoch is 1904.
		if want := time.Date(2026, 2, 3, 10, 11, 12, 0, time.UTC); !got.TakenAt.Equal(want) {
			t.Errorf("%s: TakenAt = %v, want %v", name, got.TakenAt, want)
		}
		if got.Orientation != 0 {
			t.Errorf("%s: orientation = %d, want none", name, got.Orientation)
		}
	}
}

// TestProbeVideoReadsTheDisplayMatrix is what keeps a portrait recording from
// playing on its side. The fixture is the same film with the matrix a phone
// writes when it is held upright -- ffprobe calls it a rotation of -90, and the
// EXIF value for the same quarter turn is 6.
func TestProbeVideoReadsTheDisplayMatrix(t *testing.T) {
	t.Parallel()

	got := probe(t, rotated)
	if got.Orientation != 6 {
		t.Errorf("orientation = %d, want 6 for a quarter turn clockwise", got.Orientation)
	}
	// The dimensions are the coded ones either way: the matrix turns the frame,
	// it does not change what was encoded.
	if got.Width != 320 || got.Height != 240 {
		t.Errorf("%dx%d, want the coded size", got.Width, got.Height)
	}
}

// TestProbeVideoGivesUp: every one of these is a file ffprobe has to look at,
// and the answer here has to be "not mine" rather than a half-filled row.
func TestProbeVideoGivesUp(t *testing.T) {
	t.Parallel()

	sound := readFixture(t, phone)
	cases := map[string][]byte{
		"nothing at all":       nil,
		"not a container":      []byte("this is a text file, honestly"),
		"a header and no more": readFixture(t, phone)[:16],
		"a truncated moov":     readFixture(t, phone)[:len(sound)-100],
		"a size that lies": func() []byte {
			broken := readFixture(t, phone)
			// The first box claims to be longer than the file.
			broken[0], broken[1], broken[2], broken[3] = 0x7f, 0xff, 0xff, 0xff
			return broken
		}(),
	}

	for name, body := range cases {
		if _, err := probeVideo(bytes.NewReader(body), int64(len(body))); err == nil {
			t.Errorf("%s: answered instead of standing aside", name)
		}
	}
}

// TestProbeVideoRefusesAnUnknownCodec is the rule that keeps the two paths
// writing the same column: a fourcc this does not know is not guessed at, it is
// handed to ffprobe, which does know.
func TestProbeVideoRefusesAnUnknownCodec(t *testing.T) {
	t.Parallel()

	body := readFixture(t, phone)
	// The last one: the first is in the list of brands the file declares at the
	// very top, which is not what is being read here.
	i := bytes.LastIndex(body, []byte("avc1"))
	if i < 0 {
		t.Fatal("the fixture has no avc1 sample entry")
	}
	copy(body[i:], "zzzz")

	if m, err := probeVideo(bytes.NewReader(body), int64(len(body))); err == nil {
		t.Errorf("a codec nothing recognises came back as %+v", m)
	}
}

// TestProbeVideoAgreesWithFFprobe is the only honest way to know that a
// hand-written reader of somebody else's format is right: ask the reference.
// Skipped where there is no ffprobe, which is the toolchain container -- the
// container smoke suite runs it in the image that has one.
func TestProbeVideoAgreesWithFFprobe(t *testing.T) {
	t.Parallel()

	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("no ffprobe on the PATH")
	}

	for _, name := range []string{faststart, phone, rotated} {
		report, err := runProbe(t.Context(), ffprobe, filepath.Join("testdata", name))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		want := report.mediaFrom(db.KindVideo)
		got := probe(t, name)

		if got.DurationMS != want.DurationMS {
			t.Errorf("%s: duration = %d, ffprobe says %d", name, got.DurationMS, want.DurationMS)
		}
		if got.Width != want.Width || got.Height != want.Height {
			t.Errorf("%s: %dx%d, ffprobe says %dx%d", name, got.Width, got.Height, want.Width, want.Height)
		}
		if got.Codec != want.Codec {
			t.Errorf("%s: codec = %q, ffprobe says %q", name, got.Codec, want.Codec)
		}
		if !got.TakenAt.Equal(want.TakenAt) {
			t.Errorf("%s: TakenAt = %v, ffprobe says %v", name, got.TakenAt, want.TakenAt)
		}
		if got.Orientation != want.Orientation {
			t.Errorf("%s: orientation = %d, ffprobe says %d", name, got.Orientation, want.Orientation)
		}
	}
}

// TestProbeVideoReadsLittle is the whole point of this file: the frames are
// never read. A reader that counted every byte it was asked for would have to
// report the size of the file.
func TestProbeVideoReadsLittle(t *testing.T) {
	t.Parallel()

	body := readFixture(t, phone)
	counted := &countingReader{inner: bytes.NewReader(body)}
	if _, err := probeVideo(counted, int64(len(body))); err != nil {
		t.Fatal(err)
	}

	// The moov box of this fixture is under a kilobyte and the frames are six,
	// so anything near the size of the file means the mdat box was read.
	if counted.read > int64(len(body))/2 {
		t.Errorf("read %d bytes of a %d-byte file: the frames are being read",
			counted.read, len(body))
	}
}

type countingReader struct {
	inner *bytes.Reader
	read  int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.inner.Read(p)
	c.read += int64(n)
	return n, err
}

func (c *countingReader) Seek(offset int64, whence int) (int64, error) {
	return c.inner.Seek(offset, whence)
}

func probe(t *testing.T, name string) db.Media {
	t.Helper()
	body := readFixture(t, name)
	m, err := probeVideo(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatalf("probeVideo(%s): %v", name, err)
	}
	return m
}

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return body
}

// TestISOBMFFNames pins which files are claimed, since the answer decides
// whether a copy happens at all.
func TestISOBMFFNames(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"holiday.mp4", "clip.MOV", "film.m4v"} {
		if !isobmff(name) {
			t.Errorf("%s is not claimed", name)
		}
	}
	// .m4a is the same box structure and deliberately not claimed: what a track
	// carries is tags this does not read, and it is megabytes rather than
	// gigabytes.
	for _, name := range []string{"song.m4a", "film.mkv", "clip.webm", "notes.txt", "noext"} {
		if isobmff(name) {
			t.Errorf("%s is claimed and should not be", name)
		}
	}
}
