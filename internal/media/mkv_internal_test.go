package media

import (
	"bytes"
	"encoding/binary"
	"math"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/C0piIot/stratus-backend/internal/db"
)

// The fixtures are a second of 320x240 from ffmpeg's testsrc, muxed twice: h264
// in Matroska and VP9 in WebM, which are the same container with two names and
// two codec strings.
const (
	matroskaFixture = "clip.mkv"
	webmFixture     = "clip.webm"
)

func TestProbeMatroska(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		matroskaFixture: "h264",
		webmFixture:     "vp9",
	}

	for name, codec := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			body := readFixture(t, name)

			got, err := probeMatroska(bytes.NewReader(body), int64(len(body)))
			if err != nil {
				t.Fatalf("probeMatroska: %v", err)
			}
			if got.Kind != db.KindVideo {
				t.Errorf("kind = %q", got.Kind)
			}
			if got.DurationMS != 1000 {
				t.Errorf("duration = %dms, want 1000", got.DurationMS)
			}
			if got.Width != 320 || got.Height != 240 {
				t.Errorf("%dx%d, want 320x240", got.Width, got.Height)
			}
			if got.Codec != codec {
				t.Errorf("codec = %q, want %q", got.Codec, codec)
			}
			// The recording date, which Matroska counts in nanoseconds from
			// 2001 rather than from 1970.
			if want := time.Date(2026, 2, 3, 10, 11, 12, 0, time.UTC); !got.TakenAt.Equal(want) {
				t.Errorf("TakenAt = %v, want %v", got.TakenAt, want)
			}
		})
	}
}

// TestProbeMatroskaReadsLittle is the point of the whole file: the clusters are
// the film and they are never read.
func TestProbeMatroskaReadsLittle(t *testing.T) {
	t.Parallel()

	body := readFixture(t, matroskaFixture)
	counted := &countingReader{inner: bytes.NewReader(body)}
	if _, err := probeMatroska(counted, int64(len(body))); err != nil {
		t.Fatal(err)
	}

	// Info and Tracks live in the first few hundred bytes of the fixture and
	// the clusters are the remaining seven kilobytes.
	if counted.read > int64(len(body))/4 {
		t.Errorf("read %d bytes of a %d-byte file: the clusters are being read",
			counted.read, len(body))
	}
}

// TestProbeMatroskaGivesUp: each of these is a file ffprobe has to look at, and
// the answer here has to be "not mine" rather than half a row.
func TestProbeMatroskaGivesUp(t *testing.T) {
	t.Parallel()

	whole := readFixture(t, matroskaFixture)
	cases := map[string][]byte{
		"nothing at all":  nil,
		"not a container": []byte("this is a text file, honestly"),
		"a header alone":  whole[:8],
		"a truncated segment": func() []byte {
			return whole[:120]
		}(),
		// An element whose length is every bit set, which is what a live
		// broadcast writes: there is no arithmetic to step over it with.
		"a length nobody knows": ebmlFile(
			ebml(idSegment, append(
				ebml(idInfo, ebmlUint(idTimecodeScale, 1_000_000)),
				ebmlUnknownLength(idTracks)...,
			)),
		),
	}

	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if m, err := probeMatroska(bytes.NewReader(body), int64(len(body))); err == nil {
				t.Errorf("%s answered %+v instead of standing aside", name, m)
			}
		})
	}
}

// TestProbeMatroskaRefusesWhatItCannotSay covers the three built by hand: a
// codec nothing here knows, a track that says its size is turned, and a file
// with no duration in it at all.
func TestProbeMatroskaRefusesWhatItCannotSay(t *testing.T) {
	t.Parallel()

	tests := map[string][]byte{
		// V_MS/VFW/FOURCC hides another codec inside a structure this does not
		// read, so it is the case the table leaves out on purpose.
		"a codec in a wrapper": matroskaFile(1000, "V_MS/VFW/FOURCC", 320, 240, false),
		"a codec nobody knows": matroskaFile(1000, "V_SOMETHING_NEW", 320, 240, false),
		// Rotation lives in Projection, and answering without reading it hands
		// back a portrait film as landscape.
		"a turned picture": matroskaFile(1000, "V_VP9", 320, 240, true),
		"no duration":      matroskaFile(0, "V_VP9", 320, 240, false),
		"no size":          matroskaFile(1000, "V_VP9", 0, 0, false),
	}

	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if m, err := probeMatroska(bytes.NewReader(body), int64(len(body))); err == nil {
				t.Errorf("%s answered %+v", name, m)
			}
		})
	}
}

// TestProbeMatroskaAgreesWithFFprobe is the only honest way to know a
// hand-written reader of somebody else's format is right. Skipped where there
// is no ffprobe, which is the toolchain container.
func TestProbeMatroskaAgreesWithFFprobe(t *testing.T) {
	t.Parallel()

	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("no ffprobe on the PATH")
	}

	for _, name := range []string{matroskaFixture, webmFixture} {
		report, err := runProbe(t.Context(), ffprobe, filepath.Join("testdata", name))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		want := report.mediaFrom(db.KindVideo)

		body := readFixture(t, name)
		got, err := probeMatroska(bytes.NewReader(body), int64(len(body)))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}

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
	}
}

func TestMatroskaNames(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"film.mkv", "clip.WEBM"} {
		if !matroska(name) {
			t.Errorf("%s is not claimed", name)
		}
	}
	for _, name := range []string{"film.mp4", "song.mka", "notes.txt", "noext"} {
		if matroska(name) {
			t.Errorf("%s is claimed and should not be", name)
		}
	}
}

// --- building files by hand -------------------------------------------------
//
// The fixtures next door are real files and cover what a muxer writes. These
// are the shapes it does not: a codec nothing knows, a length nobody can step
// over, a track that says it is turned.

// ebmlID writes an identifier, which keeps the marker bits it was written with.
func ebmlID(id uint32) []byte {
	switch {
	case id <= 0xFF:
		return []byte{byte(id)}
	case id <= 0xFFFF:
		return []byte{byte(id >> 8), byte(id)}
	case id <= 0xFFFFFF:
		return []byte{byte(id >> 16), byte(id >> 8), byte(id)}
	default:
		return []byte{byte(id >> 24), byte(id >> 16), byte(id >> 8), byte(id)}
	}
}

// ebmlSize writes a length as an eight-byte variable integer, which is always
// valid and saves counting.
func ebmlSize(n int) []byte {
	out := make([]byte, 8)
	binary.BigEndian.PutUint64(out, uint64(n))
	out[0] |= 0x01 // the marker for the widest form
	return out
}

func ebml(id uint32, body []byte) []byte {
	out := append(ebmlID(id), ebmlSize(len(body))...)
	return append(out, body...)
}

// ebmlUnknownLength is the element a live broadcast writes: every value bit of
// the length set.
func ebmlUnknownLength(id uint32) []byte {
	return append(ebmlID(id), 0xFF)
}

func ebmlUint(id uint32, v uint64) []byte {
	body := make([]byte, 8)
	binary.BigEndian.PutUint64(body, v)
	return ebml(id, body)
}

func ebmlFloat(id uint32, v float64) []byte {
	body := make([]byte, 8)
	binary.BigEndian.PutUint64(body, math.Float64bits(v))
	return ebml(id, body)
}

// ebmlFile puts the EBML header in front of whatever follows, since every file
// starts with one and the reader checks for it.
func ebmlFile(rest []byte) []byte {
	header := ebml(idEBMLHeader, []byte("\x42\x82\x88matroska"))
	return append(header, rest...)
}

// matroskaFile is a whole small file: a header, a segment, and inside it the
// information and the one track this reads.
func matroskaFile(durationMS int, codec string, width, height int, turned bool) []byte {
	info := ebmlUint(idTimecodeScale, 1_000_000)
	if durationMS > 0 {
		info = append(info, ebmlFloat(idDuration, float64(durationMS))...)
	}

	video := []byte{}
	if width > 0 {
		video = append(video, ebmlUint(idPixelWidth, uint64(width))...)
		video = append(video, ebmlUint(idPixelHeight, uint64(height))...)
	}
	if turned {
		video = append(video, ebml(idProjection, ebmlUint(0x7671, 0))...)
	}

	entry := ebmlUint(idTrackType, trackTypeVideo)
	entry = append(entry, ebml(idCodecID, []byte(codec))...)
	entry = append(entry, ebml(idVideo, video)...)

	segment := append(ebml(idInfo, info), ebml(idTracks, ebml(idTrackEntry, entry))...)
	return ebmlFile(ebml(idSegment, segment))
}

// TestMatroskaByHand is the builder proving itself: what it writes has to be
// read back, or every refusal above would pass for the wrong reason.
func TestMatroskaByHand(t *testing.T) {
	t.Parallel()

	body := matroskaFile(2500, "V_VP9", 640, 480, false)
	got, err := probeMatroska(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatalf("probeMatroska: %v", err)
	}
	if got.DurationMS != 2500 || got.Width != 640 || got.Height != 480 || got.Codec != "vp9" {
		t.Errorf("got %+v", got)
	}
}
