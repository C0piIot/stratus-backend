package media

import (
	"encoding/binary"
	"math"
	"testing"

	"github.com/C0piIot/stratus-backend/internal/db"
)

// The configuration records the fixtures in testdata do not reach, built by
// hand from their specifications. Each comment spells out the bits, because a
// test of a bit reader whose bytes nobody can read back is a test of nothing.

func TestAVCConf(t *testing.T) {
	t.Parallel()

	// High 10, level 4.0, one two-byte SPS and one one-byte PPS, then the
	// extension: chroma_format 1, luma depth 8+2, chroma depth 8+2, no SPS ext.
	high10 := []byte{1, 110, 0, 40, 0xff, 0xe1, 0, 2, 0xaa, 0xbb, 1, 0, 1, 0xcc, 0xfd, 0xfa, 0xfa, 0}
	c, ok := avcConf(high10)
	if !ok || c != (streamConf{profile: "High 10", level: 40, depth: 10}) {
		t.Errorf("High 10 = %+v, %v", c, ok)
	}

	// The same record cut off before its extension: the depth is not stated,
	// so it is not known.
	c, ok = avcConf(high10[:14])
	if !ok || c.depth != 0 {
		t.Errorf("High 10 without its extension = %+v, %v; want an unknown depth", c, ok)
	}

	// Baseline with constraint_set1 is Constrained Baseline, and eight bits
	// without asking.
	c, ok = avcConf([]byte{1, 66, 0x40, 30, 0xff, 0xe0, 0})
	if !ok || c != (streamConf{profile: "Constrained Baseline", level: 30, depth: 8}) {
		t.Errorf("Constrained Baseline = %+v, %v", c, ok)
	}

	if _, ok := avcConf([]byte{0, 100, 0, 40, 0xff, 0xe0, 0}); ok {
		t.Error("a record whose version is not 1 was read")
	}
}

func TestHEVCConf(t *testing.T) {
	t.Parallel()

	// Main 10 at level 4 (120), depth 8+2 in byte 17.
	rec := make([]byte, 23)
	rec[0], rec[1], rec[12], rec[17] = 1, 0x02, 120, 0xfa
	c, ok := hevcConf(rec)
	if !ok || c != (streamConf{profile: "Main 10", level: 120, depth: 10}) {
		t.Errorf("Main 10 = %+v, %v", c, ok)
	}
}

func TestVP9Conf(t *testing.T) {
	t.Parallel()

	// Profile 2, level 31 (not taken: ffprobe reports none), ten bits and 4:2:0.
	c, ok := vp9Conf([]byte{2, 31, 0xa2})
	if !ok || c != (streamConf{profile: "Profile 2", depth: 10}) {
		t.Errorf("vpcC = %+v, %v", c, ok)
	}

	// Matroska's feature list: profile 1, then depth 12.
	c, ok = vp9Private([]byte{1, 1, 1, 3, 1, 12})
	if !ok || c != (streamConf{profile: "Profile 1", depth: 12}) {
		t.Errorf("CodecPrivate = %+v, %v", c, ok)
	}
	if _, ok := vp9Private(nil); ok {
		t.Error("an empty CodecPrivate -- which is most of them -- was read as a profile")
	}
}

func TestAV1Conf(t *testing.T) {
	t.Parallel()

	// Marker and version, then profile 0 and level 8, then high_bitdepth set
	// and twelve_bit clear: Main, ten bits.
	c, ok := av1Conf([]byte{0x81, 0x08, 0x40, 0})
	if !ok || c != (streamConf{profile: "Main", level: 8, depth: 10}) {
		t.Errorf("av1C = %+v, %v", c, ok)
	}
	c, ok = av1Conf([]byte{0x81, 0x40 | 0x09, 0x60, 0})
	if !ok || c != (streamConf{profile: "Professional", level: 9, depth: 12}) {
		t.Errorf("av1C = %+v, %v", c, ok)
	}
}

func TestAACChannels(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		asc  []byte
		want int
	}{
		// object type 2, frequency index 3, channel configuration 6
		"lc 5.1": {[]byte{0x11, 0xb0}, 6},
		// configuration 7 is eight channels, not seven
		"lc 7.1": {[]byte{0x11, 0xb8}, 8},
		// an escaped object type (31, then 6 more bits), index 3, stereo
		"escaped": {[]byte{0xf9, 0x46, 0x40}, 2},
		// configuration 0: the channels are in a program config element
		"pce":       {[]byte{0x11, 0x80}, 0},
		"too short": {[]byte{0x11}, 0},
	} {
		if got := aacChannels(c.asc); got != c.want {
			t.Errorf("%s: %d channels, want %d", name, got, c.want)
		}
	}
}

func TestAC3Channels(t *testing.T) {
	t.Parallel()

	// dac3: fscod 0, bsid 8, bsmod 0, acmod 7 (3/2), lfeon 1 -- 5.1.
	if got := ac3Channels([]byte{0x10, 0x3c, 0x00}); got != 6 {
		t.Errorf("dac3 5.1 = %d channels", got)
	}

	// dec3: one independent substream -- fscod 0, bsid 16, acmod 7, lfeon 1,
	// no dependents -- is 5.1 too.
	if got := eac3Channels([]byte{0, 0, 0x20, 0x0f, 0x00}); got != 6 {
		t.Errorf("dec3 5.1 = %d channels", got)
	}
	// A dependent substream adds channels this does not count, so the answer
	// is unknown rather than short.
	if got := eac3Channels([]byte{0, 0, 0x20, 0x0f, 0x02}); got != 0 {
		t.Errorf("dec3 with a dependent substream = %d channels, want unknown", got)
	}
}

// soundTrak builds a sound track holding one sample entry, which is all
// soundTrack reads.
func soundTrak(format string, entry []byte) []byte {
	hdlr := append(make([]byte, 8), []byte("soun")...)
	stsd := append([]byte{0, 0, 0, 0, 0, 0, 0, 1}, boxOf(format, entry)...)
	return boxOf("mdia", append(boxOf("hdlr", hdlr),
		boxOf("minf", boxOf("stbl", boxOf("stsd", stsd)))...))
}

// audioEntry is the fixed part of an AudioSampleEntry at version 0 -- channel
// count at 16, rate in the integer half of a 16.16 at 24 -- followed by its
// child boxes.
func audioEntry(channels, rate uint16, children ...[]byte) []byte {
	var fixed [28]byte
	binary.BigEndian.PutUint16(fixed[16:], channels)
	binary.BigEndian.PutUint16(fixed[24:], rate)
	e := fixed[:]
	for _, c := range children {
		e = append(e, c...)
	}
	return e
}

// TestSoundTrack is each sound entry an MP4 or a QuickTime file carries, and
// the box inside it that is the authority on channels where the entry's own
// count is not.
func TestSoundTrack(t *testing.T) {
	t.Parallel()

	// esds: ES_Descriptor with the URL flag and a four-byte URL to step over,
	// then a DecoderConfigDescriptor naming MP3 (0x6b), with no decoder info.
	esds := append([]byte{0, 0, 0, 0}, 0x03, 23, 0, 1, 0x40, 4, 'a', 'b', 'c', 'd',
		0x04, 13, 0x6b, 0x15, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0)

	// A QuickTime version 2 entry: the rate as a float64 at 32 and the
	// channel count as a uint32 at 40, the old fields left at their defaults.
	v2 := make([]byte, 64)
	binary.BigEndian.PutUint16(v2[8:], 2)
	binary.BigEndian.PutUint64(v2[32:], math.Float64bits(96_000))
	binary.BigEndian.PutUint32(v2[40:], 2)

	// Version 1 adds sixteen bytes before the children.
	v1 := audioEntry(2, 44_100)
	binary.BigEndian.PutUint16(v1[8:], 1)
	v1 = append(v1, make([]byte, 16)...)
	v1 = append(v1, boxOf("esds", esds)...)

	for name, c := range map[string]struct {
		trak []byte
		want db.Media
	}{
		"ac-3 counts from dac3": {
			soundTrak("ac-3", audioEntry(2, 48_000, boxOf("dac3", []byte{0x10, 0x3c, 0x00}))),
			db.Media{AudioCodec: "ac3", Channels: 6, SampleRate: 48_000},
		},
		"e-ac-3 counts from dec3": {
			soundTrak("ec-3", audioEntry(2, 48_000, boxOf("dec3", []byte{0, 0, 0x20, 0x0f, 0x00}))),
			db.Media{AudioCodec: "eac3", Channels: 6, SampleRate: 48_000},
		},
		"e-ac-3 with no dec3 is unknown channels": {
			soundTrak("ec-3", audioEntry(2, 48_000)),
			db.Media{AudioCodec: "eac3", SampleRate: 48_000},
		},
		"opus counts from dOps": {
			soundTrak("Opus", audioEntry(2, 48_000, boxOf("dOps", []byte{0, 6, 0, 0}))),
			db.Media{AudioCodec: "opus", Channels: 6, SampleRate: 48_000},
		},
		"mp3 inside mp4a, in a version 1 entry": {
			soundTrak("mp4a", v1),
			db.Media{AudioCodec: "mp3", Channels: 2, SampleRate: 44_100},
		},
		"a version 2 entry": {
			soundTrak("fLaC", v2),
			db.Media{AudioCodec: "flac", Channels: 2, SampleRate: 96_000},
		},
		"a format this does not name": {
			soundTrak("sowt", audioEntry(2, 44_100)),
			db.Media{},
		},
		"mp4a with no esds": {
			soundTrak("mp4a", audioEntry(2, 44_100)),
			db.Media{},
		},
		"an entry too short to hold its fields": {
			soundTrak("alac", make([]byte, 12)),
			db.Media{},
		},
		"a version 2 entry cut short": {
			soundTrak("fLaC", v2[:40]),
			db.Media{},
		},
		"no sample table": {
			boxOf("mdia", nil),
			db.Media{},
		},
	} {
		var m db.Media
		soundTrack(c.trak, &m)
		if m != c.want {
			t.Errorf("%s: got %+v, want %+v", name, m, c.want)
		}
	}
}

// TestPictureConf is the configuration boxes the fixtures do not carry, as
// they sit after a VisualSampleEntry's fixed part.
func TestPictureConf(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		children []byte
		want     db.Media
	}{
		"vpcC, a full box": {
			boxOf("vpcC", []byte{1, 0, 0, 0, 2, 31, 0xa2}),
			db.Media{CodecProfile: "Profile 2", BitDepth: 10},
		},
		"av1C": {
			boxOf("av1C", []byte{0x81, 0x28, 0x00, 0}),
			db.Media{CodecProfile: "High", Level: 8, BitDepth: 8},
		},
		"a pasp before the record is stepped over": {
			append(boxOf("pasp", make([]byte, 8)), boxOf("av1C", []byte{0x81, 0x08, 0x40, 0})...),
			db.Media{CodecProfile: "Main", Level: 8, BitDepth: 10},
		},
		"a record that does not parse says nothing": {
			boxOf("hvcC", []byte{1, 2, 3}),
			db.Media{},
		},
		"no record at all": {
			boxOf("pasp", make([]byte, 8)),
			db.Media{},
		},
	} {
		var m db.Media
		pictureConf(c.children, &m)
		if m != c.want {
			t.Errorf("%s: got %+v, want %+v", name, m, c.want)
		}
	}
}

// TestFrameRate reads mdhd at both of its versions, and gives up on a track
// that does not state what it needs.
func TestFrameRate(t *testing.T) {
	t.Parallel()

	stts := boxOf("stts", []byte{0, 0, 0, 0, 0, 0, 0, 1, 0, 0, 1, 0x2c, 0, 0, 0x03, 0xe9}) // 300 samples
	trak := func(mdhd []byte, stts []byte) []byte {
		return boxOf("mdia", append(boxOf("mdhd", mdhd), boxOf("minf", boxOf("stbl", stts))...))
	}

	// Version 1: timescale 30000 at 20, duration 300 * 1001 at 24.
	v1 := make([]byte, 32)
	v1[0] = 1
	binary.BigEndian.PutUint32(v1[20:], 30_000)
	binary.BigEndian.PutUint64(v1[24:], 300*1001)
	if got := frameRate(trak(v1, stts)); got != 29_970 {
		t.Errorf("version 1 = %d, want 29970", got)
	}

	v0 := make([]byte, 20)
	binary.BigEndian.PutUint32(v0[12:], 30_000)
	if got := frameRate(trak(v0, stts)); got != 0 {
		t.Errorf("a zero duration = %d, want unknown", got)
	}
	binary.BigEndian.PutUint32(v0[16:], 300*1001)
	if got := frameRate(trak(v0, nil)); got != 0 {
		t.Errorf("no stts = %d, want unknown", got)
	}
	if got := frameRate(trak(v0, boxOf("stts", []byte{0, 0, 0, 0, 0, 0, 0, 2, 0, 0, 0, 1}))); got != 0 {
		t.Errorf("an stts shorter than its count = %d, want unknown", got)
	}
	if got := frameRate(boxOf("mdia", nil)); got != 0 {
		t.Errorf("no mdhd = %d, want unknown", got)
	}
}

// TestPrivateConf is Matroska's side: the same records, found in CodecPrivate
// by the codec the track names.
func TestPrivateConf(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		codec   string
		private []byte
		want    db.Media
	}{
		"av1":                         {"av1", []byte{0x81, 0x08, 0x40, 0}, db.Media{CodecProfile: "Main", Level: 8, BitDepth: 10}},
		"vp9 with a feature list":     {"vp9", []byte{1, 1, 0, 3, 1, 8}, db.Media{CodecProfile: "Profile 0", BitDepth: 8}},
		"vp9 with a malformed list":   {"vp9", []byte{1, 2, 0}, db.Media{}},
		"a codec with no record read": {"mpeg4", []byte{1, 2, 3}, db.Media{}},
	} {
		var m db.Media
		privateConf(c.codec, c.private, &m)
		if m != c.want {
			t.Errorf("%s: got %+v, want %+v", name, m, c.want)
		}
	}

	for id, want := range map[string]string{
		"A_AAC": "aac", "A_AAC/MPEG4/LC/SBR": "aac", "A_DTS": "dts", "A_TRUEHD": "truehd", "A_PCM/INT/LIT": "",
	} {
		if got := audioCodecOf(id); got != want {
			t.Errorf("audioCodecOf(%q) = %q, want %q", id, got, want)
		}
	}
}

// TestShortRecords: a record too short to be one is not read as anything.
func TestShortRecords(t *testing.T) {
	t.Parallel()
	if _, ok := hevcConf([]byte{1, 2}); ok {
		t.Error("a two-byte hvcC was read")
	}
	if _, ok := vp9Conf([]byte{1}); ok {
		t.Error("a one-byte vpcC was read")
	}
	if _, ok := av1Conf([]byte{0x80, 0, 0, 0}); ok {
		t.Error("an av1C with the wrong marker was read")
	}
	if got := ac3Channels([]byte{1}); got != 0 {
		t.Errorf("a one-byte dac3 = %d channels", got)
	}
	if got := eac3Channels([]byte{0, 1, 0, 0, 0}); got != 0 {
		t.Errorf("two independent substreams = %d channels, want unknown", got)
	}
	if tag, _, _ := descriptor([]byte{0x03, 0x10, 0}); tag != 0 {
		t.Error("a descriptor longer than its buffer was read")
	}
}
