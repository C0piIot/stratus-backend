package media

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/C0piIot/stratus-backend/internal/db"
)

// sonos is the ClientInfo the specification gives as its example.
var sonos = Capabilities{
	MaxBitrate:          512_000,
	MaxTranscodeBitrate: 256_000,
	Direct: []DirectProfile{
		{Containers: []string{"mp3"}, Codecs: []string{"mp3"}, Protocols: []string{"http"}, MaxChannels: 2},
		{Containers: []string{"flac"}, Codecs: []string{"flac"}, MaxChannels: 2},
		{Containers: []string{"mp4"}, Codecs: []string{"flac", "aac", "alac"}, MaxChannels: 2},
	},
	Targets: []TargetProfile{
		{Container: "mp3", Codec: "mp3", Protocol: "http", MaxChannels: 2},
		{Container: "flac", Codec: "flac", Protocol: "hls", MaxChannels: 2},
	},
	Codecs: []CodecProfile{
		{Codec: "mp3", Limits: []Limit{{Name: "audioBitrate", Comparison: "LessThanEqual", Values: []string{"320000"}, Required: true}}},
		{Codec: "flac", Limits: []Limit{
			{Name: "audioSamplerate", Comparison: "LessThanEqual", Values: []string{"192000"}},
			{Name: "audioChannels", Comparison: "Equals", Values: []string{"1", "2"}},
		}},
	},
}

func TestDecideForTheSpecificationsClient(t *testing.T) {
	t.Parallel()

	cd := db.Media{Codec: "flac", Bitrate: 900_000, SampleRate: 44_100, Channels: 2, BitDepth: 16}
	d := DecideFor(db.File{Path: "a/01.flac"}, cd, sonos)
	if d.Direct || !d.Transcode {
		t.Fatalf("a 900 kbps FLAC against a 512 kbps limit = %+v, want a transcode", d)
	}
	want := Plan{Format: "mp3", Encoder: "libmp3lame", Muxer: "mp3", MIME: "audio/mpeg", Container: "mp3", Bitrate: 256_000}
	if d.Plan != want {
		t.Errorf("plan = %+v\nwant   %+v", d.Plan, want)
	}
	if d.Target != (Stream{Protocol: "http", Container: "mp3", Codec: "mp3", Channels: 2, Bitrate: 256_000, SampleRate: 44_100}) {
		t.Errorf("target = %+v", d.Target)
	}
	if d.Source.Container != "flac" || d.Source.Bitrate != 900_000 || len(d.Reasons) != 1 {
		t.Errorf("source = %+v, reasons = %q", d.Source, d.Reasons)
	}

	// The specification's own source: six channels at 96 kHz, 24 bits.
	surround := db.Media{Codec: "flac", Bitrate: 3_000_000, SampleRate: 96_000, Channels: 6, BitDepth: 24}
	d = DecideFor(db.File{Path: "a/02.flac"}, surround, sonos)
	if d.Plan.Channels != 2 || d.Plan.SampleRate != 48_000 || d.Plan.Bitrate != 256_000 {
		t.Errorf("5.1 at 96 kHz = %+v, want stereo at 48 kHz and 256 kbps", d.Plan)
	}

	for name, c := range map[string]struct {
		path string
		m    db.Media
	}{
		"an mp3 within its codec's limit": {"a/03.mp3", db.Media{Codec: "mp3", Bitrate: 320_000, Channels: 2}},
		"an m4a, which is an mp4":         {"a/04.m4a", db.Media{Codec: "aac", Bitrate: 256_000, Channels: 2}},
		// The flac limits are not required, so a 6-channel FLAC under the
		// bitrate is refused only by the profile's own channel count.
		"a FLAC at a rate over a limit that is not required": {"a/05.flac",
			db.Media{Codec: "flac", Bitrate: 400_000, SampleRate: 384_000, Channels: 2}},
	} {
		if got := DecideFor(db.File{Path: c.path}, c.m, sonos); !got.Direct || got.Plan != (Plan{Direct: true}) {
			t.Errorf("%s: %+v, want direct play", name, got)
		}
	}
}

func TestDecideFor(t *testing.T) {
	t.Parallel()
	flac := db.Media{Codec: "flac", Bitrate: 900_000, SampleRate: 44_100, Channels: 2, BitDepth: 16}
	hires := db.Media{Codec: "flac", Bitrate: 2_300_000, SampleRate: 96_000, Channels: 2, BitDepth: 24}
	mp3 := db.Media{Codec: "mp3", Bitrate: 320_000, SampleRate: 44_100, Channels: 2}

	for name, c := range map[string]struct {
		path string
		m    db.Media
		caps Capabilities
		want Plan
	}{
		"aac in an mp4 goes out as fragments": {"a.flac", flac,
			Capabilities{MaxBitrate: 192_000, Targets: []TargetProfile{{Container: "mp4", Codec: "aac"}}},
			Plan{Format: "aac", Encoder: "aac", Muxer: "ipod", MIME: "audio/mp4", Container: "mp4", Fragmented: true, Bitrate: 192_000}},
		"opus of a CD rip is 48 kHz": {"a.flac", flac,
			Capabilities{MaxBitrate: 128_000, Targets: []TargetProfile{{Container: "ogg", Codec: "opus"}}},
			Plan{Format: "opus", Encoder: "libopus", Muxer: "ogg", MIME: "audio/ogg", Container: "ogg", Bitrate: 128_000, SampleRate: 48_000}},
		"a lossy source keeps its own bitrate as the ceiling": {"a.mp3", mp3,
			Capabilities{Direct: []DirectProfile{{Codecs: []string{"flac"}}}, Targets: []TargetProfile{{Codec: "opus"}}},
			Plan{Format: "opus", Encoder: "libopus", Muxer: "ogg", MIME: "audio/ogg", Container: "ogg", Bitrate: 256_000, SampleRate: 48_000}},
		"hi-res FLAC brought down to sixteen bits": {"a.flac", hires,
			Capabilities{
				Direct:  []DirectProfile{{Codecs: []string{"flac"}, Containers: []string{"flac"}}},
				Targets: []TargetProfile{{Codec: "flac"}},
				Codecs:  []CodecProfile{{Codec: "flac", Limits: []Limit{{Name: "audioBitdepth", Comparison: "LessThanEqual", Values: []string{"16"}, Required: true}}}},
			},
			Plan{Format: "flac", Encoder: "flac", Muxer: "flac", MIME: "audio/flac", Container: "flac", BitDepth: 16}},
		"a sample-rate limit picks a rate the codec makes": {"a.flac", hires,
			Capabilities{MaxBitrate: 320_000, Targets: []TargetProfile{{Codec: "mp3"}},
				Codecs: []CodecProfile{{Codec: "mp3", Limits: []Limit{{Name: "audioSamplerate", Comparison: "LessThanEqual", Values: []string{"45000"}}}}}},
			Plan{Format: "mp3", Encoder: "libmp3lame", Muxer: "mp3", MIME: "audio/mpeg", Container: "mp3", Bitrate: 320_000, SampleRate: 44_100}},
		"a target that can only be met by raising is skipped": {"a.flac", flac,
			Capabilities{MaxBitrate: 128_000, Targets: []TargetProfile{{Codec: "opus"}, {Codec: "mp3"}},
				Codecs: []CodecProfile{{Codec: "opus", Limits: []Limit{{Name: "audioChannels", Comparison: "GreaterThanEqual", Values: []string{"6"}}}}}},
			Plan{Format: "mp3", Encoder: "libmp3lame", Muxer: "mp3", MIME: "audio/mpeg", Container: "mp3", Bitrate: 128_000}},
		"HLS and a codec nothing makes are skipped": {"a.flac", flac,
			Capabilities{MaxBitrate: 128_000, Targets: []TargetProfile{{Codec: "aac", Protocol: "hls"}, {Codec: "vorbis"}, {Container: "webm", Codec: "opus"}, {Codec: "mp3"}}},
			Plan{Format: "mp3", Encoder: "libmp3lame", Muxer: "mp3", MIME: "audio/mpeg", Container: "mp3", Bitrate: 128_000}},
	} {
		d := DecideFor(db.File{Path: c.path}, c.m, c.caps)
		if !d.Transcode || d.Plan != c.want {
			t.Errorf("%s:\n got  %+v\n want %+v", name, d.Plan, c.want)
		}
	}
}

func TestDecideForTheRest(t *testing.T) {
	t.Parallel()
	mp3 := db.Media{Codec: "mp3", Bitrate: 320_000, Channels: 2}

	// Nothing fits and nothing can be made: a lossy file for a client that
	// only takes FLAC.
	d := DecideFor(db.File{Path: "a.mp3"}, mp3, Capabilities{
		Direct:  []DirectProfile{{Codecs: []string{"flac"}}},
		Targets: []TargetProfile{{Codec: "flac"}},
	})
	if d.Direct || d.Transcode || d.Error == "" || !strings.Contains(d.Reasons[0], "mp3") {
		t.Errorf("no way to play it = %+v, want an error and a reason", d)
	}

	// A direct profile only over HLS is not one this server serves.
	d = DecideFor(db.File{Path: "a.mp3"}, mp3, Capabilities{Direct: []DirectProfile{{Protocols: []string{"hls"}}}})
	if d.Direct || len(d.Reasons) != 1 || d.Reasons[0] != "protocol not supported" {
		t.Errorf("an HLS-only profile = %+v", d)
	}

	// A WAV is PCM, and a client calls it wav.
	wav := db.Media{Codec: "pcm_s16le", Bitrate: 1_411_200, Channels: 2}
	if got := DecideFor(db.File{Path: "a.wav"}, wav, Capabilities{Direct: []DirectProfile{{Codecs: []string{"wav"}}}}); !got.Direct {
		t.Errorf("a WAV for a client that plays wav = %+v", got)
	}

	// A required limit that is not met stops direct play; the profile's
	// reason is the limit's name.
	d = DecideFor(db.File{Path: "a.mp3"}, mp3, Capabilities{
		Direct: []DirectProfile{{}},
		Codecs: []CodecProfile{{Codec: "mp3", Limits: []Limit{{Name: "audioBitrate", Comparison: "LessThanEqual", Values: []string{"128000"}, Required: true}}}},
	})
	if d.Direct || d.Reasons[0] != "audioBitrate not supported" {
		t.Errorf("a required limit = %+v", d)
	}
}

func TestLimits(t *testing.T) {
	t.Parallel()
	lim := func(cmp string, values ...string) Limit { return Limit{Comparison: cmp, Values: values} }
	for name, c := range map[string]struct {
		v       int
		l       Limit
		allowed []int
		want    int
		ok      bool
	}{
		"already under":                  {128, lim("LessThanEqual", "320"), nil, 128, true},
		"brought down":                   {320, lim("LessThanEqual", "128"), nil, 128, true},
		"down to what the codec makes":   {96_000, lim("LessThanEqual", "45000"), []int{48_000, 44_100}, 44_100, true},
		"equals picks the highest below": {6, lim("Equals", "1", "2"), nil, 2, true},
		"equals with nothing below":      {1, lim("Equals", "2", "6"), nil, 1, false},
		"greater than cannot be met":     {2, lim("GreaterThanEqual", "6"), nil, 2, false},
		"not equals cannot be met":       {2, lim("NotEquals", "2"), nil, 2, false},
		"unknown is let through":         {0, lim("LessThanEqual", "1"), nil, 0, true},
		"a value that is not a number":   {6, lim("LessThanEqual", "many"), nil, 6, true},
	} {
		if got, ok := lower(c.v, c.l, c.allowed); got != c.want || ok != c.ok {
			t.Errorf("%s: lower = %d, %v; want %d, %v", name, got, ok, c.want, c.ok)
		}
	}
	for _, c := range []struct {
		v    string
		l    Limit
		want bool
	}{
		{"LC", lim("Equals", "lc", "he-aac"), true},
		{"HE-AAC", lim("NotEquals", "HE-AAC"), false},
		{"", lim("Equals", "LC"), true},
		{"LC", lim("LessThanEqual", "x"), true},
	} {
		if got := compareText(c.v, c.l); got != c.want {
			t.Errorf("compareText(%q, %+v) = %v", c.v, c.l, got)
		}
	}
}

func TestContainerOf(t *testing.T) {
	t.Parallel()
	for p, want := range map[string]string{
		"a/b.FLAC": "flac", "x.m4a": "mp4", "x.opus": "ogg", "x.aif": "aiff", "x.wma": "asf", "x.mp3": "mp3", "x": "",
	} {
		if got := containerOf(p); got != want {
			t.Errorf("containerOf(%q) = %q, want %q", p, got, want)
		}
	}
}

// TestParamsRoundTrip: what the decision writes, the stream reads back as the
// same Plan -- and nothing else is read at all.
func TestParamsRoundTrip(t *testing.T) {
	t.Parallel()
	for _, p := range []Plan{
		{Format: "mp3", Encoder: "libmp3lame", Muxer: "mp3", MIME: "audio/mpeg", Container: "mp3", Bitrate: 128_000, Channels: 2},
		{Format: "aac", Encoder: "aac", Muxer: "ipod", MIME: "audio/mp4", Container: "mp4", Fragmented: true, Bitrate: 192_000, SampleRate: 48_000},
		{Format: "flac", Encoder: "flac", Muxer: "flac", MIME: "audio/flac", Container: "flac", BitDepth: 16},
	} {
		got, err := PlanFromParams(p.Params())
		if err != nil || got != p {
			t.Errorf("%s read back as %+v, %v", p.Params(), got, err)
		}
	}

	for _, s := range []string{
		"", "v2-mp3-mp3-128000-0-2-0", "v1-mp3-mp3-128000-0-2", "v1-vorbis-ogg-128000-0-2-0",
		"v1-mp3-mp4-128000-0-2-0", "v1-mp3-mp3-999000-0-2-0", "v1-mp3-mp3-128000-12345-2-0",
		"v1-mp3-mp3-128000-0-6-0", "v1-mp3-mp3-128000-0-2-16", "v1-flac-flac-128000-0-2-0",
		"v1-flac-flac-0-0-2-24", "v1-mp3-mp3-x-0-2-0", "v1-mp3-mp3--1-0-2-0",
	} {
		if _, err := PlanFromParams(s); !errors.Is(err, ErrBadParams) {
			t.Errorf("PlanFromParams(%q) = %v, want ErrBadParams", s, err)
		}
	}
	if !slices.Contains(targets["mp3"].rates, 44_100) {
		t.Fatal("the rate table this test assumes has moved")
	}
}
