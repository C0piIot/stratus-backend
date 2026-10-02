package web

import (
	"testing"

	"github.com/C0piIot/stratus-backend/internal/db"
)

// A row holds a zero for "unknown" and for nothing at all alike, so every one
// of these renders to the empty string that keeps a line off the page. The
// values beside them are what a person reads instead of what ffprobe printed.
func TestRenderingWhatWasExtracted(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		got  string
		want string
	}{
		{"no duration", duration(0), ""},
		{"a track", duration(225_000), "3:45"},
		{"under a minute", duration(7_400), "0:07"},
		{"a film", duration(6_750_000), "1:52:30"},

		{"no dimensions", dimensions(0, 1080), ""},
		{"a frame", dimensions(1920, 1080), "1920 × 1080"},

		{"no codec", codec("", "Main", 51), ""},
		{"a codec alone", codec("flac", "", 0), "flac"},
		{"a codec and its level", codec("h264", "High", 41), "h264 High level 4.1"},

		{"no bitrate", bitrate(0), ""},
		{"a track's", bitrate(320_000), "320 kbps"},
		{"a film's", bitrate(42_000_000), "42.0 Mbps"},

		{"no sample rate", sampleRate(0), ""},
		{"a CD's", sampleRate(44_100), "44.1 kHz"},
		{"a whole number", sampleRate(48_000), "48 kHz"},

		{"no frame rate", frameRate(0), ""},
		{"NTSC", frameRate(23_976), "23.976 fps"},
		{"PAL", frameRate(25_000), "25 fps"},

		{"no channels", channels(0), ""},
		{"one", channels(1), "mono"},
		{"two", channels(2), "stereo"},
		{"more", channels(6), "6 channels"},

		{"no depth", bitDepth(0), ""},
		{"ten bits", bitDepth(10), "10-bit"},

		{"no number", count(0), ""},
		{"a track number", count(3), "3"},

		{"an album artist of its own", otherName("Various Artists", "Björk"), "Various Artists"},
		{"the artist again", otherName("Björk", "Björk"), ""},

		{"nowhere", coordinates(nil), ""},
		{"somewhere", coordinates(&db.GPS{Latitude: 41.3879, Longitude: 2.1699}), "41.38790, 2.16990"},

		{"no colour", colour(db.Media{}), ""},
		{"HDR10", colour(db.Media{ColorPrimaries: "bt2020", ColorTransfer: "smpte2084"}), "bt2020, smpte2084"},

		{"no sound", sound(db.Media{}), ""},
		{"a track of its own", sound(db.Media{AudioCodec: "aac", Channels: 2}), "aac, stereo"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if tt.got != tt.want {
				t.Errorf("= %q, want %q", tt.got, tt.want)
			}
		})
	}
}
