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
		// ffprobe puts the reason on stderr and only an exit status in err, so
		// without this every failure reads "exit status 1".
		var exit *exec.ExitError
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

type probeStream struct {
	CodecType string            `json:"codec_type"`
	CodecName string            `json:"codec_name"`
	Width     int               `json:"width"`
	Height    int               `json:"height"`
	Duration  string            `json:"duration"`
	Tags      map[string]string `json:"tags"`
}

type probeFormat struct {
	Duration string            `json:"duration"`
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
		// Phones record rotated and put the angle in a side matrix, which
		// ffprobe surfaces as a tag. Without it every portrait video plays on
		// its side.
		m.Orientation = orientationFrom(tag(video.Tags, "rotate"))
	case kind == db.KindAudio && audio != nil:
		m.Codec = audio.CodecName
		if m.DurationMS == 0 {
			m.DurationMS = durationMS(audio.Duration)
		}
	}

	tags := p.Format.Tags
	m.Title = tag(tags, "title")
	m.Artist = firstOf(tag(tags, "artist"), tag(tags, "album_artist"))
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

// orientationFrom maps a rotation in degrees onto the EXIF orientation values,
// so that a photo and a video rotated the same way are stored the same way.
func orientationFrom(rotate string) int {
	switch strings.TrimSpace(rotate) {
	case "90":
		return 6
	case "180":
		return 3
	case "270", "-90":
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
