package media

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/C0piIot/stratus-backend/internal/db"
)

// probeTimeout bounds ffprobe. A malformed file can send it looking for a moov
// atom through the whole thing, and the indexer must not stall on one file.
const probeTimeout = 2 * time.Minute

// runProbe executes ffprobe against a local path and returns its report.
//
// A local path, and not a pipe: ffprobe seeks, and an MP4 whose moov atom sits
// at the end -- which is every video a phone records -- cannot be read from a
// stream at all.
func runProbe(ctx context.Context, ffprobe, path string) (probeReport, error) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	// The path comes from exec.LookPath at startup and never from a request, so
	// there is nothing here an upload can influence. gosec cannot see that.
	//nolint:gosec // the binary is resolved once at startup, not per request
	cmd := exec.CommandContext(ctx, ffprobe,
		"-hide_banner", "-loglevel", "error",
		"-print_format", "json", "-show_format", "-show_streams", path)

	out, err := cmd.Output()
	if err != nil {
		// A process that was killed did not read the file and did not judge it:
		// the out-of-memory killer and the timeout above both arrive this way,
		// and neither says the recording is unreadable (#157).
		var exit *exec.ExitError
		// An exit code of -1 is the standard library's way of saying a signal
		// ended it, and is the one form of this that is portable.
		if errors.As(err, &exit) && exit.ExitCode() < 0 {
			return probeReport{}, fmt.Errorf("%w: ffprobe stopped with %s", errUnreachable, exit.ProcessState)
		}
		// ffprobe puts the reason on stderr and only an exit status in err, so
		// without this every failure reads "exit status 1".
		if errors.As(err, &exit) && len(exit.Stderr) > 0 {
			return probeReport{}, fmt.Errorf("ffprobe: %s", strings.TrimSpace(string(exit.Stderr)))
		}
		return probeReport{}, fmt.Errorf("ffprobe: %w", err)
	}

	var report probeReport
	if err := json.Unmarshal(out, &report); err != nil {
		return probeReport{}, fmt.Errorf("ffprobe returned something that is not JSON: %w", err)
	}
	return report, nil
}

// probeReport is the part of ffprobe's output this project reads.
type probeReport struct {
	Streams []probeStream `json:"streams"`
	Format  probeFormat   `json:"format"`
}

// The numbers ffprobe prints as strings -- a rate, a bitrate, a raw sample
// width -- are strings here too, and are parsed where they are read: a field
// that did not parse would otherwise fail the whole report.
type probeStream struct {
	CodecType        string            `json:"codec_type"`
	CodecName        string            `json:"codec_name"`
	Profile          string            `json:"profile"`
	Level            int               `json:"level"`
	PixFmt           string            `json:"pix_fmt"`
	AvgFrameRate     string            `json:"avg_frame_rate"`
	Width            int               `json:"width"`
	Height           int               `json:"height"`
	Duration         string            `json:"duration"`
	SampleRate       string            `json:"sample_rate"`
	Channels         int               `json:"channels"`
	BitRate          string            `json:"bit_rate"`
	BitsPerSample    int               `json:"bits_per_sample"`
	BitsPerRawSample string            `json:"bits_per_raw_sample"`
	Tags             map[string]string `json:"tags"`
	SideData         []probeSideData   `json:"side_data_list"`
}

// probeSideData is where a display matrix arrives. ffprobe used to put the
// angle in a stream tag and now reports it here instead, so both are read: the
// version in the image is ours to choose, and a file indexed by one build must
// not come back rotated differently under the next.
type probeSideData struct {
	// Rotation is in degrees counter-clockwise, and fractional in principle.
	// The name of the side data type is not matched on, because ffprobe spells
	// it "Display Matrix" in one version and "DisplayMatrix" in another --
	// whereas the field itself only appears on the entry that has one.
	Rotation float64 `json:"rotation"`
}

type probeFormat struct {
	Duration string            `json:"duration"`
	BitRate  string            `json:"bit_rate"`
	Tags     map[string]string `json:"tags"`
}

// mediaFrom turns a report into a row. Split from running ffprobe on purpose:
// all the interpretation is here, where it can be tested against captured
// output without a binary to invoke.
func (p probeReport) mediaFrom(kind db.Kind) db.Media {
	m := db.Media{Kind: kind, DurationMS: durationMS(p.Format.Duration)}

	video := p.stream("video")
	audio := p.stream("audio")

	switch {
	case kind == db.KindVideo && video != nil:
		m.Codec = video.CodecName
		m.Width, m.Height = video.Width, video.Height
		if m.DurationMS == 0 {
			m.DurationMS = durationMS(video.Duration)
		}
		// Phones record rotated and put the angle in a side matrix. Without
		// reading it every portrait video plays on its side.
		m.Orientation = videoOrientation(video)
		pictureStream(&m, video, p.Format)
		if audio != nil {
			m.AudioCodec = audio.CodecName
			m.Channels = audio.Channels
			m.SampleRate = atoi(audio.SampleRate)
		}
	case kind == db.KindAudio && audio != nil:
		m.Codec = audio.CodecName
		if m.DurationMS == 0 {
			m.DurationMS = durationMS(audio.Duration)
		}
		audioStream(&m, audio, p.Format)
	}

	tags := p.Format.Tags
	m.Title = tag(tags, "title")
	m.Artist = firstOf(tag(tags, "artist"), tag(tags, "album_artist"))
	// Its own field rather than a fallback for Artist: they differ on exactly
	// the records where it matters, and grouping albums by the track artist is
	// what turns a compilation into one album per track.
	m.AlbumArtist = firstOf(tag(tags, "album_artist"), m.Artist)
	m.Album = tag(tags, "album")
	m.Genre = tag(tags, "genre")
	m.TrackNo = leadingInt(tag(tags, "track"))
	m.DiscNo = leadingInt(tag(tags, "disc"))
	m.Year = leadingInt(tag(tags, "date"))
	if m.Year == 0 {
		m.Year = leadingInt(tag(tags, "year"))
	}
	m.TakenAt = parseProbeTime(tag(tags, "creation_time"))
	return m
}

// audioStream fills what a transcode decision is made from (#197).
//
// A FLAC states no bitrate for its stream, so the file's stands in -- which
// is its size over its duration and therefore counts an embedded cover too.
// Close enough to choose between direct play and a transcode, and the only
// number there is. The width is the decoded one for a lossless codec and the
// container's for PCM; a lossy codec has neither and keeps zero.
func audioStream(m *db.Media, audio *probeStream, format probeFormat) {
	m.SampleRate = atoi(audio.SampleRate)
	m.Channels = audio.Channels
	m.Bitrate = atoi(audio.BitRate)
	if m.Bitrate == 0 {
		m.Bitrate = atoi(format.BitRate)
	}
	m.BitDepth = atoi(audio.BitsPerRawSample)
	if m.BitDepth == 0 {
		m.BitDepth = audio.BitsPerSample
	}
	m.CodecProfile = profileName(audio.CodecName, audio.Profile)
}

// pictureStream fills what a transcode decision needs of a video's picture
// (#207), in the same words the in-place readers use.
//
// The depth comes from bits_per_raw_sample where ffprobe states it, and from
// the pixel format where it does not -- which is HEVC and VP9, that is, the
// ten-bit cases this is for. The bitrate is the file's: ffprobe's container
// figure, which is its size over its duration.
func pictureStream(m *db.Media, video *probeStream, format probeFormat) {
	m.CodecProfile = profileName(video.CodecName, video.Profile)
	if video.Level > 0 {
		m.Level = video.Level
	}
	m.BitDepth = atoi(video.BitsPerRawSample)
	if m.BitDepth == 0 {
		m.BitDepth = pixelDepth(video.PixFmt)
	}
	m.FrameRate = milliRate(video.AvgFrameRate)
	m.Bitrate = atoi(format.BitRate)
}

// profileName is ffprobe's name for a profile, whichever way it was printed.
//
// A full ffprobe prints the name. Ours prints a number, because --enable-small
// drops the tables the names are in (build/ffprobe/Dockerfile), so the number
// is mapped back here -- through the same tables the in-place readers use for
// H.264 and HEVC, so that the two paths cannot name one profile two ways.
func profileName(codec, printed string) string {
	if printed == "" || printed == "unknown" {
		return ""
	}
	n, err := strconv.Atoi(printed)
	if err != nil {
		return printed
	}
	switch codec {
	case "h264":
		// libavcodec folds two constraint flags into the number: 1<<9 is
		// constrained and 1<<11 is intra, which are constraint_set1 and
		// constraint_set3 where the record keeps them.
		var constraints byte
		if n&(1<<9) != 0 {
			constraints |= 0x40
		}
		if n&(1<<11) != 0 {
			constraints |= 0x10
		}
		return avcProfile(byte(n&0xff), constraints)
	case "hevc":
		return hevcProfile(byte(n)) //nolint:gosec // a profile idc, five bits
	case "vp9":
		return "Profile " + printed
	case "av1":
		switch n {
		case 0:
			return "Main"
		case 1:
			return "High"
		case 2:
			return "Professional"
		}
	case "aac":
		return aacProfiles[n]
	}
	return ""
}

// aacProfiles are libavcodec's numbers for the AAC profiles, which are the
// audio object type less one, and its names for them.
var aacProfiles = map[int]string{
	0: "Main", 1: "LC", 2: "SSR", 3: "LTP", 4: "HE-AAC", 22: "LD", 28: "HE-AACv2", 38: "ELD",
}

// pixelDepth reads the depth out of a pixel format's name: yuv420p10le is ten
// bits, and a planar or packed format with no number after its layout is eight.
func pixelDepth(pixFmt string) int {
	if pixFmt == "" || pixFmt == "unknown" {
		return 0
	}
	name := strings.TrimSuffix(strings.TrimSuffix(pixFmt, "le"), "be")
	for _, depth := range []int{16, 14, 12, 10, 9} {
		if strings.HasSuffix(name, "p"+strconv.Itoa(depth)) {
			return depth
		}
	}
	if strings.HasPrefix(pixFmt, "p0") && len(name) == 4 { // p010, p012, p016
		return atoi(name[2:])
	}
	return 8
}

// milliRate turns ffprobe's rational frame rate into frames per thousand
// seconds, and 0/0 into unknown.
func milliRate(rational string) int {
	num, den, ok := strings.Cut(rational, "/")
	if !ok {
		return 0
	}
	n, d := atoi(num), atoi(den)
	if n == 0 || d == 0 {
		return 0
	}
	return n * 1000 / d
}

// atoi reads one of ffprobe's numbers-as-strings, and "N/A" or nothing as zero,
// which is what the row calls unknown.
func atoi(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

func (p probeReport) stream(codecType string) *probeStream {
	for i := range p.Streams {
		if p.Streams[i].CodecType == codecType {
			return &p.Streams[i]
		}
	}
	return nil
}

// tag looks a key up without caring about case: Matroska writes TITLE, MP4
// writes title, and ffprobe passes both through as it found them.
func tag(tags map[string]string, key string) string {
	for k, v := range tags {
		if strings.EqualFold(k, key) {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func firstOf(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// durationMS reads ffprobe's seconds-with-decimals into milliseconds.
func durationMS(seconds string) int64 {
	if seconds == "" || seconds == "N/A" {
		return 0
	}
	f, err := strconv.ParseFloat(seconds, 64)
	if err != nil || f <= 0 {
		return 0
	}
	return int64(f * 1000)
}

// leadingInt reads the number a tag starts with: "3/12" is track three, and
// "1997-05-20" is the year.
func leadingInt(s string) int {
	end := 0
	for end < len(s) && s[end] >= '0' && s[end] <= '9' {
		end++
	}
	if end == 0 {
		return 0
	}
	n, err := strconv.Atoi(s[:end])
	if err != nil {
		return 0
	}
	return n
}

// videoOrientation reads the rotation of a video stream from wherever this
// ffprobe puts it: the old clockwise "rotate" tag, or the display matrix in the
// side data, whose angle runs the other way.
func videoOrientation(s *probeStream) int {
	if degrees, err := strconv.Atoi(strings.TrimSpace(tag(s.Tags, "rotate"))); err == nil {
		return orientationFrom(degrees)
	}
	for _, side := range s.SideData {
		if side.Rotation != 0 {
			return orientationFrom(-int(side.Rotation))
		}
	}
	return 0
}

// orientationFrom maps a clockwise rotation in degrees onto the EXIF
// orientation values, so that a photo and a video rotated the same way are
// stored the same way.
func orientationFrom(clockwise int) int {
	switch ((clockwise % 360) + 360) % 360 {
	case 90:
		return 6
	case 180:
		return 3
	case 270:
		return 8
	default:
		return 0
	}
}

func parseProbeTime(value string) time.Time {
	if value == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02 15:04:05"} {
		if t, err := time.Parse(layout, value); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}
