package media

import "testing"

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
