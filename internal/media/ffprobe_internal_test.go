package media

import (
	"os"
	"strings"
	"testing"

	"github.com/C0piIot/stratus-backend/internal/db"
)

// demuxerFor names the FFmpeg demuxer that reads each container Stratus
// probes. It is the mapping the Dockerfile's --enable-demuxer list encodes,
// written out here where a test can check it.
var demuxerFor = map[string]string{
	".mp3":  "mp3",
	".flac": "flac",
	".wav":  "wav",
	".aac":  "aac",
	".ogg":  "ogg",
	".opus": "ogg",
	".m4a":  "mov",
	".mp4":  "mov",
	".m4v":  "mov",
	".mov":  "mov",
	".mkv":  "matroska",
	".webm": "matroska",
	".avi":  "avi",
	".mpg":  "mpegps",
	".mpeg": "mpegps",
	".wma":  "asf",
	".wmv":  "asf",
}

// TestFFprobeReadsEveryExtension is the guard on a coupling nothing else
// enforces. The image ships an ffprobe built with only the demuxers this
// project needs, so an extension added to byExtension without its demuxer
// compiles, passes every test, and then cannot read that format in production.
//
// It reads the recipe as text rather than running ffprobe, so it holds in
// `make test` where there is no ffprobe at all.
func TestFFprobeReadsEveryExtension(t *testing.T) {
	t.Parallel()

	enabled := enabledDemuxers(t)

	for ext, kind := range byExtension {
		if kind == db.KindImage {
			continue // EXIF is read in pure Go; ffprobe never sees an image
		}

		demuxer, known := demuxerFor[ext]
		if !known {
			t.Errorf("%s is probed but no demuxer is named for it: add it to demuxerFor, "+
				"and to --enable-demuxer in build/ffprobe/Dockerfile", ext)
			continue
		}
		if !enabled[demuxer] {
			t.Errorf("%s needs the %q demuxer, which build/ffprobe/Dockerfile does not enable",
				ext, demuxer)
		}
	}
}

// TestDemuxerMapHasNoStrays keeps the mapping from outliving the table it
// describes, so a format dropped from byExtension does not leave a demuxer
// compiled in for nothing.
func TestDemuxerMapHasNoStrays(t *testing.T) {
	t.Parallel()

	for ext := range demuxerFor {
		if kind, ok := byExtension[ext]; !ok || kind == db.KindImage {
			t.Errorf("demuxerFor names %s, which byExtension does not probe", ext)
		}
	}
}

// enabledDemuxers reads the --enable-demuxer list out of the recipe. Tests run
// with the package directory as the working directory.
func enabledDemuxers(t *testing.T) map[string]bool {
	t.Helper()

	const recipe = "../../build/ffprobe/Dockerfile"
	body, err := os.ReadFile(recipe)
	if err != nil {
		t.Fatalf("reading %s: %v", recipe, err)
	}

	const flag = "--enable-demuxer="
	_, rest, found := strings.Cut(string(body), flag)
	if !found {
		t.Fatalf("%s has no %s line", recipe, flag)
	}
	list, _, _ := strings.Cut(rest, " ")

	out := map[string]bool{}
	for _, name := range strings.Split(strings.TrimSpace(strings.Trim(list, "\\\n")), ",") {
		if name != "" {
			out[name] = true
		}
	}
	if len(out) == 0 {
		t.Fatalf("%s enables no demuxers", recipe)
	}
	return out
}
