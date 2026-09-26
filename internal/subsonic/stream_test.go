package subsonic_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/C0piIot/stratus-backend/internal/db"
	"github.com/C0piIot/stratus-backend/internal/media"
)

// fakeTranscoder stands in for ffmpeg: it records what it was asked to do and
// answers with the Plan's format written out, so a test can tell a transcode
// from the stored bytes.
type fakeTranscoder struct {
	mu      sync.Mutex
	plans   []media.Plan
	offsets []time.Duration
	err     error
	body    string
}

func (f *fakeTranscoder) Transcode(_ context.Context, _ db.File, p media.Plan, offset time.Duration) (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.plans = append(f.plans, p)
	f.offsets = append(f.offsets, offset)
	if f.err != nil {
		return nil, f.err
	}
	body := f.body
	if body == "" {
		body = "transcoded to " + p.Format
	}
	return io.NopCloser(strings.NewReader(body)), nil
}

func (f *fakeTranscoder) calls() ([]media.Plan, []time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]media.Plan(nil), f.plans...), append([]time.Duration(nil), f.offsets...)
}

// hiFi is a track a phone on mobile data would rather not be sent whole.
func hiFi() db.Media {
	m := song("Björk", "Homogenic", "Hunter", 1)
	m.Bitrate, m.SampleRate, m.Channels, m.BitDepth = 900_000, 44_100, 2, 16
	return m
}

// TestStreamTranscodesWhatTheClientAskedFor: format and maxBitRate reach the
// decision, the plan reaches ffmpeg, and what comes back is ffmpeg's -- typed
// as the target and offering no ranges, since a transcode has none to offer.
func TestStreamTranscodesWhatTheClientAskedFor(t *testing.T) {
	t.Parallel()
	l := newLibrary(t)
	f := l.add(t, "music/01 Hunter.flac", hiFi())

	rec := get(t, l, "stream", query("id", songIDOf(f.ID), "format", "mp3", "maxBitRate", "128"),
		"Range", "bytes=0-10")
	if rec.Code != http.StatusOK {
		t.Fatalf("stream = %d, want 200 -- a Range is not honoured on a transcode", rec.Code)
	}
	if got := rec.Body.String(); got != "transcoded to mp3" {
		t.Errorf("body = %q, want the transcode", got)
	}
	for header, want := range map[string]string{
		"Content-Type": "audio/mpeg", "Accept-Ranges": "none", "Content-Length": "",
	} {
		if got := rec.Header().Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}

	plans, offsets := l.transcoder.calls()
	if len(plans) != 1 || plans[0].Bitrate != 128_000 || plans[0].Encoder != "libmp3lame" || offsets[0] != 0 {
		t.Errorf("asked ffmpeg for %+v at %v, want 128 kbps MP3 from the start", plans, offsets)
	}
}

// TestStreamWithNothingToChangeIsTheOriginal: a limit the file is already
// under, and format=raw, never start ffmpeg.
func TestStreamWithNothingToChangeIsTheOriginal(t *testing.T) {
	t.Parallel()
	l := newLibrary(t)
	const path = "music/01 Hunter.flac"
	f := l.add(t, path, hiFi())

	for _, q := range []string{
		query("id", songIDOf(f.ID), "maxBitRate", "1000"),
		query("id", songIDOf(f.ID), "format", "raw", "maxBitRate", "64"),
		query("id", songIDOf(f.ID), "format", "flac"),
	} {
		if rec := get(t, l, "stream", q); rec.Body.String() != path {
			t.Errorf("%s: body = %q, want the stored bytes", q, rec.Body.String())
		}
	}
	if plans, _ := l.transcoder.calls(); len(plans) != 0 {
		t.Errorf("ffmpeg was asked for %+v", plans)
	}
}

// TestStreamSeeksWithTimeOffset is the transcodeOffset extension: the offset
// reaches ffmpeg, and an estimated length counts only what is left.
func TestStreamSeeksWithTimeOffset(t *testing.T) {
	t.Parallel()
	l := newLibrary(t)
	l.transcoder.body = strings.Repeat("x", 100_000)
	m := hiFi()
	m.DurationMS = 10_000
	f := l.add(t, "music/01 Hunter.flac", m)

	rec := get(t, l, "stream", query("id", songIDOf(f.ID), "format", "mp3", "maxBitRate", "128",
		"timeOffset", "4", "estimateContentLength", "true"))
	// 128 kbps is 16 000 bytes a second, and six seconds are left.
	if got := rec.Header().Get("Content-Length"); got != "96000" {
		t.Errorf("Content-Length = %q, want 96000", got)
	}
	if got := rec.Body.Len(); got != 96_000 {
		t.Errorf("body = %d bytes, want it cut to the length it announced", got)
	}
	if _, offsets := l.transcoder.calls(); len(offsets) != 1 || offsets[0] != 4*time.Second {
		t.Errorf("offsets = %v, want 4s", offsets)
	}
}

// TestStreamWhenEveryTranscodeIsTakenSendsTheOriginal: the answer chosen for a
// full house, because a larger file plays and an error is silence.
func TestStreamWhenEveryTranscodeIsTakenSendsTheOriginal(t *testing.T) {
	t.Parallel()
	l := newLibrary(t)
	l.transcoder.err = media.ErrBusy
	const path = "music/01 Hunter.flac"
	f := l.add(t, path, hiFi())

	rec := get(t, l, "stream", query("id", songIDOf(f.ID), "format", "mp3"))
	if rec.Code != http.StatusOK || rec.Body.String() != path {
		t.Errorf("stream = %d %q, want the stored bytes", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "audio/flac" {
		t.Errorf("Content-Type = %q, want the original's", got)
	}
}

// TestStreamWhenFFmpegFailsIsAnError: a file ffmpeg cannot read is reported
// before a byte is sent, in the XML this endpoint answers errors with.
func TestStreamWhenFFmpegFailsIsAnError(t *testing.T) {
	t.Parallel()
	l := newLibrary(t)
	l.transcoder.err = errors.New("ffmpeg: Invalid data found when processing input")
	f := l.add(t, "music/01 Hunter.flac", hiFi())

	rec := get(t, l, "stream", query("id", songIDOf(f.ID), "format", "mp3"))
	if !strings.Contains(rec.Header().Get("Content-Type"), "xml") || !strings.Contains(rec.Body.String(), `status="failed"`) {
		t.Errorf("stream = %q %q, want a failed envelope in XML", rec.Header().Get("Content-Type"), rec.Body.String())
	}
}

// TestHeadOfATranscodeStartsNothing: a client asking what it would get is
// told, and no ffmpeg is started to tell it.
func TestHeadOfATranscodeStartsNothing(t *testing.T) {
	t.Parallel()
	l := newLibrary(t)
	f := l.add(t, "music/01 Hunter.flac", hiFi())

	req := request(t, "stream", query("id", songIDOf(f.ID), "format", "opus"))
	req.Method = http.MethodHead
	rec := httptest.NewRecorder()
	l.ServeHTTP(rec, req)
	if got := rec.Header().Get("Content-Type"); got != "audio/ogg" {
		t.Errorf("Content-Type = %q, want audio/ogg", got)
	}
	if plans, _ := l.transcoder.calls(); len(plans) != 0 {
		t.Errorf("a HEAD started ffmpeg: %+v", plans)
	}
}

// TestDownloadIsNeverTranscoded: download means the file.
func TestDownloadIsNeverTranscoded(t *testing.T) {
	t.Parallel()
	l := newLibrary(t)
	const path = "music/01 Hunter.flac"
	f := l.add(t, path, hiFi())

	if rec := get(t, l, "download", query("id", songIDOf(f.ID), "format", "mp3", "maxBitRate", "64")); rec.Body.String() != path {
		t.Errorf("download = %q, want the stored bytes", rec.Body.String())
	}
}

// TestAnEstimateNeedsABitrateAndSomethingLeft: a lossless target has no
// bitrate to multiply, and an offset past the end leaves nothing to count, so
// neither announces a length it would have to invent.
func TestAnEstimateNeedsABitrateAndSomethingLeft(t *testing.T) {
	t.Parallel()
	l := newLibrary(t)
	wav := hiFi()
	wav.Codec, wav.DurationMS = "pcm_s16le", 10_000
	f := l.add(t, "music/01 Hunter.wav", wav)

	for _, q := range []string{
		query("id", songIDOf(f.ID), "format", "flac", "estimateContentLength", "true"),
		query("id", songIDOf(f.ID), "format", "mp3", "timeOffset", "20", "estimateContentLength", "true"),
	} {
		rec := get(t, l, "stream", q)
		if got := rec.Header().Get("Content-Length"); got != "" {
			t.Errorf("%s: Content-Length = %q, want none", q, got)
		}
		if !strings.HasPrefix(rec.Body.String(), "transcoded to ") {
			t.Errorf("%s: body = %q, want a transcode", q, rec.Body.String())
		}
	}
}
