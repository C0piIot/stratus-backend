package stream

import (
	"net/http"
	"testing"
	"time"
)

func TestATranscodeSaysWhatItIsAndThatItCannotBeRanged(t *testing.T) {
	t.Parallel()
	p := Plan{MIME: "audio/mpeg", Bitrate: 128_000}

	h := http.Header{}
	p.Headers(h, -1)
	if h.Get("Content-Type") != "audio/mpeg" || h.Get("Accept-Ranges") != "none" || h.Get("Content-Length") != "" {
		t.Errorf("no estimate = %v", h)
	}
	h = http.Header{}
	p.Headers(h, 1000)
	if h.Get("Content-Length") != "1000" {
		t.Errorf("an estimate = %v", h)
	}
}

func TestAnEstimateIsTheBitrateTimesWhatIsLeft(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name     string
		bitrate  int
		duration int64
		offset   time.Duration
		want     int64
	}{
		{"a minute at 128 kbps", 128_000, 60_000, 0, 960_000},
		{"the last half of it", 128_000, 60_000, 30 * time.Second, 480_000},
		{"lossless has no bitrate", 0, 60_000, 0, -1},
		{"nothing left", 128_000, 60_000, time.Minute, -1},
	} {
		if got := (Plan{Bitrate: c.bitrate}).EstimatedLength(c.duration, c.offset); got != c.want {
			t.Errorf("%s = %d, want %d", c.name, got, c.want)
		}
	}
}
