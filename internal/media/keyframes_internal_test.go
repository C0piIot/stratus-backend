package media

import (
	"bytes"
	"math"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/C0piIot/stratus-backend/internal/db"
)

// The gop fixtures are ten seconds of 64x64 at 25 fps with a keyframe every
// two seconds and three B-frames between references, muxed once as MP4 --
// which gives it composition offsets and an edit list -- and copied into
// Matroska, which carries a 23 ms start the copy inherited from the audio.
// The expected times are what ffprobe printed for the keyframe packets.
func TestKeyframesOf(t *testing.T) {
	t.Parallel()
	ms := func(v ...int) []time.Duration {
		out := make([]time.Duration, len(v))
		for i, n := range v {
			out[i] = time.Duration(n) * time.Millisecond
		}
		return out
	}
	for name, want := range map[string][]time.Duration{
		"gop.mp4": ms(0, 2000, 4000, 6000, 8000),
		"gop.mkv": ms(23, 2023, 4023, 6023, 8023),
	} {
		body := readFixture(t, name)
		ix, err := KeyframesOf(bytes.NewReader(body), db.File{Path: name, Size: int64(len(body))})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !slices.Equal(ix.Keyframes, want) {
			t.Errorf("%s: keyframes = %v, want %v", name, ix.Keyframes, want)
		}
		if ix.Duration < 9900*time.Millisecond || ix.Duration > 10100*time.Millisecond {
			t.Errorf("%s: duration = %v, want about ten seconds", name, ix.Duration)
		}
	}

	// A film with one keyframe is an index of one, and a file with no index
	// is refused rather than guessed at.
	body := readFixture(t, "moov-first.mp4")
	if ix, err := KeyframesOf(bytes.NewReader(body), db.File{Path: "moov-first.mp4", Size: int64(len(body))}); err != nil || len(ix.Keyframes) == 0 {
		t.Errorf("moov-first.mp4 = %v, %v", ix, err)
	}
	if _, err := KeyframesOf(bytes.NewReader([]byte("not a film")), db.File{Path: "x.avi", Size: 10}); err == nil {
		t.Error("an AVI was indexed")
	}
	if _, err := KeyframesOf(bytes.NewReader([]byte("not a film")), db.File{Path: "x.mkv", Size: 10}); err == nil {
		t.Error("ten bytes of text were indexed as Matroska")
	}
}

// TestKeyframesAgreeWithFFprobe holds the index to the keyframe packets
// ffprobe reports, on every fixture with a picture, wherever there is one.
func TestKeyframesAgreeWithFFprobe(t *testing.T) {
	t.Parallel()
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("no ffprobe on the PATH")
	}
	for _, name := range []string{"gop.mp4", "gop.mkv", "moov-first.mp4", "moov-last.mp4", "portrait.mp4", "clip.mkv", "clip.webm", "aac51.mp4", "main10.mp4"} {
		out, err := exec.CommandContext(t.Context(), ffprobe, "-v", "error", "-select_streams", "v:0",
			"-show_packets", "-show_entries", "packet=pts_time,flags", "-of", "csv=p=0",
			filepath.Join("testdata", name)).Output()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var want []time.Duration
		for line := range strings.Lines(string(out)) {
			pts, flags, _ := strings.Cut(strings.TrimSpace(line), ",")
			if !strings.Contains(flags, "K") {
				continue
			}
			s, perr := strconv.ParseFloat(pts, 64)
			if perr != nil {
				continue
			}
			want = append(want, time.Duration(math.Round(s*1000))*time.Millisecond)
		}
		slices.Sort(want)

		body := readFixture(t, name)
		ix, err := KeyframesOf(bytes.NewReader(body), db.File{Path: name, Size: int64(len(body))})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		got := make([]time.Duration, len(ix.Keyframes))
		for i, k := range ix.Keyframes {
			got[i] = k.Round(time.Millisecond)
		}
		if !slices.Equal(got, want) {
			t.Errorf("%s: keyframes = %v, ffprobe says %v", name, got, want)
		}
	}
}
