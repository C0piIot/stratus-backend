package media

import (
	"bytes"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/C0piIot/stratus-backend/internal/db"
)

func sec(s float64) time.Duration { return time.Duration(s * float64(time.Second)) }

// TestSegments: about six seconds each, every one starting on a keyframe, the
// first at zero and the last running to the end -- with the frames each holds
// and where its seek is aimed.
func TestSegments(t *testing.T) {
	t.Parallel()
	every := func(step time.Duration, until time.Duration) []time.Duration {
		var out []time.Duration
		for k := time.Duration(0); k < until; k += step {
			out = append(out, k)
		}
		return out
	}
	for name, c := range map[string]struct {
		ix   Index
		want []Segment
	}{
		"an MP4, counted from its sample table": {
			Index{Keyframes: []time.Duration{0, sec(2), sec(4), sec(6), sec(8)}, KeySamples: []int{0, 50, 100, 150, 200}, Samples: 250, Duration: sec(10)},
			[]Segment{{0, sec(6), 150, 0}, {sec(6), sec(4), 100, sec(6.15)}},
		},
		"a Matroska file, counted from its frame duration": {
			Index{Keyframes: []time.Duration{sec(0.023), sec(2.023), sec(4.023), sec(6.023), sec(8.023)}, FrameDuration: 40 * time.Millisecond, Duration: sec(10)},
			[]Segment{{0, sec(6.023), 150, 0}, {sec(6.023), sec(3.977), 100, sec(6.173)}},
		},
		"sparse keyframes make long segments": {
			Index{Keyframes: []time.Duration{0, sec(20)}, KeySamples: []int{0, 500}, Samples: 625, Duration: sec(25)},
			[]Segment{{0, sec(20), 500, 0}, {sec(20), sec(5), 125, sec(20.15)}},
		},
		"all intra: the seek stays short of the next keyframe": {
			Index{Keyframes: every(40*time.Millisecond, sec(12)), FrameDuration: 40 * time.Millisecond, Duration: sec(12)},
			[]Segment{{0, sec(6), 150, 0}, {sec(6), sec(6), 151, sec(6.02)}},
		},
	} {
		if got := Segments(c.ix); !slices.Equal(got, c.want) {
			t.Errorf("%s:\n got  %v\n want %v", name, got, c.want)
		}
	}
	if got := Segments(Index{Keyframes: []time.Duration{0, sec(8)}, Duration: sec(10)}); got != nil {
		t.Errorf("a film whose frames cannot be counted = %v, want no segments", got)
	}
}

func TestSegmentNames(t *testing.T) {
	t.Parallel()
	s := Segment{Start: sec(6.006), Length: sec(5.5), Frames: 132, Seek: sec(6.156)}
	if s.Name() != "6006-5500-132-6156.ts" {
		t.Fatalf("Name = %q", s.Name())
	}
	if got, err := ParseSegment(s.Name()); err != nil || got != s {
		t.Errorf("ParseSegment(%q) = %v, %v", s.Name(), got, err)
	}
	for _, bad := range []string{
		"", "6006-5500-132-6156", "6006-5500.ts", "a-1-1-0.ts", "0-0-1-0.ts", "0-600000-1-0.ts",
		"0-1000-0-0.ts", "0-1000-1000-0.ts", "5000-1000-10-4000.ts", "5000-1000-10-7000.ts", "-1-1000-10-0.ts",
	} {
		if _, err := ParseSegment(bad); err == nil {
			t.Errorf("ParseSegment(%q) was accepted", bad)
		}
	}
}

func TestPlaylist(t *testing.T) {
	t.Parallel()
	got := Playlist([]Segment{{0, sec(6), 150, 0}, {sec(6), sec(4.5), 112, sec(6.15)}}, func(name string) string { return "?hls=" + name + "&k=t" })
	want := "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-PLAYLIST-TYPE:VOD\n#EXT-X-MEDIA-SEQUENCE:0\n#EXT-X-TARGETDURATION:6\n" +
		"#EXTINF:6.000,\n?hls=0-6000-150-0.ts&k=t\n#EXTINF:4.500,\n?hls=6000-4500-112-6150.ts&k=t\n#EXT-X-ENDLIST\n"
	if got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
}

func TestRemuxFor(t *testing.T) {
	t.Parallel()
	video := func(codec, audio string) db.Media {
		return db.Media{Kind: db.KindVideo, Codec: codec, AudioCodec: audio}
	}
	for name, c := range map[string]struct {
		m    db.Media
		want Remux
		ok   bool
	}{
		"h264 with aac, both copied":  {video("h264", "aac"), Remux{CopyAudio: true}, true},
		"hevc with mp3":               {video("hevc", "mp3"), Remux{CopyAudio: true}, true},
		"h264 with ac3, sound to aac": {video("h264", "ac3"), Remux{}, true},
		"h264 with no sound":          {video("h264", ""), Remux{}, true},
		"vp9 would need re-encoding":  {video("vp9", "opus"), Remux{}, false},
		"a track is not a film":       {db.Media{Kind: db.KindAudio, Codec: "h264"}, Remux{}, false},
	} {
		got, err := RemuxFor(c.m)
		if got != c.want || (err == nil) != c.ok {
			t.Errorf("%s: %+v, %v", name, got, err)
		}
	}
}

func TestSegmentArgs(t *testing.T) {
	t.Parallel()
	got := segmentArgs(Remux{}, Segment{Start: sec(6), Length: sec(4.5), Frames: 112, Seek: sec(6.15)})("http://127.0.0.1:1/t")
	want := []string{
		"-hide_banner", "-v", "error", "-nostdin", "-ss", "6.15",
		"-protocol_whitelist", "http,tcp", "-i", "http://127.0.0.1:1/t",
		"-map", "0:v:0", "-map", "0:a:0?", "-sn", "-dn",
		"-frames:v", "112", "-to", "10.5", "-c:v", "copy",
		"-c:a", "aac", "-ac", "2", "-b:a", "192000",
		"-copyts", "-output_ts_offset", "10", "-muxdelay", "0", "-muxpreload", "0", "-f", "mpegts", "-",
	}
	if !slices.Equal(got, want) {
		t.Errorf("got  %q\nwant %q", got, want)
	}
	first := segmentArgs(Remux{CopyAudio: true}, Segment{Length: sec(6), Frames: 150})("u")
	if slices.Contains(first, "-ss") || slices.Contains(first, "aac") {
		t.Errorf("the first segment, sound copied: %q", first)
	}
}

// TestSegmentsForReal: every segment of each film comes out as MPEG-TS,
// through the loopback like any transcode, sound copied or made AAC.
func TestSegmentsForReal(t *testing.T) {
	t.Parallel()
	tr := newTranscoder(realFFmpeg(t), loopback(t), 4)
	for _, c := range []struct {
		name string
		r    Remux
	}{{"gop.mkv", Remux{CopyAudio: true}}, {"gop.mp4", Remux{CopyAudio: true}}, {"ac3.mkv", Remux{}}} {
		for _, s := range segmentsOf(t, c.name) {
			b := segmentBytes(t, tr, c.name, c.r, s)
			// MPEG-TS is 188-byte packets, each opening with 0x47.
			if len(b) < 376 || len(b)%188 != 0 || b[0] != 0x47 || b[188] != 0x47 {
				t.Errorf("%s %s: %d bytes that are not MPEG-TS", c.name, s.Name(), len(b))
			}
		}
	}
	if len(tr.slots) != 0 {
		t.Error("a segment kept its slot")
	}
}

// TestSegmentsAreContiguous is the property the whole design rests on, held
// with ffprobe wherever there is one: each segment opens on its keyframe,
// holds the frames the playlist counted, and ends before the next begins.
func TestSegmentsAreContiguous(t *testing.T) {
	t.Parallel()
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("no ffprobe on the PATH")
	}
	tr := newTranscoder(realFFmpeg(t), loopback(t), 4)
	dir := t.TempDir()
	for _, name := range []string{"gop.mp4", "gop.mkv"} {
		body := readFixture(t, name)
		ix, err := KeyframesOf(bytes.NewReader(body), db.File{Path: name, Size: int64(len(body))})
		if err != nil {
			t.Fatal(err)
		}
		var lastEnd float64
		for i, s := range Segments(ix) {
			path := filepath.Join(dir, fmt.Sprintf("%s-%d.ts", name, i))
			if err := os.WriteFile(path, segmentBytes(t, tr, name, Remux{CopyAudio: true}, s), 0o600); err != nil {
				t.Fatal(err)
			}
			out, err := exec.CommandContext(t.Context(), ffprobe, "-v", "error", "-select_streams", "v:0",
				"-show_entries", "packet=pts_time", "-of", "csv=p=0", path).Output()
			if err != nil {
				t.Fatal(err)
			}
			var pts []float64
			for line := range strings.Lines(string(out)) {
				v, err := strconv.ParseFloat(strings.Trim(strings.TrimSpace(line), ","), 64)
				if err == nil {
					pts = append(pts, v)
				}
			}
			slices.Sort(pts)
			// The last segment's count may run one over what is there, since
			// a Matroska duration is a hair past the last frame: ffmpeg stops
			// at the end either way. Anywhere else it must be exact.
			last := i == len(Segments(ix))-1
			if len(pts) != s.Frames && (!last || len(pts) != s.Frames-1) {
				t.Errorf("%s %s: %d frames, the playlist counted %d", name, s.Name(), len(pts), s.Frames)
			}
			wantFirst := (ix.Keyframes[0] + timelineOffset).Seconds()
			if i > 0 {
				wantFirst = (s.Start + timelineOffset).Seconds()
			}
			if len(pts) > 0 && math.Abs(pts[0]-wantFirst) > 0.0015 {
				t.Errorf("%s %s: opens at %.3f, want its keyframe at %.3f", name, s.Name(), pts[0], wantFirst)
			}
			if len(pts) > 0 && i > 0 && pts[0] <= lastEnd {
				t.Errorf("%s %s: opens at %.3f, before the segment before it ended at %.3f", name, s.Name(), pts[0], lastEnd)
			}
			if len(pts) > 0 {
				lastEnd = pts[len(pts)-1]
			}
		}
	}
}

func segmentsOf(t *testing.T, name string) []Segment {
	t.Helper()
	body := readFixture(t, name)
	ix, err := KeyframesOf(bytes.NewReader(body), db.File{Path: name, Size: int64(len(body))})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	segs := Segments(ix)
	if len(segs) == 0 {
		t.Fatalf("%s: no segments from %+v", name, ix)
	}
	return segs
}

func segmentBytes(t *testing.T, tr *Transcoder, name string, r Remux, s Segment) []byte {
	t.Helper()
	out, err := tr.Segment(t.Context(), db.File{Path: name, BlobKey: name}, r, s)
	if err != nil {
		t.Fatalf("%s %s: %v", name, s.Name(), err)
	}
	defer func() { _ = out.Close() }()
	b, err := io.ReadAll(out)
	if err != nil {
		t.Fatalf("%s %s: %v", name, s.Name(), err)
	}
	return b
}

func TestIsVideoAndPlaysInBrowser(t *testing.T) {
	t.Parallel()
	if !IsVideo("a/film.MKV") || IsVideo("a/track.flac") || IsVideo("notes.txt") {
		t.Error("IsVideo does not go by the extension")
	}
	for name, c := range map[string]struct {
		path string
		m    db.Media
		want bool
	}{
		"h264 and aac in an mp4":      {"a.mp4", db.Media{Codec: "h264", AudioCodec: "aac"}, true},
		"vp9 and opus in a webm":      {"a.webm", db.Media{Codec: "vp9", AudioCodec: "opus"}, true},
		"silent h264 in a mov":        {"a.mov", db.Media{Codec: "h264"}, true},
		"matroska is not one":         {"a.mkv", db.Media{Codec: "h264", AudioCodec: "aac"}, false},
		"hevc is not every browser's": {"a.mp4", db.Media{Codec: "hevc", AudioCodec: "aac"}, false},
		"ac3 sound is not":            {"a.mp4", db.Media{Codec: "h264", AudioCodec: "ac3"}, false},
	} {
		if got := PlaysInBrowser(db.File{Path: c.path}, c.m); got != c.want {
			t.Errorf("%s: %v", name, got)
		}
	}
}
