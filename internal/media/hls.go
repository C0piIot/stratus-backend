package media

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/C0piIot/stratus-backend/internal/db"
)

// A film as HLS, for the players that cannot take it as it is (#50).
//
// Remux only: the picture is copied, never re-encoded, because re-encoding
// video is the one job a Raspberry Pi cannot do at any useful speed and the
// one the issue asked to be the exception. What is changed is the container --
// Matroska, which a Chromecast does not take, into MPEG-TS, which it does --
// and the sound, when it is AC-3, DTS or anything else a television will not
// decode, into stereo AAC. That covers the commonest reason a film does not
// play: a picture every device decodes, in a box or with a soundtrack it does
// not.
//
// **Every segment is its own ffmpeg**, started when the segment is asked for
// and gone when it is sent. There is no session, no process that outlives a
// request and no directory of segments to clean up: seeking, resuming and a
// player fetching three segments at once are all just requests. What makes it
// possible is that a copied segment must start on a keyframe, and the
// container already says where those are (keyframes.go), so the playlist is
// written whole, with every segment's start and length, before anything is
// transcoded -- and each segment's name says where it is, so producing one
// needs nothing but its name.
//
// MPEG-TS rather than fragmented MP4 for the same reason: a TS segment stands
// alone, while fMP4 segments share an initialisation segment and a timeline
// that independent processes would have to agree on.
//
// What that costs, measured: copied sound cannot be cut between its packets,
// so a segment opens with the tenth of a second of sound the one before it
// ended with. Players take the overlap in their stride; a cut to the sample
// would mean re-encoding sound that is already fine.

// ErrNoHLS means a film cannot be offered as HLS here: its picture is one a
// remux cannot carry, or its container does not say where its keyframes are.
var ErrNoHLS = errors.New("media: this film cannot be remuxed to HLS")

// segmentTarget is how long a segment is meant to be. Six seconds is what
// Apple recommends and what a Chromecast buffers comfortably; a segment ends
// at the first keyframe past it, so a film with sparse keyframes has longer
// ones.
const segmentTarget = 6 * time.Second

// maxSegment bounds what a segment's name may ask for, so that a name nobody
// was given cannot ask one ffmpeg for the whole film.
const maxSegment = 60 * time.Second

// Segment is one piece of the playlist: where it starts and how long it is,
// which the playlist states; how many frames it holds in decode order, which
// is what ends it cleanly; and where ffmpeg is told to seek, which is not
// quite the start.
type Segment struct {
	Start, Length time.Duration
	Frames        int
	Seek          time.Duration
}

// Name is how a segment is asked for, and says everything needed to produce
// it: start, length, frames and seek, all in milliseconds but the frames.
func (s Segment) Name() string {
	return fmt.Sprintf("%d-%d-%d-%d.ts", s.Start.Milliseconds(), s.Length.Milliseconds(), s.Frames, s.Seek.Milliseconds())
}

// maxFrameRate bounds the frames a name may ask for per second of length.
const maxFrameRate = 240

// ParseSegment reads a Name back, refusing one that asks for more than a
// segment is.
func ParseSegment(name string) (Segment, error) {
	base, ok := strings.CutSuffix(name, ".ts")
	parts := strings.Split(base, "-")
	if !ok || len(parts) != 4 {
		return Segment{}, ErrNoHLS
	}
	var n [4]int64
	for i, p := range parts {
		v, err := strconv.ParseInt(p, 10, 64)
		if err != nil || v < 0 {
			return Segment{}, ErrNoHLS
		}
		n[i] = v
	}
	s := Segment{
		Start: time.Duration(n[0]) * time.Millisecond, Length: time.Duration(n[1]) * time.Millisecond,
		Frames: int(n[2]), Seek: time.Duration(n[3]) * time.Millisecond,
	}
	switch {
	case s.Length <= 0 || s.Length > maxSegment,
		s.Frames <= 0 || float64(s.Frames) > s.Length.Seconds()*maxFrameRate+1,
		s.Seek < s.Start || s.Seek > s.Start+s.Length:
		return Segment{}, ErrNoHLS
	}
	return s, nil
}

// seekBackoff is how far past a keyframe a seek is aimed. ffmpeg moves a seek
// in a film with B-frames back by 3/23 of a second before it looks for a
// keyframe, so a target just past one lands on the one before -- measured:
// aimed at 6.024 in a Matroska file with a keyframe at 6.023, it began at
// 4.023. Aimed past the backoff, it lands where it was meant to.
const seekBackoff = 150 * time.Millisecond

// Segments groups the keyframes into segments of about segmentTarget, each
// starting on one; the first starts at zero, so nothing before the first
// keyframe is lost, and the last runs to the end. It returns nil for a film
// whose frames between keyframes cannot be counted.
func Segments(ix Index) []Segment {
	counted := len(ix.KeySamples) == len(ix.Keyframes) && ix.Samples > 0
	if len(ix.Keyframes) == 0 || (!counted && ix.FrameDuration <= 0) {
		return nil
	}
	// frames is how many frames lie between keyframe i and keyframe j (or the
	// end), in decode order.
	frames := func(i, j int) int {
		if counted {
			end := ix.Samples
			if j < len(ix.KeySamples) {
				end = ix.KeySamples[j]
			}
			return end - ix.KeySamples[i]
		}
		end := ix.Duration + ix.FrameDuration
		if j < len(ix.Keyframes) {
			end = ix.Keyframes[j]
		}
		return int(math.Round(float64(end-ix.Keyframes[i]) / float64(ix.FrameDuration)))
	}
	// seek is where to aim for keyframe i: past it by the backoff, or by half
	// the way to the next keyframe when that is closer, so that the aim never
	// reaches a keyframe it did not mean.
	seek := func(i int) time.Duration {
		if i == 0 {
			return 0
		}
		gap := seekBackoff
		if i+1 < len(ix.Keyframes) {
			gap = min(gap, (ix.Keyframes[i+1]-ix.Keyframes[i])/2)
		}
		return ix.Keyframes[i] + gap
	}

	var out []Segment
	first := 0
	start := time.Duration(0)
	for i, k := range ix.Keyframes {
		if i == 0 || k-start < segmentTarget || k >= ix.Duration {
			continue
		}
		out = append(out, Segment{Start: start, Length: k - start, Frames: frames(first, i), Seek: seek(first)})
		first, start = i, k
	}
	if ix.Duration > start {
		out = append(out, Segment{Start: start, Length: ix.Duration - start, Frames: frames(first, len(ix.Keyframes)), Seek: seek(first)})
	}
	return out
}

// Playlist is the media playlist for segments, each addressed as uri(name).
// A VOD playlist, whole and ended, which is what lets a player show the
// length and seek anywhere before a segment exists.
func Playlist(segments []Segment, uri func(name string) string) string {
	longest := time.Duration(0)
	for _, s := range segments {
		longest = max(longest, s.Length)
	}
	var b strings.Builder
	b.WriteString("#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-PLAYLIST-TYPE:VOD\n#EXT-X-MEDIA-SEQUENCE:0\n")
	fmt.Fprintf(&b, "#EXT-X-TARGETDURATION:%d\n", int(math.Ceil(longest.Seconds())))
	for _, s := range segments {
		fmt.Fprintf(&b, "#EXTINF:%.3f,\n%s\n", s.Length.Seconds(), uri(s.Name()))
	}
	b.WriteString("#EXT-X-ENDLIST\n")
	return b.String()
}

// Remux is what a segment does to a film's streams: the picture is always
// copied, and the sound is copied when CopyAudio says so and otherwise made
// stereo AAC.
type Remux struct {
	CopyAudio bool
}

// RemuxFor decides whether a film can be offered as HLS, and how.
//
// The picture has to be H.264 or HEVC: what MPEG-TS carries and a player
// decodes out of it. Anything else -- VP9, AV1, MPEG-4 Part 2 -- would need
// re-encoding, and is refused, so the caller offers the file as it is. The
// sound is copied when it is AAC or MP3, which is what every HLS player takes,
// and made AAC otherwise.
func RemuxFor(m db.Media) (Remux, error) {
	if m.Kind != db.KindVideo || (m.Codec != "h264" && m.Codec != "hevc") {
		return Remux{}, ErrNoHLS
	}
	// A film with no sound, or sound nobody named, is asked for AAC: -map
	// 0:a:0? makes that nothing when there is nothing to map.
	return Remux{CopyAudio: m.AudioCodec == "aac" || m.AudioCodec == "mp3"}, nil
}

// Segment produces one segment of f, remuxed as r says, through the same
// slots and the same loopback as an audio transcode.
func (t *Transcoder) Segment(ctx context.Context, f db.File, r Remux, s Segment) (io.ReadCloser, error) {
	return t.run(ctx, f, segmentArgs(r, s))
}

// segmentArgs is the whole of what a segment asks of ffmpeg.
func segmentArgs(r Remux, s Segment) func(url string) []string {
	return func(url string) []string {
		args := []string{"-hide_banner", "-v", "error", "-nostdin"}
		if s.Seek > 0 {
			args = append(args, "-ss", seconds(s.Seek))
		}
		args = append(args,
			"-protocol_whitelist", "http,tcp",
			"-i", url,
			"-map", "0:v:0", "-map", "0:a:0?", "-sn", "-dn",
			// The picture ends on its frame count, which is exact; the sound
			// ends where the segment does. Where, not how long: with -copyts
			// the output keeps the film's own timestamps, and -t measured
			// against those ends every segment but the first before it begins.
			"-frames:v", strconv.Itoa(s.Frames),
			"-to", seconds(s.Start+s.Length),
			"-c:v", "copy",
		)
		if r.CopyAudio {
			args = append(args, "-c:a", "copy")
		} else {
			args = append(args, "-c:a", "aac", "-ac", "2", "-b:a", "192000")
		}
		return append(args,
			// The film's own timestamps rather than each segment starting
			// again at zero: the segments are independent processes, and this
			// is what makes them one timeline to a player. Moved on by a
			// constant, because B-frames give the first segment a decode time
			// below zero and the muxer would shift that one segment alone --
			// by 80 ms on the fixtures, enough to overlap its neighbour.
			// -muxdelay 0 stops the muxer adding its own 1.4 seconds.
			"-copyts", "-output_ts_offset", seconds(timelineOffset),
			"-muxdelay", "0", "-muxpreload", "0",
			"-f", "mpegts", "-",
		)
	}
}

// timelineOffset is added to every segment's timestamps. Ten seconds is far
// more than any B-frame delay, and a player takes the timeline from where the
// first segment starts.
const timelineOffset = 10 * time.Second

func seconds(d time.Duration) string {
	return strconv.FormatFloat(d.Seconds(), 'f', -1, 64)
}

// IsVideo reports whether a name is a film's, by its extension -- which is
// what a listing has to go on, since it does not read every row's metadata.
func IsVideo(name string) bool {
	return kindFromExtension(path.Ext(name)) == db.KindVideo
}

// PlaysInBrowser reports whether a browser takes a film as it is: a container
// every one of them opens, a picture every one decodes and a sound likewise.
// HEVC is not among them -- some do and most do not -- so it goes to HLS, where
// it is at least the television's or Safari's to judge.
func PlaysInBrowser(f db.File, m db.Media) bool {
	switch containerOf(f.Path) {
	case "mp4", "m4v", "mov", "webm":
	default:
		return false
	}
	switch m.Codec {
	case "h264", "vp8", "vp9", "av1":
	default:
		return false
	}
	switch m.AudioCodec {
	case "", "aac", "mp3", "opus", "vorbis", "flac":
		return true
	}
	return false
}
