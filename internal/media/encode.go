package media

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/C0piIot/stratus-backend/internal/db"
	"github.com/C0piIot/stratus-backend/internal/files"
	"github.com/C0piIot/stratus-backend/internal/storage"
)

// A film re-encoded to H.264, for the players that cannot decode its picture
// (#50): HEVC on an older Chromecast or in Chrome, VP9 or AV1 on a television,
// 4K on a 1080p device, and HDR -- which is what an iPhone records -- on
// anything that shows only SDR.
//
// **It is offered, never imposed.** The playlist becomes a master playlist
// with the remux beside the re-encode, each declaring its CODECS, and the
// player takes the one it can decode: a Chromecast Ultra plays the HEVC copied
// for nothing and an old one takes the H.264. That is HLS's own negotiation,
// which is why nothing here has to know what device is asking.
//
// **Segments are a fixed six seconds**, not the keyframes a remux has to follow:
// a re-encoded segment makes its own keyframe where it starts, and ffmpeg's
// seek decodes forward from the one before to land exactly. Each is its own
// ffmpeg, like a remux, and **each is kept** as a derived blob, since encoding
// one is the most expensive thing this server does and seeking back or watching
// again should not do it twice. The name marks it a cache, which the sweep
// collects a week after it was written whatever its parent is doing
// (files.CachePrefix): a film is gigabytes re-encoded, and a cache that only
// grew would fill the disk with films watched once.
//
// **HDR is brought down to SDR**, HLG and PQ alike, through zscale and a Hable
// curve, with the colour the indexer read declared to it by setparams -- zscale
// finds no path from a frame whose colour nobody stated. Dolby Vision profile 5
// is refused: its picture is IPT-PQ, which that chain turns green and purple.
// Profile 8, which is what an iPhone records, carries an HLG or HDR10 picture
// that it handles like any other.

// ErrNoEncode means a film cannot be offered re-encoded here.
var ErrNoEncode = errors.New("media: this film cannot be re-encoded")

// Encode is what a film's re-encode is made as.
type Encode struct {
	// Width and Height are the picture's, brought within 1920x1080.
	Width, Height int
	// Tonemap brings HDR down to SDR, and Primaries, Transfer and Space are
	// the colour declared to it.
	Tonemap                    bool
	Primaries, Transfer, Space string
	// CopyAudio keeps sound that is already AAC or MP3.
	CopyAudio bool
	// HighRate is a picture above thirty frames a second, which needs a
	// higher level.
	HighRate bool
}

// The ceiling a re-encode is made within: 1080p, which every Chromecast since
// the third generation plays, at a bitrate cap that keeps a busy scene from
// swamping a home connection.
const (
	encodeWidth   = 1920
	encodeHeight  = 1080
	encodeMaxRate = 8_000_000
	encodeAudio   = 192_000
)

// EncodeFor decides what a film's re-encode is, or that it has none.
func EncodeFor(m db.Media) (Encode, error) {
	if m.Kind != db.KindVideo || m.Width <= 0 || m.Height <= 0 || m.DurationMS <= 0 || m.DoViProfile == 5 {
		return Encode{}, ErrNoEncode
	}
	e := Encode{
		CopyAudio: m.AudioCodec == "aac" || m.AudioCodec == "mp3",
		HighRate:  m.FrameRate > 30_500,
	}
	// A phone held upright records a landscape frame and a matrix saying turn
	// it; ffmpeg turns it before the scale, so the box is fitted to the frame
	// as it will be seen.
	w, h := m.Width, m.Height
	if m.Orientation == 6 || m.Orientation == 8 {
		w, h = h, w
	}
	e.Width, e.Height = fit(w, h)
	if m.ColorTransfer == "smpte2084" || m.ColorTransfer == "arib-std-b67" {
		e.Tonemap = true
		e.Primaries, e.Transfer, e.Space = orText(m.ColorPrimaries, "bt2020"), m.ColorTransfer, orText(m.ColorSpace, "bt2020nc")
	}
	return e, nil
}

// NeedsEncode reports whether a film is one a re-encode would help: anything
// but an eight-bit SDR H.264 within 1080p, which every player there is takes
// as a remux already.
func NeedsEncode(m db.Media) bool {
	w, h := m.Width, m.Height
	if m.Orientation == 6 || m.Orientation == 8 {
		w, h = h, w
	}
	sdr := m.ColorTransfer != "smpte2084" && m.ColorTransfer != "arib-std-b67"
	return m.Codec != "h264" || m.BitDepth > 8 || max(w, h) > encodeWidth || min(w, h) > encodeHeight || !sdr
}

// orText is s, or fallback when s is empty.
func orText(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

// fit brings a picture within 1920x1080 -- or 1080x1920, since the box turns
// with the picture and a portrait film is not made smaller for being upright
// -- keeping its shape and never enlarging it, to the even dimensions H.264
// needs.
func fit(w, h int) (int, int) {
	boxW, boxH := encodeWidth, encodeHeight
	if h > w {
		boxW, boxH = boxH, boxW
	}
	scale := math.Min(1, math.Min(float64(boxW)/float64(w), float64(boxH)/float64(h)))
	even := func(v float64) int { return max(2, int(math.Round(v/2))*2) }
	return even(float64(w) * scale), even(float64(h) * scale)
}

// encodedTarget is how long a re-encoded segment is.
const encodedTarget = 6 * time.Second

// EncodedSegments is the fixed grid of a re-encode: six seconds each, the last
// whatever is left.
func EncodedSegments(duration time.Duration) []Segment {
	var out []Segment
	for start := time.Duration(0); start < duration; start += encodedTarget {
		out = append(out, Segment{Start: start, Length: min(encodedTarget, duration-start)})
	}
	return out
}

// EncodedName is how a re-encoded segment is asked for, which a remuxed
// segment's name can never be mistaken for: it starts with a letter.
func EncodedName(s Segment) string {
	return fmt.Sprintf("h%d-%d.ts", s.Start.Milliseconds(), s.Length.Milliseconds())
}

// ParseEncoded reads EncodedName back.
func ParseEncoded(name string) (Segment, error) {
	base, ok := strings.CutSuffix(strings.TrimPrefix(name, "h"), ".ts")
	start, length, ok2 := strings.Cut(base, "-")
	if !ok || !ok2 || !strings.HasPrefix(name, "h") {
		return Segment{}, ErrNoEncode
	}
	s, err1 := strconv.ParseInt(start, 10, 64)
	l, err2 := strconv.ParseInt(length, 10, 64)
	if err1 != nil || err2 != nil || s < 0 || l <= 0 || time.Duration(l)*time.Millisecond > encodedTarget {
		return Segment{}, ErrNoEncode
	}
	return Segment{Start: time.Duration(s) * time.Millisecond, Length: time.Duration(l) * time.Millisecond}, nil
}

// Variant is one entry of a master playlist.
type Variant struct {
	URI           string
	Bandwidth     int
	Width, Height int
	Codecs        string
}

// MasterPlaylist offers variants for a player to choose among, by the codecs
// each declares.
func MasterPlaylist(variants []Variant) string {
	var b strings.Builder
	b.WriteString("#EXTM3U\n#EXT-X-VERSION:3\n")
	for _, v := range variants {
		fmt.Fprintf(&b, "#EXT-X-STREAM-INF:BANDWIDTH=%d", v.Bandwidth)
		if v.Width > 0 && v.Height > 0 {
			fmt.Fprintf(&b, ",RESOLUTION=%dx%d", v.Width, v.Height)
		}
		if v.Codecs != "" {
			fmt.Fprintf(&b, ",CODECS=%q", v.Codecs)
		}
		fmt.Fprintf(&b, "\n%s\n", v.URI)
	}
	return b.String()
}

// RemuxVariant is a film's remux as a master playlist names it: its own
// codecs, as RFC 6381 spells them, and its own bitrate.
func RemuxVariant(m db.Media, uri string) Variant {
	v := Variant{URI: uri, Bandwidth: or(m.Bitrate, encodeMaxRate), Width: m.Width, Height: m.Height}
	var codecs []string
	switch m.Codec {
	case "h264":
		codecs = append(codecs, avcCodecs(m.CodecProfile, m.Level))
	case "hevc":
		codecs = append(codecs, hevcCodecs(m.CodecProfile, m.Level))
	}
	switch {
	case m.AudioCodec == "mp3":
		codecs = append(codecs, "mp4a.40.34")
	case m.AudioCodec != "" || m.Channels > 0:
		codecs = append(codecs, "mp4a.40.2")
	}
	v.Codecs = strings.Join(codecs, ",")
	return v
}

// EncodeVariant is a film's re-encode as a master playlist names it.
func EncodeVariant(m db.Media, e Encode, uri string) Variant {
	level := 40
	if e.HighRate {
		level = 42
	}
	codecs := avcCodecs("High", level)
	if m.AudioCodec != "" || m.Channels > 0 {
		codecs += ",mp4a.40.2"
	}
	return Variant{URI: uri, Bandwidth: encodeMaxRate + encodeAudio, Width: e.Width, Height: e.Height, Codecs: codecs}
}

// avcCodecs is avc1 and the profile, constraints and level in hex. An unknown
// profile is named High, which is what a player checks for in practice.
func avcCodecs(profile string, level int) string {
	idc, constraints := 0x64, 0
	switch profile {
	case "Constrained Baseline":
		idc, constraints = 0x42, 0x40
	case "Baseline":
		idc = 0x42
	case "Main":
		idc = 0x4d
	case "Extended":
		idc = 0x58
	case "High 10", "High 10 Intra":
		idc = 0x6e
	case "High 4:2:2", "High 4:2:2 Intra":
		idc = 0x7a
	case "High 4:4:4 Predictive", "High 4:4:4 Intra":
		idc = 0xf4
	}
	if level <= 0 {
		level = 41
	}
	return fmt.Sprintf("avc1.%02x%02x%02x", idc, constraints, level)
}

// hevcCodecs is hvc1 as Apple's HLS authoring spec writes it for the two
// profiles a camera records.
func hevcCodecs(profile string, level int) string {
	if level <= 0 {
		level = 120
	}
	if profile == "Main 10" {
		return "hvc1.2.4.L" + strconv.Itoa(level) + ".B0"
	}
	return "hvc1.1.6.L" + strconv.Itoa(level) + ".B0"
}

// encodeArgs is the whole of what a re-encoded segment asks of ffmpeg.
func encodeArgs(e Encode, s Segment, threads int) func(url string) []string {
	return func(url string) []string {
		args := []string{"-hide_banner", "-v", "error", "-nostdin"}
		if s.Start > 0 {
			// Before -i, and exact: re-encoding decodes forward from the
			// keyframe before and starts on the frame asked for.
			args = append(args, "-ss", seconds(s.Start))
		}
		chain := fmt.Sprintf("scale=%d:%d", e.Width, e.Height)
		if e.Tonemap {
			// Scaled in the first step, where it is cheapest: tone mapping a
			// 4K frame in 32-bit float costs four times what a 1080p one does.
			chain = fmt.Sprintf("setparams=color_primaries=%s:color_trc=%s:colorspace=%s:range=tv,"+
				"zscale=w=%d:h=%d:t=linear:npl=100,format=gbrpf32le,zscale=p=bt709,tonemap=hable:desat=0,"+
				"zscale=t=bt709:m=bt709:r=tv", e.Primaries, e.Transfer, e.Space, e.Width, e.Height)
		}
		args = append(args,
			"-protocol_whitelist", "http,tcp",
			"-i", url,
			"-to", seconds(s.Start+s.Length),
			"-map", "0:v:0", "-map", "0:a:0?", "-sn", "-dn",
			"-vf", chain+",format=yuv420p",
			"-c:v", "libx264", "-preset", "veryfast", "-crf", "21",
			"-maxrate", strconv.Itoa(encodeMaxRate), "-bufsize", strconv.Itoa(2*encodeMaxRate),
			"-profile:v", "high", "-threads", strconv.Itoa(threads),
			// One keyframe, at the start: a segment is never sought within,
			// and a keyframe anywhere else only costs bits.
			"-g", "100000", "-sc_threshold", "0",
			"-color_primaries", "bt709", "-color_trc", "bt709", "-colorspace", "bt709",
		)
		if e.CopyAudio {
			args = append(args, "-c:a", "copy")
		} else {
			args = append(args, "-c:a", "aac", "-ac", "2", "-b:a", strconv.Itoa(encodeAudio))
		}
		return append(args,
			"-copyts", "-output_ts_offset", seconds(timelineOffset),
			"-muxdelay", "0", "-muxpreload", "0",
			"-f", "mpegts", "-",
		)
	}
}

// Encoder re-encodes segments, one per share of the machine it may take, and
// keeps each in the blob store.
type Encoder struct {
	tr      *Transcoder
	blobs   storage.Storage
	slots   chan struct{}
	threads int
}

// NewEncoder sizes itself to the machine: a re-encode is the heaviest job
// here, so it gets half the CPUs at a time and 256 MB of memory each, and the
// threads it may use are the CPUs divided among the slots.
func NewEncoder(tr *Transcoder, blobs storage.Storage) *Encoder {
	cpus := runtime.GOMAXPROCS(0)
	slots := encodeSlots(cpus, memoryLimit(os.DirFS("/")))
	return &Encoder{tr: tr, blobs: blobs, slots: make(chan struct{}, slots), threads: max(1, cpus/slots)}
}

// encodeSlots is decodeSlots for the heaviest job there is.
func encodeSlots(cpus int, limit int64) int {
	slots := max(1, cpus/2)
	if limit > 0 {
		slots = min(slots, int((limit-reservedMemory)/encodeBudget))
	}
	return max(1, slots)
}

// encodeBudget is what one re-encode is allowed: a 4K HEVC decoder, a scaler
// and libx264's lookahead at 1080p, with room.
const encodeBudget = 256 << 20

// Segment produces one re-encoded segment, from the store when it was made
// before.
func (e *Encoder) Segment(ctx context.Context, f db.File, enc Encode, s Segment) (io.ReadCloser, error) {
	key := files.DerivedKey(f.BlobKey, files.CachePrefix+"hls-h264-"+EncodedName(s))
	if body, _, err := e.blobs.Get(ctx, key, storage.All()); err == nil {
		return body, nil
	}

	select {
	case e.slots <- struct{}{}:
	default:
		return nil, ErrBusy
	}
	defer func() { <-e.slots }()

	out, err := e.tr.run(ctx, f, encodeArgs(enc, s, e.threads))
	if err != nil {
		return nil, err
	}
	// Whole before it is sent: a segment is a few megabytes, and one kept
	// half-written would be served half-written from then on.
	var buf bytes.Buffer
	_, err = io.Copy(&buf, out)
	_ = out.Close()
	if err != nil {
		return nil, fmt.Errorf("re-encode %q at %s: %w", f.Path, s.Start, err)
	}
	if _, err := e.blobs.Put(ctx, key, bytes.NewReader(buf.Bytes()), int64(buf.Len())); err != nil {
		// A segment that could not be kept is still a segment: it is sent,
		// and made again next time.
		return io.NopCloser(&buf), nil
	}
	return io.NopCloser(&buf), nil
}
