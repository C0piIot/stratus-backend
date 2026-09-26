package media

import (
	"strings"

	"github.com/C0piIot/stratus-backend/internal/db"
)

// Deciding whether a track is sent as it is or transcoded, and into what (#50).
//
// No HTTP here and no ffmpeg: the facts of the file on one side, what a client
// asked for on the other, and a Plan out. OpenSubsonic's stream is the first
// caller, its transcoding extension the second, and DLNA the one after that --
// three ways of asking the same question, which is why the answer is not
// written inside any of them.
//
// The rules are the ones a server that does this well already follows, with
// Navidrome as the reference: the original whenever it will do, and a
// transcode that never pretends to be better than its source -- no higher
// bitrate than the file has, no lossless file made out of a lossy one, no rate
// or channels that were not there.

// Request is what a client asked for. Format is a target's name, "raw" for
// the original whatever else is said, or empty for the server's choice.
// MaxBitrate is bits per second, and zero is no limit.
type Request struct {
	Format     string
	MaxBitrate int
}

// Plan is the answer. Direct means the original, and nothing else in it
// matters; otherwise it is what ffmpeg is to produce.
type Plan struct {
	Direct bool
	// Format is the target's name, and Encoder, Muxer and MIME what it means
	// to ffmpeg and to the client.
	Format, Encoder, Muxer, MIME string
	// Container is what the client is told the bytes are in, as it named it.
	Container string
	// Fragmented writes an MP4 as it goes rather than rewriting its head at
	// the end, which is the only MP4 a response can carry.
	Fragmented bool
	// Bitrate is bits per second, and zero for a lossless target, which has
	// none to set.
	Bitrate              int
	SampleRate, Channels int
	// BitDepth is set only to bring a lossless target down to sixteen bits;
	// zero keeps the source's.
	BitDepth int
}

// target is one thing a transcode can produce.
type target struct {
	encoder, muxer, mime string
	// container is what the output is called when nothing else is asked.
	container string
	lossless  bool
	// maxChannels is the most the encoder is given.
	maxChannels int
	// fallback is the bitrate when neither the client nor the source says
	// anything lower, and ceiling the most the encoder is asked for.
	fallback, ceiling int
	// rates are the sample rates this produces, highest first. A source at
	// one of them keeps it; any other is brought down to the first that
	// divides it evenly, and otherwise to the highest.
	rates []int
}

// targets are the formats a transcode produces. Each is here because a client
// asks for it by this name. m4a is not: its muxer rewrites the file's head
// once the end is known, and a response cannot be rewound.
var targets = map[string]target{
	"mp3": {
		encoder: "libmp3lame", muxer: "mp3", mime: "audio/mpeg", container: "mp3", maxChannels: 2,
		fallback: 192_000, ceiling: 320_000,
		// MPEG-1, MPEG-2 and MPEG-2.5 between them.
		rates: []int{48_000, 44_100, 32_000, 24_000, 22_050, 16_000, 12_000, 11_025, 8_000},
	},
	"opus": {
		// Ogg rather than the bare opus muxer's .opus: the same bytes, and the
		// MIME type every player that takes Opus over HTTP already knows.
		encoder: "libopus", muxer: "ogg", mime: "audio/ogg", container: "ogg", maxChannels: 8,
		fallback: 128_000, ceiling: 256_000,
		// All libopus takes. 44.1 kHz is not one of them, so the ordinary CD
		// rip goes to 48, which is what an Opus decoder puts out anyway.
		rates: []int{48_000, 24_000, 16_000, 12_000, 8_000},
	},
	"aac": {
		// ADTS, which is AAC a player can start before it has seen the end.
		encoder: "aac", muxer: "adts", mime: "audio/aac", container: "aac", maxChannels: 8,
		fallback: 192_000, ceiling: 320_000,
		// AAC goes to 96 kHz, and a phone on mobile data does not need it to.
		rates: []int{48_000, 44_100, 32_000, 24_000, 22_050, 16_000, 12_000, 11_025, 8_000},
	},
	"flac": {
		encoder: "flac", muxer: "flac", mime: "audio/flac", container: "flac", lossless: true, maxChannels: 8,
	},
}

// defaultFormat is what a transcode produces when the client did not name one:
// MP3, because it is the one format every client there has ever been plays.
const defaultFormat = "mp3"

// lossless reports whether a codec, as ffprobe names it, keeps every sample.
func lossless(codec string) bool {
	switch {
	case codec == "flac", codec == "alac", codec == "wavpack", codec == "ape", codec == "tta":
		return true
	case strings.HasPrefix(codec, "pcm_"):
		return true
	}
	return false
}

// Decide is the whole policy.
func Decide(m db.Media, req Request) Plan {
	format := strings.ToLower(req.Format)
	if format == "raw" {
		return Plan{Direct: true}
	}
	t, named := targets[format]

	tooLarge := req.MaxBitrate > 0 && (m.Bitrate == 0 || m.Bitrate > req.MaxBitrate)
	sameCodec := !named || format == m.Codec
	if sameCodec && !tooLarge {
		// What was asked for is what the file already is, or nothing was asked
		// for that the file does not already satisfy.
		return Plan{Direct: true}
	}
	if named && t.lossless && !lossless(m.Codec) {
		// A lossless copy of a lossy file is the same sound in more bytes.
		return Plan{Direct: true}
	}
	if !named || (t.lossless && tooLarge) {
		// Nothing named, or a lossless format under a bitrate it cannot keep
		// to: the limit is what the client needs, so a lossy format meets it.
		format, t = defaultFormat, targets[defaultFormat]
	}

	p := Plan{Format: format, Encoder: t.encoder, Muxer: t.muxer, MIME: t.mime, Container: t.container}
	if !t.lossless {
		p.Bitrate = t.fallback
		if req.MaxBitrate > 0 {
			p.Bitrate = min(p.Bitrate, req.MaxBitrate)
		}
		if !lossless(m.Codec) && m.Bitrate > 0 {
			p.Bitrate = min(p.Bitrate, m.Bitrate)
		}
		p.Bitrate = min(p.Bitrate, t.ceiling)
		// A lossy target carries two channels at most here: it is what a
		// phone on mobile data is going to play them through.
		p.Channels = min2(m.Channels, 2)
	}
	p.SampleRate = rateFor(t.rates, m.SampleRate)
	return p
}

// rateFor keeps the source's rate when the target carries it, and otherwise
// brings it to one the target does, preferring one it divides evenly -- 88.2
// kHz becomes 44.1 rather than 48. Zero means the source's own, and a source
// with no rate recorded is left to the encoder.
func rateFor(rates []int, source int) int {
	if len(rates) == 0 || source == 0 {
		return 0
	}
	for _, r := range rates {
		if r == source {
			return 0
		}
	}
	for _, r := range rates {
		if r <= source && source%r == 0 {
			return r
		}
	}
	return rates[0]
}

// min2 is min where zero means unknown and is left alone.
func min2(n, most int) int {
	if n == 0 {
		return 0
	}
	return min(n, most)
}
