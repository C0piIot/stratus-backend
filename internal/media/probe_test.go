package media

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/C0piIot/stratus-backend/internal/db"
)

// videoReport is what ffprobe actually prints for a video off a phone,
// trimmed to the fields this package reads. Captured rather than invented:
// running ffprobe is the smoke test's job, interpreting it is this one's.
const videoReport = `{
  "streams": [
    {"codec_type": "video", "codec_name": "h264", "width": 1920, "height": 1080,
     "duration": "12.345000", "tags": {"rotate": "90", "language": "und"}},
    {"codec_type": "audio", "codec_name": "aac", "duration": "12.400000"}
  ],
  "format": {
    "duration": "12.400000",
    "tags": {"creation_time": "2024-06-01T12:30:15.000000Z", "com.apple.quicktime.model": "iPhone 15 Pro"}
  }
}`

const audioReport = `{
  "streams": [{"codec_type": "audio", "codec_name": "flac", "duration": "254.120000"}],
  "format": {
    "duration": "254.120000",
    "tags": {"TITLE": "Hunter", "ARTIST": "Björk", "ALBUM": "Homogenic",
             "track": "1/10", "disc": "1/1", "DATE": "1997-09-22", "GENRE": "Electronic"}
  }
}`

func TestProbeReportVideo(t *testing.T) {
	t.Parallel()
	m := parse(t, videoReport).mediaFrom(db.KindVideo)

	if m.Codec != "h264" {
		t.Errorf("Codec = %q", m.Codec)
	}
	if m.Width != 1920 || m.Height != 1080 {
		t.Errorf("dimensions = %dx%d", m.Width, m.Height)
	}
	if m.DurationMS != 12_400 {
		t.Errorf("DurationMS = %d, want 12400", m.DurationMS)
	}
	// A phone records rotated and says so in a tag; without this every portrait
	// video plays on its side.
	if m.Orientation != 6 {
		t.Errorf("Orientation = %d, want 6 for a 90 degree rotation", m.Orientation)
	}
	want := time.Date(2024, 6, 1, 12, 30, 15, 0, time.UTC)
	if !m.TakenAt.Equal(want) {
		t.Errorf("TakenAt = %v, want %v", m.TakenAt, want)
	}
}

func TestProbeReportAudio(t *testing.T) {
	t.Parallel()
	m := parse(t, audioReport).mediaFrom(db.KindAudio)

	if m.Codec != "flac" || m.DurationMS != 254_120 {
		t.Errorf("got codec %q duration %d", m.Codec, m.DurationMS)
	}
	// Matroska and FLAC write their tags in upper case and MP4 in lower, so the
	// lookup cannot care.
	if m.Title != "Hunter" || m.Artist != "Björk" || m.Album != "Homogenic" {
		t.Errorf("got %+v", m)
	}
	if m.TrackNo != 1 || m.DiscNo != 1 {
		t.Errorf("track %d disc %d, want the number before the slash", m.TrackNo, m.DiscNo)
	}
	if m.Year != 1997 {
		t.Errorf("Year = %d, want the year out of a full date", m.Year)
	}
	if m.Genre != "Electronic" {
		t.Errorf("Genre = %q", m.Genre)
	}
	// An audio file has no dimensions, and inventing them would be worse than
	// leaving them at zero.
	if m.Width != 0 || m.Height != 0 {
		t.Errorf("dimensions = %dx%d on an audio file", m.Width, m.Height)
	}
}

func TestProbeReportEdges(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		report string
		check  func(t *testing.T, m db.Media)
	}{
		{
			name:   "no tags at all",
			report: `{"streams":[{"codec_type":"audio","codec_name":"mp3"}],"format":{"duration":"1.0"}}`,
			check: func(t *testing.T, m db.Media) {
				if m.Artist != "" || m.Year != 0 || m.DurationMS != 1000 {
					t.Errorf("got %+v", m)
				}
			},
		},
		{
			name:   "duration not available",
			report: `{"streams":[{"codec_type":"video","codec_name":"h264","duration":"5.5"}],"format":{"duration":"N/A"}}`,
			check: func(t *testing.T, m db.Media) {
				// Matroska often has no duration in the container, only on the
				// stream.
				if m.DurationMS != 5500 {
					t.Errorf("DurationMS = %d, want it taken from the stream", m.DurationMS)
				}
			},
		},
		{
			name:   "album artist stands in for artist",
			report: `{"streams":[],"format":{"tags":{"album_artist":"Various"}}}`,
			check: func(t *testing.T, m db.Media) {
				if m.Artist != "Various" {
					t.Errorf("Artist = %q", m.Artist)
				}
			},
		},
		{
			name:   "a rotation of 270",
			report: `{"streams":[{"codec_type":"video","tags":{"rotate":"270"}}],"format":{}}`,
			check: func(t *testing.T, m db.Media) {
				if m.Orientation != 8 {
					t.Errorf("Orientation = %d, want 8", m.Orientation)
				}
			},
		},
		{
			name:   "a video with no streams",
			report: `{"streams":[],"format":{"duration":"3.0"}}`,
			check: func(t *testing.T, m db.Media) {
				if m.DurationMS != 3000 || m.Codec != "" {
					t.Errorf("got %+v", m)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			kind := db.KindVideo
			if tt.name == "no tags at all" || tt.name == "album artist stands in for artist" {
				kind = db.KindAudio
			}
			tt.check(t, parse(t, tt.report).mediaFrom(kind))
		})
	}
}

func parse(t *testing.T, s string) probeReport {
	t.Helper()
	var report probeReport
	if err := json.Unmarshal([]byte(s), &report); err != nil {
		t.Fatalf("parse the report: %v", err)
	}
	return report
}
