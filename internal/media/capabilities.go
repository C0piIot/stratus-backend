package media

import (
	"errors"
	"fmt"
	"path"
	"slices"
	"strconv"
	"strings"

	"github.com/C0piIot/stratus-backend/internal/db"
)

// Deciding from what a client says it can play, rather than from what it asked
// for (#50).
//
// OpenSubsonic's transcoding extension is the first to ask this way: a client
// sends the containers and codecs it plays as they are, the formats it will
// take a transcode in, in order of preference, and limits per codec -- and the
// server chooses. The model here is that payload's, field for field, because it
// is also what a DLNA renderer's protocolInfo or a Chromecast's codec list will
// come down to: the adapter parses, this decides.
//
// It follows the reference implementation, Navidrome's, where the
// specification leaves room: a limitation not marked required never stops a
// file being played as it is, and a transcode never claims more than the file
// has -- no lossless target from a lossy source, no rate or channels added, a
// profile that could only be met by raising something is skipped.

// Capabilities is what a client can play. Bitrates are bits per second, and
// zero is no limit.
type Capabilities struct {
	MaxBitrate          int
	MaxTranscodeBitrate int
	Direct              []DirectProfile
	Targets             []TargetProfile
	Codecs              []CodecProfile
}

// DirectProfile is a combination a client plays as it is. An empty list
// means any.
type DirectProfile struct {
	Containers, Codecs, Protocols []string
	MaxChannels                   int
}

// TargetProfile is a format a client takes a transcode in.
type TargetProfile struct {
	Container, Codec, Protocol string
	MaxChannels                int
}

// CodecProfile is what a client says about one codec.
type CodecProfile struct {
	Codec  string
	Limits []Limit
}

// Limit is one condition on a codec: Name is audioBitrate, audioSamplerate,
// audioChannels, audioBitdepth or audioProfile, and Comparison is Equals,
// NotEquals, LessThanEqual or GreaterThanEqual. The last two read the first
// value only.
type Limit struct {
	Name, Comparison string
	Values           []string
	Required         bool
}

// Stream describes the audio a client would receive, in the shape the answer
// takes. Bitrate is bits per second.
type Stream struct {
	Protocol, Container, Codec, Profile     string
	Channels, Bitrate, SampleRate, BitDepth int
}

// Decision is the answer. Direct is the file as it is; otherwise Transcode
// says whether one was possible, and Plan is it.
type Decision struct {
	Direct, Transcode bool
	Plan              Plan
	Source, Target    Stream
	// Reasons say why each direct profile did not fit, for a client's log.
	Reasons []string
	// Error is set when neither was possible.
	Error string
}

// DecideFor is the whole policy for a client that described itself.
func DecideFor(f db.File, m db.Media, c Capabilities) Decision {
	src := Stream{
		Protocol: "http", Container: containerOf(f.Path), Codec: m.Codec, Profile: m.CodecProfile,
		Channels: m.Channels, Bitrate: m.Bitrate, SampleRate: m.SampleRate, BitDepth: m.BitDepth,
	}
	d := Decision{Source: src}

	if c.MaxBitrate > 0 && (src.Bitrate == 0 || src.Bitrate > c.MaxBitrate) {
		d.Reasons = append(d.Reasons, "audio bitrate not supported")
	} else {
		for _, p := range c.Direct {
			reason := directMisfit(src, p, c.Codecs)
			if reason == "" {
				return Decision{Direct: true, Plan: Plan{Direct: true}, Source: src}
			}
			d.Reasons = append(d.Reasons, reason)
		}
	}

	for _, p := range c.Targets {
		if plan, ok := planFor(src, p, c); ok {
			d.Transcode, d.Plan = true, plan
			d.Target = Stream{
				Protocol: "http", Container: plan.Container, Codec: plan.Format,
				Channels: or(plan.Channels, src.Channels), Bitrate: plan.Bitrate,
				SampleRate: or(plan.SampleRate, src.SampleRate),
			}
			if targets[plan.Format].lossless {
				d.Target.BitDepth = or(plan.BitDepth, src.BitDepth)
			}
			return d
		}
	}
	d.Error = "no compatible playback profile found"
	return d
}

// directMisfit is why a file cannot be played as it is under p, or "".
func directMisfit(src Stream, p DirectProfile, codecs []CodecProfile) string {
	if len(p.Protocols) > 0 && !containsFold(p.Protocols, "http") {
		return "protocol not supported"
	}
	if len(p.Containers) > 0 && !matchContainer(src.Container, p.Containers) {
		return fmt.Sprintf("container %q not supported", src.Container)
	}
	if len(p.Codecs) > 0 && !matchCodec(src.Codec, p.Codecs) {
		return fmt.Sprintf("audio codec %q not supported", src.Codec)
	}
	if p.MaxChannels > 0 && src.Channels > p.MaxChannels {
		return fmt.Sprintf("%d audio channels not supported", src.Channels)
	}
	for _, cp := range codecs {
		if !matchCodec(src.Codec, []string{cp.Codec}) {
			continue
		}
		for _, l := range cp.Limits {
			if l.Required && !satisfies(src, l) {
				return l.Name + " not supported"
			}
		}
	}
	return ""
}

// planFor builds the transcode a target profile asks for, or reports that it
// cannot be built without claiming more than the source has.
func planFor(src Stream, p TargetProfile, c Capabilities) (Plan, bool) {
	if p.Protocol != "" && !strings.EqualFold(p.Protocol, "http") {
		return Plan{}, false // HLS is not served
	}
	format := strings.ToLower(p.Codec)
	if format == "" {
		format = strings.ToLower(p.Container)
	}
	t, ok := targets[format]
	if !ok {
		return Plan{}, false
	}
	container := strings.ToLower(p.Container)
	if container == "" {
		container = t.container
	}
	muxer, mime, fragmented, ok := packaging(format, container)
	if !ok {
		return Plan{}, false
	}
	sourceLossless := lossless(src.Codec)
	if t.lossless && !sourceLossless {
		return Plan{}, false
	}

	plan := Plan{
		Format: format, Encoder: t.encoder, Muxer: muxer, MIME: mime, Container: container,
		Fragmented: fragmented,
	}

	channels := min2(src.Channels, t.maxChannels)
	if p.MaxChannels > 0 {
		channels = min2(channels, p.MaxChannels)
	}
	rate := or(rateFor(t.rates, src.SampleRate), src.SampleRate)

	bitrate := 0
	switch {
	case t.lossless:
		if c.MaxBitrate > 0 && (src.Bitrate == 0 || src.Bitrate > c.MaxBitrate) {
			return Plan{}, false
		}
	case sourceLossless || src.Bitrate == 0:
		bitrate = firstNonZero(c.MaxTranscodeBitrate, c.MaxBitrate, t.fallback)
	default:
		bitrate = src.Bitrate
	}
	if bitrate > 0 {
		if c.MaxBitrate > 0 {
			bitrate = min(bitrate, c.MaxBitrate)
		}
		bitrate = min(bitrate, t.ceiling)
	}
	depth := 0
	if t.lossless {
		depth = src.BitDepth
	}

	// The target codec's own limits, which can bring a value down or rule the
	// profile out, and never raise anything.
	for _, cp := range c.Codecs {
		if !matchCodec(format, []string{cp.Codec}) {
			continue
		}
		for _, l := range cp.Limits {
			var fits bool
			switch l.Name {
			case "audioBitrate":
				if t.lossless {
					fits = satisfies(Stream{Bitrate: src.Bitrate}, l)
				} else {
					bitrate, fits = lower(bitrate, l, nil)
				}
			case "audioSamplerate":
				rate, fits = lower(rate, l, t.rates)
			case "audioChannels":
				channels, fits = lower(channels, l, nil)
			case "audioBitdepth":
				if depth == 0 {
					fits = true
					break
				}
				// Sixteen is the one depth a lossless target is brought down
				// to (Plan.BitDepth); any other change is not offered.
				var to int
				to, fits = lower(depth, l, []int{depth, 16})
				fits = fits && (to == depth || to == 16)
				depth = to
			default:
				fits = true
			}
			if !fits {
				return Plan{}, false
			}
		}
	}

	plan.Bitrate = bitrate
	if rate != src.SampleRate {
		plan.SampleRate = rate
	}
	if channels != src.Channels {
		plan.Channels = channels
	}
	if t.lossless && depth == 16 && src.BitDepth > 16 {
		plan.BitDepth = 16
	}
	return plan, true
}

// packaging is how a target format goes out in the container a client named,
// or not at all: an MP4 only as fragments, since its usual head is written
// last, and no container a format does not belong in.
func packaging(format, container string) (muxer, mime string, fragmented, ok bool) {
	switch {
	case format == "mp3" && container == "mp3":
		return "mp3", "audio/mpeg", false, true
	case format == "opus" && (container == "ogg" || container == "opus" || container == "oga"):
		return "ogg", "audio/ogg", false, true
	case format == "aac" && (container == "aac" || container == "adts"):
		return "adts", "audio/aac", false, true
	case format == "aac" && (container == "mp4" || container == "m4a"):
		return "ipod", "audio/mp4", true, true
	case format == "flac" && container == "flac":
		return "flac", "audio/flac", false, true
	}
	return "", "", false, false
}

// satisfies reports whether the source meets a limit as it is.
func satisfies(src Stream, l Limit) bool {
	switch l.Name {
	case "audioBitrate":
		return compare(src.Bitrate, l)
	case "audioSamplerate":
		return compare(src.SampleRate, l)
	case "audioChannels":
		return compare(src.Channels, l)
	case "audioBitdepth":
		return compare(src.BitDepth, l)
	case "audioProfile":
		return compareText(src.Profile, l)
	}
	return true
}

// compare is one numeric limit against a value. A value nobody knows is let
// through: refusing a file for a number the indexer could not read would stop
// it playing anywhere.
func compare(v int, l Limit) bool {
	if v == 0 || len(l.Values) == 0 {
		return true
	}
	switch l.Comparison {
	case "LessThanEqual":
		n, ok := number(l.Values[0])
		return !ok || v <= n
	case "GreaterThanEqual":
		n, ok := number(l.Values[0])
		return !ok || v >= n
	case "Equals":
		return slices.ContainsFunc(l.Values, func(s string) bool { n, ok := number(s); return ok && n == v })
	case "NotEquals":
		return !slices.ContainsFunc(l.Values, func(s string) bool { n, ok := number(s); return ok && n == v })
	}
	return true
}

func compareText(v string, l Limit) bool {
	switch l.Comparison {
	case "Equals":
		return v == "" || containsFold(l.Values, v)
	case "NotEquals":
		return !containsFold(l.Values, v)
	}
	return true
}

// lower brings v down until it meets l, choosing among allowed where the
// target only produces some values, and reports false when that would take
// raising it. A zero is left alone, as compare lets it through.
func lower(v int, l Limit, allowed []int) (int, bool) {
	if compare(v, l) {
		return v, true
	}
	var candidates []int
	switch l.Comparison {
	case "LessThanEqual":
		n, _ := number(l.Values[0])
		candidates = []int{n}
	case "Equals":
		for _, s := range l.Values {
			if n, ok := number(s); ok {
				candidates = append(candidates, n)
			}
		}
	default:
		// GreaterThanEqual can only be met by raising, and NotEquals by a
		// value the client did not name.
		return v, false
	}
	best := 0
	for _, want := range candidates {
		for _, n := range fitTo(want, allowed) {
			if n <= v && n <= want && n > best && (l.Comparison != "Equals" || n == want) {
				best = n
			}
		}
	}
	if best == 0 {
		return v, false
	}
	return best, true
}

// fitTo is want where anything goes, or the values allowed that do not exceed
// it.
func fitTo(want int, allowed []int) []int {
	if allowed == nil {
		return []int{want}
	}
	var out []int
	for _, a := range allowed {
		if a <= want {
			out = append(out, a)
		}
	}
	return out
}

// containerOf names a file's container the way clients do, from its
// extension: the indexer does not record one, and the name is what a client
// sees in the suffix field anyway.
func containerOf(p string) string {
	ext := strings.TrimPrefix(strings.ToLower(path.Ext(p)), ".")
	switch ext {
	case "m4a", "m4b", "m4p":
		return "mp4"
	case "oga", "opus":
		return "ogg"
	case "aif":
		return "aiff"
	case "wma":
		return "asf"
	}
	return ext
}

// containerAliases and codecAliases are the names clients use for the same
// thing. Each group's first name is the one compared.
var containerAliases = aliases([][]string{
	{"mp4", "m4a", "m4b", "m4p"},
	{"aac", "adts"},
	{"mp3", "mpeg"},
	{"ogg", "oga", "opus"},
	{"aiff", "aif"},
	{"asf", "wma"},
})

var codecAliases = aliases([][]string{
	{"aac", "adts"},
	{"ac3", "ac-3"},
	{"eac3", "e-ac3", "e-ac-3", "eac-3"},
	{"wmav2", "wma2", "wma"},
})

func aliases(groups [][]string) map[string]string {
	m := map[string]string{}
	for _, g := range groups {
		for _, name := range g {
			m[name] = g[0]
		}
	}
	return m
}

func canonical(name string, table map[string]string) string {
	name = strings.ToLower(name)
	if c, ok := table[name]; ok {
		return c
	}
	return name
}

func matchContainer(c string, list []string) bool {
	c = canonical(c, containerAliases)
	return slices.ContainsFunc(list, func(s string) bool { return canonical(s, containerAliases) == c })
}

// matchCodec compares codec names, and lets any PCM variant ffprobe reports
// answer to what a client calls it: pcm, lpcm or wav.
func matchCodec(codec string, list []string) bool {
	c := canonical(codec, codecAliases)
	pcm := strings.HasPrefix(c, "pcm_")
	return slices.ContainsFunc(list, func(s string) bool {
		s = canonical(s, codecAliases)
		return s == c || (pcm && (s == "pcm" || s == "lpcm" || s == "wav"))
	})
}

func containsFold(list []string, s string) bool {
	return slices.ContainsFunc(list, func(v string) bool { return strings.EqualFold(v, s) })
}

func number(s string) (int, bool) {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	return n, err == nil
}

// or is v, or fallback when v is zero.
func or(v, fallback int) int {
	if v == 0 {
		return fallback
	}
	return v
}

// firstNonZero is the first of the values that is not zero.
func firstNonZero(values ...int) int {
	for _, v := range values {
		if v != 0 {
			return v
		}
	}
	return 0
}

// Params is a Plan as the transcodeParams a client carries from the decision
// to the stream: versioned, readable and unsigned.
//
// Unsigned on purpose. The stream it is redeemed at is authenticated like
// every other call, and nothing it can name is anything that caller could not
// ask stream for directly -- so a signature would guard nothing, and would be
// one more key derived from the password. What is guarded instead is that it
// names a Plan this server would have made: PlanFromParams refuses anything
// else.
func (p Plan) Params() string {
	return strings.Join([]string{"v1", p.Format, p.Container,
		strconv.Itoa(p.Bitrate), strconv.Itoa(p.SampleRate), strconv.Itoa(p.Channels), strconv.Itoa(p.BitDepth)}, "-")
}

// ErrBadParams means a transcodeParams this server did not write.
var ErrBadParams = errors.New("media: transcode parameters this server did not write")

// PlanFromParams is Params read back, and refused unless every part is one
// the decision could have produced.
func PlanFromParams(s string) (Plan, error) {
	parts := strings.Split(s, "-")
	if len(parts) != 7 || parts[0] != "v1" {
		return Plan{}, ErrBadParams
	}
	format, container := parts[1], parts[2]
	t, ok := targets[format]
	if !ok {
		return Plan{}, ErrBadParams
	}
	muxer, mime, fragmented, ok := packaging(format, container)
	if !ok {
		return Plan{}, ErrBadParams
	}
	var n [4]int
	for i, part := range parts[3:] {
		v, err := strconv.Atoi(part)
		if err != nil || v < 0 {
			return Plan{}, ErrBadParams
		}
		n[i] = v
	}
	bitrate, rate, channels, depth := n[0], n[1], n[2], n[3]
	switch {
	case t.lossless && bitrate != 0,
		bitrate > t.ceiling,
		rate != 0 && !slices.Contains(t.rates, rate),
		channels > t.maxChannels,
		depth != 0 && (!t.lossless || depth != 16):
		return Plan{}, ErrBadParams
	}
	return Plan{
		Format: format, Encoder: t.encoder, Muxer: muxer, MIME: mime, Container: container,
		Fragmented: fragmented, Bitrate: bitrate, SampleRate: rate, Channels: channels, BitDepth: depth,
	}, nil
}
