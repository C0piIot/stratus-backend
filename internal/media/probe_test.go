package media

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/C0piIot/stratus-backend/internal/db"
)

// videoReport is what ffprobe actually prints for a video off a phone,
// trimmed to the fields this package reads. Captured rather than invented:
// running ffprobe is the smoke test's job, interpreting it is this one's.
const videoReport = `{
  "streams": [
    {"codec_type": "video", "codec_name": "h264", "width": 1920, "height": 1080,
     "duration": "12.345000", "tags": {"rotate": "90", "language": "und"}},
    {"codec_type": "audio", "codec_name": "aac", "duration": "12.400000"}
  ],
  "format": {
    "duration": "12.400000",
    "tags": {"creation_time": "2024-06-01T12:30:15.000000Z", "com.apple.quicktime.model": "iPhone 15 Pro"}
  }
}`

const audioReport = `{
  "streams": [{"codec_type": "audio", "codec_name": "flac", "duration": "254.120000"}],
  "format": {
    "duration": "254.120000",
    "tags": {"TITLE": "Hunter", "ARTIST": "Björk", "ALBUM": "Homogenic",
             "track": "1/10", "disc": "1/1", "DATE": "1997-09-22", "GENRE": "Electronic"}
  }
}`

func TestProbeReportVideo(t *testing.T) {
	t.Parallel()
	m := parse(t, videoReport).mediaFrom(db.KindVideo)

	if m.Codec != "h264" {
		t.Errorf("Codec = %q", m.Codec)
	}
	if m.Width != 1920 || m.Height != 1080 {
		t.Errorf("dimensions = %dx%d", m.Width, m.Height)
	}
	if m.DurationMS != 12_400 {
		t.Errorf("DurationMS = %d, want 12400", m.DurationMS)
	}
	// A phone records rotated and says so in a tag; without this every portrait
	// video plays on its side.
	if m.Orientation != 6 {
		t.Errorf("Orientation = %d, want 6 for a 90 degree rotation", m.Orientation)
	}
	want := time.Date(2024, 6, 1, 12, 30, 15, 0, time.UTC)
	if !m.TakenAt.Equal(want) {
		t.Errorf("TakenAt = %v, want %v", m.TakenAt, want)
	}
}

// TestProbeReportRotationAsSideData is the same phone video as above, printed
// by a modern ffprobe: no rotate tag at all, and a display matrix in the side
// data whose angle runs counter-clockwise. Captured from ffprobe 8, which is
// how the shipped 7 prints it too -- the tag went away in 7, so without this
// every portrait video had an orientation of none.
func TestProbeReportRotationAsSideData(t *testing.T) {
	t.Parallel()

	const report = `{
  "streams": [
    {"codec_type": "video", "codec_name": "h264", "width": 1920, "height": 1080,
     "duration": "12.345000", "tags": {"language": "und"},
     "side_data_list": [{"side_data_type": "DisplayMatrix", "rotation": -90}]}
  ],
  "format": {"duration": "12.345000"}
}`

	m := parse(t, report).mediaFrom(db.KindVideo)
	if m.Orientation != 6 {
		t.Errorf("Orientation = %d, want 6: a rotation of -90 counter-clockwise is a quarter turn clockwise",
			m.Orientation)
	}
}

func TestProbeReportAudio(t *testing.T) {
	t.Parallel()
	m := parse(t, audioReport).mediaFrom(db.KindAudio)

	if m.Codec != "flac" || m.DurationMS != 254_120 {
		t.Errorf("got codec %q duration %d", m.Codec, m.DurationMS)
	}
	// Matroska and FLAC write their tags in upper case and MP4 in lower, so the
	// lookup cannot care.
	if m.Title != "Hunter" || m.Artist != "Björk" || m.Album != "Homogenic" {
		t.Errorf("got %+v", m)
	}
	if m.TrackNo != 1 || m.DiscNo != 1 {
		t.Errorf("track %d disc %d, want the number before the slash", m.TrackNo, m.DiscNo)
	}
	if m.Year != 1997 {
		t.Errorf("Year = %d, want the year out of a full date", m.Year)
	}
	if m.Genre != "Electronic" {
		t.Errorf("Genre = %q", m.Genre)
	}
	// An audio file has no dimensions, and inventing them would be worse than
	// leaving them at zero.
	if m.Width != 0 || m.Height != 0 {
		t.Errorf("dimensions = %dx%d on an audio file", m.Width, m.Height)
	}
}

func TestProbeReportEdges(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		report string
		check  func(t *testing.T, m db.Media)
	}{
		{
			name:   "no tags at all",
			report: `{"streams":[{"codec_type":"audio","codec_name":"mp3"}],"format":{"duration":"1.0"}}`,
			check: func(t *testing.T, m db.Media) {
				if m.Artist != "" || m.Year != 0 || m.DurationMS != 1000 {
					t.Errorf("got %+v", m)
				}
			},
		},
		{
			name:   "duration not available",
			report: `{"streams":[{"codec_type":"video","codec_name":"h264","duration":"5.5"}],"format":{"duration":"N/A"}}`,
			check: func(t *testing.T, m db.Media) {
				// Matroska often has no duration in the container, only on the
				// stream.
				if m.DurationMS != 5500 {
					t.Errorf("DurationMS = %d, want it taken from the stream", m.DurationMS)
				}
			},
		},
		{
			name:   "album artist stands in for artist",
			report: `{"streams":[],"format":{"tags":{"album_artist":"Various"}}}`,
			check: func(t *testing.T, m db.Media) {
				if m.Artist != "Various" {
					t.Errorf("Artist = %q", m.Artist)
				}
			},
		},
		{
			// The pair that makes a compilation one album instead of twelve.
			name: "a compilation keeps both artists apart",
			report: `{"streams":[],"format":{"tags":{
				"artist":"Boards of Canada","album_artist":"Various Artists"}}}`,
			check: func(t *testing.T, m db.Media) {
				if m.Artist != "Boards of Canada" {
					t.Errorf("Artist = %q, want the track's own", m.Artist)
				}
				if m.AlbumArtist != "Various Artists" {
					t.Errorf("AlbumArtist = %q, want the album's", m.AlbumArtist)
				}
			},
		},
		{
			// The common case: one artist, and no album_artist tag at all.
			name:   "artist stands in for album artist",
			report: `{"streams":[],"format":{"tags":{"artist":"Bj\u00f6rk"}}}`,
			check: func(t *testing.T, m db.Media) {
				if m.AlbumArtist != "Björk" {
					t.Errorf("AlbumArtist = %q, want it to fall back to the artist", m.AlbumArtist)
				}
			},
		},
		{
			name:   "a rotation of 270",
			report: `{"streams":[{"codec_type":"video","tags":{"rotate":"270"}}],"format":{}}`,
			check: func(t *testing.T, m db.Media) {
				if m.Orientation != 8 {
					t.Errorf("Orientation = %d, want 8", m.Orientation)
				}
			},
		},
		{
			name:   "a video with no streams",
			report: `{"streams":[],"format":{"duration":"3.0"}}`,
			check: func(t *testing.T, m db.Media) {
				if m.DurationMS != 3000 || m.Codec != "" {
					t.Errorf("got %+v", m)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			kind := db.KindVideo
			if tt.name == "no tags at all" || tt.name == "album artist stands in for artist" {
				kind = db.KindAudio
			}
			tt.check(t, parse(t, tt.report).mediaFrom(kind))
		})
	}
}

func parse(t *testing.T, s string) probeReport {
	t.Helper()
	var report probeReport
	if err := json.Unmarshal([]byte(s), &report); err != nil {
		t.Fatalf("parse the report: %v", err)
	}
	return report
}

// TestProbeReportAudioStream is what a transcode decision is made from. The
// first four are what ffprobe 7.1 printed for the same two seconds of sine
// encoded four ways, trimmed to the fields that matter and with every quirk
// left in: the rates are strings, a FLAC has no bitrate on its stream, a
// lossless codec states its width in bits_per_raw_sample and a lossy one
// states none. PCM and the unparseable report are written for their edges.
func TestProbeReportAudioStream(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		report string
		want   db.Media
	}{
		"flac takes the file's bitrate": {
			report: `{"streams": [{"codec_type": "audio", "codec_name": "flac", "sample_rate": "44100",
				"channels": 2, "bits_per_sample": 0, "bits_per_raw_sample": "16"}],
				"format": {"duration": "2.000000", "bit_rate": "131028"}}`,
			want: db.Media{Codec: "flac", Bitrate: 131_028, SampleRate: 44_100, Channels: 2, BitDepth: 16},
		},
		"mp3 has a bitrate and no width": {
			report: `{"streams": [{"codec_type": "audio", "codec_name": "mp3", "sample_rate": "44100",
				"channels": 2, "bits_per_sample": 0, "bit_rate": "320000"}],
				"format": {"duration": "2.000000", "bit_rate": "330364"}}`,
			want: db.Media{Codec: "mp3", Bitrate: 320_000, SampleRate: 44_100, Channels: 2},
		},
		"aac says which profile": {
			report: `{"streams": [{"codec_type": "audio", "codec_name": "aac", "profile": "LC",
				"sample_rate": "44100", "channels": 2, "bits_per_sample": 0, "bit_rate": "217241"}],
				"format": {"duration": "2.000000", "bit_rate": "224400"}}`,
			want: db.Media{Codec: "aac", CodecProfile: "LC", Bitrate: 217_241, SampleRate: 44_100, Channels: 2},
		},
		"alac is lossless in an m4a": {
			report: `{"streams": [{"codec_type": "audio", "codec_name": "alac", "sample_rate": "44100",
				"channels": 2, "bits_per_sample": 0, "bit_rate": "136208", "bits_per_raw_sample": "16"}],
				"format": {"duration": "2.000000", "bit_rate": "139500"}}`,
			want: db.Media{Codec: "alac", Bitrate: 136_208, SampleRate: 44_100, Channels: 2, BitDepth: 16},
		},
		"pcm states its width in bits_per_sample": {
			report: `{"streams": [{"codec_type": "audio", "codec_name": "pcm_s24le", "sample_rate": "48000",
				"channels": 2, "bits_per_sample": 24, "bit_rate": "2304000"}],
				"format": {"duration": "2.000000", "bit_rate": "2304200"}}`,
			want: db.Media{Codec: "pcm_s24le", Bitrate: 2_304_000, SampleRate: 48_000, Channels: 2, BitDepth: 24},
		},
		"aac as the image's own ffprobe prints it": {
			report: `{"streams": [{"codec_type": "audio", "codec_name": "aac", "profile": "1",
				"sample_rate": "44100", "channels": 2, "bits_per_sample": 0, "bit_rate": "127930"}],
				"format": {"duration": "2.000000", "bit_rate": "131000"}}`,
			want: db.Media{Codec: "aac", CodecProfile: "LC", Bitrate: 127_930, SampleRate: 44_100, Channels: 2},
		},
		"nothing parseable is unknown": {
			report: `{"streams": [{"codec_type": "audio", "codec_name": "opus", "profile": "unknown",
				"sample_rate": "N/A", "bit_rate": "N/A"}], "format": {"bit_rate": "N/A"}}`,
			want: db.Media{Codec: "opus"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			m := parse(t, c.report).mediaFrom(db.KindAudio)
			if m.Codec != c.want.Codec || m.CodecProfile != c.want.CodecProfile || m.Bitrate != c.want.Bitrate ||
				m.SampleRate != c.want.SampleRate || m.Channels != c.want.Channels || m.BitDepth != c.want.BitDepth {
				t.Errorf("got %s/%q %d bps %d Hz %d ch %d bits, want %s/%q %d bps %d Hz %d ch %d bits",
					m.Codec, m.CodecProfile, m.Bitrate, m.SampleRate, m.Channels, m.BitDepth,
					c.want.Codec, c.want.CodecProfile, c.want.Bitrate, c.want.SampleRate, c.want.Channels, c.want.BitDepth)
			}
		})
	}
}

// TestProbeReportVideoStream is what a direct play, remux or transcode
// decision needs of a video (#207), read out of what ffprobe 7.1 printed for
// the fixtures in testdata, trimmed. The quirks are ffprobe's: HEVC and VP9
// state no bits_per_raw_sample, so the depth comes from the pixel format; VP9
// has no level; an AC3 has no profile; the bitrate is the file's.
func TestProbeReportVideoStream(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		report string
		want   db.Media
	}{
		"hevc main 10 with aac": {
			report: `{"streams": [
				{"codec_type": "video", "codec_name": "hevc", "profile": "Main 10", "level": 30,
				 "pix_fmt": "yuv420p10le", "avg_frame_rate": "10/1", "width": 64, "height": 64},
				{"codec_type": "audio", "codec_name": "aac", "profile": "LC", "sample_rate": "48000", "channels": 2}],
				"format": {"duration": "1.000000", "bit_rate": "172816"}}`,
			want: db.Media{Codec: "hevc", CodecProfile: "Main 10", Level: 30, BitDepth: 10, FrameRate: 10_000,
				Bitrate: 172_816, AudioCodec: "aac", Channels: 2, SampleRate: 48_000},
		},
		"h264 with ac3 5.1 in matroska": {
			report: `{"streams": [
				{"codec_type": "video", "codec_name": "h264", "profile": "High", "level": 10,
				 "pix_fmt": "yuv420p", "bits_per_raw_sample": "8", "avg_frame_rate": "10/1", "width": 64, "height": 64},
				{"codec_type": "audio", "codec_name": "ac3", "profile": "unknown", "sample_rate": "48000", "channels": 6}],
				"format": {"duration": "1.000000", "bit_rate": "416848"}}`,
			want: db.Media{Codec: "h264", CodecProfile: "High", Level: 10, BitDepth: 8, FrameRate: 10_000,
				Bitrate: 416_848, AudioCodec: "ac3", Channels: 6, SampleRate: 48_000},
		},
		"vp9 with opus, which has no level": {
			report: `{"streams": [
				{"codec_type": "video", "codec_name": "vp9", "profile": "Profile 0", "level": -99,
				 "pix_fmt": "yuv420p", "avg_frame_rate": "10/1", "width": 64, "height": 64},
				{"codec_type": "audio", "codec_name": "opus", "profile": "unknown", "sample_rate": "48000", "channels": 2}],
				"format": {"duration": "1.008000", "bit_rate": "142714"}}`,
			want: db.Media{Codec: "vp9", CodecProfile: "Profile 0", BitDepth: 8, FrameRate: 10_000,
				Bitrate: 142_714, AudioCodec: "opus", Channels: 2, SampleRate: 48_000},
		},
		"h264 as the image's own ffprobe prints it": {
			report: `{"streams": [
				{"codec_type": "video", "codec_name": "h264", "profile": "100", "level": 10,
				 "pix_fmt": "unknown", "avg_frame_rate": "10/1", "width": 64, "height": 64},
				{"codec_type": "audio", "codec_name": "ac3", "profile": "unknown", "sample_rate": "48000", "channels": 6}],
				"format": {"duration": "1.000000", "bit_rate": "416848"}}`,
			want: db.Media{Codec: "h264", CodecProfile: "High", Level: 10, FrameRate: 10_000,
				Bitrate: 416_848, AudioCodec: "ac3", Channels: 6, SampleRate: 48_000},
		},
		"ntsc and no sound": {
			report: `{"streams": [
				{"codec_type": "video", "codec_name": "h264", "profile": "Main", "level": 40,
				 "pix_fmt": "yuv420p", "bits_per_raw_sample": "8", "avg_frame_rate": "30000/1001", "width": 64, "height": 64}],
				"format": {"duration": "1.000000", "bit_rate": "1000000"}}`,
			want: db.Media{Codec: "h264", CodecProfile: "Main", Level: 40, BitDepth: 8, FrameRate: 29_970,
				Bitrate: 1_000_000},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := parse(t, c.report).mediaFrom(db.KindVideo)
			got.Kind, got.DurationMS, got.Width, got.Height = "", 0, 0, 0
			if got != c.want {
				t.Errorf("got  %+v\nwant %+v", got, c.want)
			}
		})
	}
}

// TestPixelDepth: the names ffprobe gives the formats a camera or an encoder
// actually writes.
func TestPixelDepth(t *testing.T) {
	t.Parallel()
	for pixFmt, want := range map[string]int{
		"yuv420p": 8, "yuvj420p": 8, "nv12": 8, "gbrp": 8, "yuv444p": 8,
		"yuv420p10le": 10, "yuv422p10be": 10, "yuv420p12le": 12, "p010le": 10,
		"": 0, "unknown": 0,
	} {
		if got := pixelDepth(pixFmt); got != want {
			t.Errorf("pixelDepth(%q) = %d, want %d", pixFmt, got, want)
		}
	}
}

// TestProfileName: the numbers the image's ffprobe prints, back into the names
// a full one would.
func TestProfileName(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ codec, printed, want string }{
		{"h264", "66", "Baseline"},
		{"h264", "578", "Constrained Baseline"},
		{"h264", "100", "High"},
		{"h264", "110", "High 10"},
		{"h264", "2158", "High 10 Intra"},
		{"hevc", "2", "Main 10"},
		{"vp9", "0", "Profile 0"},
		{"av1", "0", "Main"},
		{"aac", "1", "LC"},
		{"aac", "4", "HE-AAC"},
		{"aac", "28", "HE-AACv2"},
		{"h264", "High", "High"},
		{"mp3", "unknown", ""},
		{"mp3", "3", ""},
	} {
		if got := profileName(c.codec, c.printed); got != c.want {
			t.Errorf("profileName(%q, %q) = %q, want %q", c.codec, c.printed, got, c.want)
		}
	}
}
