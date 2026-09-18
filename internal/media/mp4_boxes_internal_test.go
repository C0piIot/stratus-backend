package media

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// The fixtures next door are real files and cover what a camera writes. These
// are hand-built boxes, for the shapes a camera does not write and a reader
// still has to survive: the wide headers, a track with no picture in it, a
// sample entry that stops halfway.

func boxOf(name string, body []byte) []byte {
	out := make([]byte, 8, 8+len(body))
	binary.BigEndian.PutUint32(out, uint32(8+len(body)))
	copy(out[4:], name)
	return append(out, body...)
}

// wideBox is the same with a 64-bit length, which is what a recording long
// enough to pass four gigabytes forces on the mdat box.
func wideBox(name string, body []byte) []byte {
	out := make([]byte, 16, 16+len(body))
	binary.BigEndian.PutUint32(out, 1)
	copy(out[4:], name)
	binary.BigEndian.PutUint64(out[8:], uint64(16+len(body)))
	return append(out, body...)
}

func u32(v uint32) []byte {
	out := make([]byte, 4)
	binary.BigEndian.PutUint32(out, v)
	return out
}

// movieHeaderBox builds an mvhd of either version. timescale and duration are
// what a duration is made of; created is this format's 1904 epoch.
func movieHeaderBox(version byte, created, timescale, duration uint64) []byte {
	body := []byte{version, 0, 0, 0}
	if version == 1 {
		body = binary.BigEndian.AppendUint64(body, created)
		body = binary.BigEndian.AppendUint64(body, 0)
		body = binary.BigEndian.AppendUint32(body, uint32(timescale))
		body = binary.BigEndian.AppendUint64(body, duration)
	} else {
		body = binary.BigEndian.AppendUint32(body, uint32(created))
		body = binary.BigEndian.AppendUint32(body, 0)
		body = binary.BigEndian.AppendUint32(body, uint32(timescale))
		body = binary.BigEndian.AppendUint32(body, uint32(duration))
	}
	return boxOf("mvhd", body)
}

// trackHeaderBox builds a tkhd of either version, carrying one of the four
// display matrices.
func trackHeaderBox(version byte, a, b, c, d int32) []byte {
	body := []byte{version, 0, 0, 0}
	if version == 1 {
		body = append(body, zeros(32)...)
	} else {
		body = append(body, zeros(20)...)
	}
	body = append(body, zeros(16)...) // layer, group, volume and reserved

	matrix := []int32{a, b, 0, c, d, 0, 0, 0, 1 << 30}
	for _, v := range matrix {
		body = binary.BigEndian.AppendUint32(body, uint32(v))
	}
	body = append(body, zeros(8)...) // the display width and height
	return boxOf("tkhd", body)
}

func handlerBox(kind string) []byte {
	body := append(zeros(8), kind...)
	return boxOf("hdlr", append(body, zeros(12)...))
}

// zeros is padding, and it is a function so that appending it is appending to
// something empty -- which is what keeps the linter that watches for a slice
// grown past its own length quiet.
func zeros(n int) []byte { return make([]byte, n) }

// sampleDescriptionBox builds the stsd a video track carries: a count, and then
// one entry whose name is the codec and whose body holds the coded size.
func sampleDescriptionBox(format string, width, height uint16, short bool) []byte {
	entry := make([]byte, 0, 80)
	entry = append(entry, zeros(24)...) // the reference and the fields nothing reads
	entry = binary.BigEndian.AppendUint16(entry, width)
	entry = binary.BigEndian.AppendUint16(entry, height)
	entry = append(entry, zeros(50)...) // the depth, the name and the rest
	if short {
		entry = entry[:10]
	}

	body := append([]byte{0, 0, 0, 0}, u32(1)...)
	return boxOf("stsd", append(body, boxOf(format, entry)...))
}

// trackBox assembles the nest a sample description lives at the bottom of.
func trackBox(tkhd, hdlr, stsd []byte) []byte {
	stbl := boxOf("stbl", stsd)
	minf := boxOf("minf", stbl)
	mdia := boxOf("mdia", append(hdlr, minf...))
	return boxOf("trak", append(tkhd, mdia...))
}

// videoFile is a whole file: the brands, the frames, and the movie box behind
// them, which is where a camera puts it.
func videoFile(mvhd []byte, traks ...[]byte) []byte {
	moov := mvhd
	for _, trak := range traks {
		moov = append(moov, trak...)
	}
	file := boxOf("ftyp", []byte("isomiso2avc1mp41"))
	file = append(file, wideBox("mdat", bytes.Repeat([]byte{0}, 64))...)
	return append(file, boxOf("moov", moov)...)
}

func TestProbeVideoBoxShapes(t *testing.T) {
	t.Parallel()

	const created = 3_855_000_000 // some time in 2026, counted from 1904
	video := handlerBox("vide")
	stsd := sampleDescriptionBox("avc1", 1920, 1080, false)

	cases := []struct {
		name        string
		file        []byte
		wantOK      bool
		duration    int64
		orientation int
	}{
		{
			// The wide header is what a long recording forces, and it is a
			// different offset to every field behind it.
			name: "64-bit times and a half turn",
			file: videoFile(movieHeaderBox(1, created, 600, 1200),
				trackBox(trackHeaderBox(1, -1<<16, 0, 0, -1<<16), video, stsd)),
			wantOK: true, duration: 2000, orientation: 3,
		},
		{
			name: "a quarter turn the other way",
			file: videoFile(movieHeaderBox(0, created, 1000, 500),
				trackBox(trackHeaderBox(0, 0, -1<<16, 1<<16, 0), video, stsd)),
			wantOK: true, duration: 500, orientation: 8,
		},
		{
			// Nothing to divide by: a duration only a scan of the packets could
			// find, which is what ffprobe is for.
			name: "no timescale",
			file: videoFile(movieHeaderBox(0, created, 0, 500),
				trackBox(trackHeaderBox(0, 1<<16, 0, 0, 1<<16), video, stsd)),
		},
		{
			name: "a movie header that stops short",
			file: videoFile(boxOf("mvhd", []byte{0, 0, 0, 0, 1, 2}),
				trackBox(trackHeaderBox(0, 1<<16, 0, 0, 1<<16), video, stsd)),
		},
		{
			name: "no movie header at all",
			file: videoFile(nil, trackBox(trackHeaderBox(0, 1<<16, 0, 0, 1<<16), video, stsd)),
		},
		{
			// An .mp4 with only sound in it. The tkhd of a sound track has the
			// dimensions of nothing, so it must not be read as a video.
			name: "sound only",
			file: videoFile(movieHeaderBox(0, created, 1000, 1000),
				trackBox(trackHeaderBox(0, 1<<16, 0, 0, 1<<16), handlerBox("soun"), stsd)),
		},
		{
			name: "a track with no header",
			file: videoFile(movieHeaderBox(0, created, 1000, 1000),
				boxOf("trak", append(handlerBox("vide"),
					boxOf("mdia", append(handlerBox("vide"), boxOf("minf", boxOf("stbl", stsd))...))...))),
		},
		{
			name: "a track with no media at all",
			file: videoFile(movieHeaderBox(0, created, 1000, 1000),
				boxOf("trak", trackHeaderBox(0, 1<<16, 0, 0, 1<<16))),
		},
		{
			name: "a sample entry that stops halfway",
			file: videoFile(movieHeaderBox(0, created, 1000, 1000),
				trackBox(trackHeaderBox(0, 1<<16, 0, 0, 1<<16), video,
					sampleDescriptionBox("avc1", 1920, 1080, true))),
		},
		{
			name: "a sample entry with no size in it",
			file: videoFile(movieHeaderBox(0, created, 1000, 1000),
				trackBox(trackHeaderBox(0, 1<<16, 0, 0, 1<<16), video,
					sampleDescriptionBox("avc1", 0, 0, false))),
		},
		{
			name: "no sample description",
			file: videoFile(movieHeaderBox(0, created, 1000, 1000),
				trackBox(trackHeaderBox(0, 1<<16, 0, 0, 1<<16), video, boxOf("stts", nil))),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := probeVideo(bytes.NewReader(tc.file), int64(len(tc.file)))
			switch {
			case !tc.wantOK && err == nil:
				t.Fatalf("answered %+v instead of standing aside", got)
			case !tc.wantOK:
				return
			case err != nil:
				t.Fatalf("probeVideo: %v", err)
			}

			if got.DurationMS != tc.duration {
				t.Errorf("duration = %d, want %d", got.DurationMS, tc.duration)
			}
			if got.Orientation != tc.orientation {
				t.Errorf("orientation = %d, want %d", got.Orientation, tc.orientation)
			}
			if got.Width != 1920 || got.Height != 1080 {
				t.Errorf("%dx%d, want the coded size out of the sample entry", got.Width, got.Height)
			}
			if got.Codec != "h264" {
				t.Errorf("codec = %q", got.Codec)
			}
			if got.TakenAt.Year() != 2026 {
				t.Errorf("TakenAt = %v, want the 1904 epoch read as a date", got.TakenAt)
			}
		})
	}
}

// TestProbeVideoRefusesAnEnormousMoov: the movie box is read whole, so how big
// it may be is a decision rather than an accident. Nothing is read here beyond
// the header that claims the size.
func TestProbeVideoRefusesAnEnormousMoov(t *testing.T) {
	t.Parallel()

	header := make([]byte, 8)
	binary.BigEndian.PutUint32(header, maxMoov+1024)
	copy(header[4:], "moov")

	if _, err := probeVideo(bytes.NewReader(header), maxMoov*2); err == nil {
		t.Error("a movie box larger than the bound was read anyway")
	}
}

// TestProbeVideoWithNoTimeInIt: zero is what a file that never recorded a date
// carries, and 1904 is not a date to put in a photo library.
func TestProbeVideoWithNoTimeInIt(t *testing.T) {
	t.Parallel()

	file := videoFile(movieHeaderBox(0, 0, 1000, 1000),
		trackBox(trackHeaderBox(0, 1<<16, 0, 0, 1<<16), handlerBox("vide"),
			sampleDescriptionBox("avc1", 640, 480, false)))

	got, err := probeVideo(bytes.NewReader(file), int64(len(file)))
	if err != nil {
		t.Fatal(err)
	}
	if !got.TakenAt.IsZero() {
		t.Errorf("TakenAt = %v, want nothing", got.TakenAt)
	}
}

// TestTheReaderGivesUpQuietly walks the small refusals one by one, because
// each of them is a file that goes to ffprobe instead and none of them is a
// failure anybody should ever see.
func TestTheReaderGivesUpQuietly(t *testing.T) {
	t.Parallel()

	t.Run("a reader that cannot seek", func(t *testing.T) {
		t.Parallel()
		if _, err := probeVideo(stuckReader{}, 1024); err == nil {
			t.Error("a reader that refuses to seek was read anyway")
		}
	})

	t.Run("an empty movie box", func(t *testing.T) {
		t.Parallel()
		file := boxOf("moov", nil)
		if _, err := probeVideo(bytes.NewReader(file), int64(len(file))); err == nil {
			t.Error("a movie box with nothing in it answered")
		}
	})

	t.Run("a track with no handler", func(t *testing.T) {
		t.Parallel()
		if isVideoTrack(boxOf("mdia", boxOf("minf", nil))) {
			t.Error("a track with no handler was read as video")
		}
		if isVideoTrack(boxOf("mdia", boxOf("hdlr", []byte{0, 0, 0, 0}))) {
			t.Error("a handler too short to name anything was read as video")
		}
		if isVideoTrack(boxOf("tkhd", nil)) {
			t.Error("a track with no media was read as video")
		}
	})

	t.Run("a track header with no matrix", func(t *testing.T) {
		t.Parallel()
		if got := trackOrientation(nil); got != 0 {
			t.Errorf("orientation of nothing = %d", got)
		}
		if got := trackOrientation([]byte{0, 0, 0, 0, 1, 2, 3}); got != 0 {
			t.Errorf("orientation of a truncated header = %d", got)
		}
	})
}

// stuckReader is a file the storage layer opened and then could not move
// around in, which is a broken backend rather than a broken file.
type stuckReader struct{}

func (stuckReader) Read([]byte) (int, error) { return 0, errNotISOBMFF }

func (stuckReader) Seek(int64, int) (int64, error) { return 0, errNotISOBMFF }

// TestAtomsStopAtNonsense: a size that runs past the end of the buffer, and one
// that says "to the end". Neither may read outside what it was given.
func TestAtomsStopAtNonsense(t *testing.T) {
	t.Parallel()

	toTheEnd := append(u32(0), []byte("free")...)
	toTheEnd = append(toTheEnd, []byte("payload")...)
	var names []string
	for name := range atoms(toTheEnd) {
		names = append(names, name)
	}
	if len(names) != 1 || names[0] != "free" {
		t.Errorf("boxes = %v, want the one that runs to the end", names)
	}

	tooLong := append(u32(4096), []byte("moov")...)
	for name := range atoms(tooLong) {
		t.Errorf("a box claiming 4096 bytes in a %d-byte buffer was yielded as %q", len(tooLong), name)
	}

	// A 64-bit length with nothing behind it, and a length smaller than the
	// header that declared it. Both are how a malformed file ends a walk.
	truncated := append(u32(1), []byte("mdat")...)
	for name := range atoms(append(truncated, 0, 0, 0, 0)) {
		t.Errorf("a truncated wide header was yielded as %q", name)
	}
	impossible := append(u32(4), []byte("free")...)
	for name := range atoms(impossible) {
		t.Errorf("a box shorter than its own header was yielded as %q", name)
	}

	if _, ok := box(toTheEnd, "free", "nope"); ok {
		t.Error("a path that does not exist came back")
	}
}
