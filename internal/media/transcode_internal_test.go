package media

import (
	"testing"

	"github.com/C0piIot/stratus-backend/internal/db"
)

// TestDecide is the policy, one case per rule, on the files a library holds.
func TestDecide(t *testing.T) {
	t.Parallel()

	flac := db.Media{Codec: "flac", Bitrate: 900_000, SampleRate: 44_100, Channels: 2, BitDepth: 16}
	hires := db.Media{Codec: "flac", Bitrate: 2_300_000, SampleRate: 96_000, Channels: 2, BitDepth: 24}
	mp3 := db.Media{Codec: "mp3", Bitrate: 320_000, SampleRate: 44_100, Channels: 2}
	low := db.Media{Codec: "mp3", Bitrate: 96_000, SampleRate: 22_050, Channels: 1}
	aac := db.Media{Codec: "aac", Bitrate: 256_000, SampleRate: 48_000, Channels: 2}
	surround := db.Media{Codec: "flac", Bitrate: 3_000_000, SampleRate: 48_000, Channels: 6}
	unread := db.Media{Codec: "flac"}

	mp3Of := func(bitrate, rate, channels int) Plan {
		return Plan{Format: "mp3", Encoder: "libmp3lame", Muxer: "mp3", MIME: "audio/mpeg",
			Bitrate: bitrate, SampleRate: rate, Channels: channels}
	}
	direct := Plan{Direct: true}

	for name, c := range map[string]struct {
		m    db.Media
		req  Request
		want Plan
	}{
		"nothing asked for":                   {flac, Request{}, direct},
		"raw, whatever else is said":          {flac, Request{Format: "raw", MaxBitrate: 128_000}, direct},
		"a limit the file is under":           {mp3, Request{MaxBitrate: 320_000}, direct},
		"the format the file already is":      {mp3, Request{Format: "mp3"}, direct},
		"a format nobody knows, and no limit": {flac, Request{Format: "flv"}, direct},
		"a lossless copy of a lossy file":     {mp3, Request{Format: "flac"}, direct},

		"a limit the file is over goes to mp3":     {flac, Request{MaxBitrate: 128_000}, mp3Of(128_000, 0, 2)},
		"mp3 of a flac, no limit":                  {flac, Request{Format: "mp3"}, mp3Of(192_000, 0, 2)},
		"a file already under the limit, as it is": {low, Request{Format: "mp3", MaxBitrate: 320_000}, direct},
		"the same format at a lower bitrate":       {mp3, Request{Format: "mp3", MaxBitrate: 128_000}, mp3Of(128_000, 0, 2)},
		"96 kHz comes down to 48":                  {hires, Request{Format: "mp3"}, mp3Of(192_000, 48_000, 2)},
		"5.1 comes down to stereo":                 {surround, Request{Format: "mp3"}, mp3Of(192_000, 0, 2)},
		"an unread bitrate under a limit":          {unread, Request{MaxBitrate: 128_000}, mp3Of(128_000, 0, 0)},
		"flac cannot keep to a bitrate limit":      {flac, Request{Format: "flac", MaxBitrate: 128_000}, mp3Of(128_000, 0, 2)},
		"a high limit is a ceiling, not a target":  {flac, Request{Format: "mp3", MaxBitrate: 1_000_000}, mp3Of(192_000, 0, 2)},

		"opus of a CD rip is 48 kHz": {flac, Request{Format: "opus", MaxBitrate: 96_000},
			Plan{Format: "opus", Encoder: "libopus", Muxer: "ogg", MIME: "audio/ogg", Bitrate: 96_000, SampleRate: 48_000, Channels: 2}},
		"never above the source's own bitrate": {low, Request{Format: "opus", MaxBitrate: 320_000},
			Plan{Format: "opus", Encoder: "libopus", Muxer: "ogg", MIME: "audio/ogg", Bitrate: 96_000, SampleRate: 48_000, Channels: 1}},
		"aac of an aac over the limit": {aac, Request{Format: "aac", MaxBitrate: 128_000},
			Plan{Format: "aac", Encoder: "aac", Muxer: "adts", MIME: "audio/aac", Bitrate: 128_000, Channels: 2}},
		"flac of a flac is the flac": {hires, Request{Format: "flac"}, direct},
	} {
		if got := Decide(c.m, c.req); got != c.want {
			t.Errorf("%s:\n got  %+v\n want %+v", name, got, c.want)
		}
	}
}

// TestRateFor: a rate the target carries is kept, and one it does not is
// brought to the one it divides evenly.
func TestRateFor(t *testing.T) {
	t.Parallel()
	mp3Rates := targets["mp3"].rates
	for source, want := range map[int]int{
		44_100: 0, 48_000: 0, 22_050: 0, 8_000: 0,
		96_000: 48_000, 88_200: 44_100, 192_000: 48_000, 176_400: 44_100,
		50_000: 48_000, 0: 0,
	} {
		if got := rateFor(mp3Rates, source); got != want {
			t.Errorf("rateFor(mp3, %d) = %d, want %d", source, got, want)
		}
	}
	if got := rateFor(targets["opus"].rates, 44_100); got != 48_000 {
		t.Errorf("opus of 44.1 kHz = %d, want 48000", got)
	}
	if got := rateFor(nil, 44_100); got != 0 {
		t.Errorf("a target with no rates = %d, want the source's", got)
	}
}
