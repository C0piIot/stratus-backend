package subsonic_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/C0piIot/stratus-backend/internal/media"
	"github.com/C0piIot/stratus-backend/internal/music"
	"github.com/C0piIot/stratus-backend/internal/subsonic"
)

// sonosBody is the ClientInfo the specification gives as its example.
const sonosBody = `{
  "name": "Play:1", "platform": "Sonos",
  "maxAudioBitrate": 512000, "maxTranscodingAudioBitrate": 256000,
  "directPlayProfiles": [
    {"containers": ["mp3"], "audioCodecs": ["mp3"], "protocols": ["http"], "maxAudioChannels": 2},
    {"containers": ["flac"], "audioCodecs": ["flac"], "protocols": [], "maxAudioChannels": 2},
    {"containers": ["mp4"], "audioCodecs": ["flac", "aac", "alac"], "protocols": [], "maxAudioChannels": 2}
  ],
  "transcodingProfiles": [
    {"container": "mp3", "audioCodec": "mp3", "protocol": "http", "maxAudioChannels": 2},
    {"container": "flac", "audioCodec": "flac", "protocol": "hls", "maxAudioChannels": 2}
  ],
  "codecProfiles": [
    {"type": "AudioCodec", "name": "mp3", "limitations": [
      {"name": "audioBitrate", "comparison": "LessThanEqual", "values": ["320000"], "required": true}]},
    {"type": "VideoCodec", "name": "h264", "limitations": []}
  ]
}`

func postDecision(t *testing.T, h http.Handler, rawQuery, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := request(t, "getTranscodeDecision", rawQuery)
	req.Method = http.MethodPost
	req.Body = http.NoBody
	if body != "" {
		req = httptest.NewRequestWithContext(t.Context(), http.MethodPost, req.URL.String(), strings.NewReader(body))
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestTranscodeDecisionForTheSpecificationsClient is the whole exchange with
// the example client: a 900 kbps FLAC is over its limit, so the answer is a
// transcode into 256 kbps MP3, with the params to fetch it by.
func TestTranscodeDecisionForTheSpecificationsClient(t *testing.T) {
	t.Parallel()
	l := newLibrary(t)
	f := l.add(t, "music/01 Hunter.flac", hiFi())

	env := response(t, postDecision(t, l, query("f", "json", "mediaId", songIDOf(f.ID), "mediaType", "song"), sonosBody))
	d, ok := env["transcodeDecision"].(map[string]any)
	if !ok {
		t.Fatalf("no transcodeDecision in %v", env)
	}
	if d["canDirectPlay"] != false || d["canTranscode"] != true {
		t.Errorf("decision = %v, want a transcode", d)
	}
	if got := d["transcodeParams"]; got != "v1-mp3-mp3-256000-0-0-0" {
		t.Errorf("transcodeParams = %v", got)
	}
	src, _ := d["sourceStream"].(map[string]any)
	dst, _ := d["transcodeStream"].(map[string]any)
	if src["container"] != "flac" || src["audioBitrate"] != 900_000.0 || src["audioBitdepth"] != 16.0 {
		t.Errorf("sourceStream = %v", src)
	}
	if dst["codec"] != "mp3" || dst["audioBitrate"] != 256_000.0 || dst["audioSamplerate"] != 44_100.0 || dst["protocol"] != "http" {
		t.Errorf("transcodeStream = %v", dst)
	}
	if reasons, _ := d["transcodeReason"].([]any); len(reasons) != 1 {
		t.Errorf("transcodeReason = %v, want the one reason: the global bitrate", d["transcodeReason"])
	}
}

// TestTranscodeDecisionForDirectPlay: an MP3 the client plays needs no params.
func TestTranscodeDecisionForDirectPlay(t *testing.T) {
	t.Parallel()
	l := newLibrary(t)
	m := hiFi()
	m.Codec, m.Bitrate = "mp3", 320_000
	f := l.add(t, "music/01 Hunter.mp3", m)

	rec := postDecision(t, l, query("mediaId", songIDOf(f.ID), "mediaType", "song"), sonosBody)
	body := rec.Body.String()
	if !strings.Contains(body, `canDirectPlay="true"`) || strings.Contains(body, "transcodeParams") ||
		!strings.Contains(body, `<sourceStream protocol="http" container="mp3" codec="mp3"`) {
		t.Errorf("XML = %s", body)
	}
}

func TestTranscodeDecisionRefuses(t *testing.T) {
	t.Parallel()
	l := newLibrary(t)
	f := l.add(t, "music/01 Hunter.flac", hiFi())
	id := songIDOf(f.ID)

	for name, c := range map[string]struct {
		q, body string
		code    float64
	}{
		"no mediaType":       {query("f", "json", "mediaId", id), sonosBody, 10},
		"a podcast":          {query("f", "json", "mediaId", id, "mediaType", "podcast"), sonosBody, 70},
		"no mediaId":         {query("f", "json", "mediaType", "song"), sonosBody, 10},
		"a body that is not": {query("f", "json", "mediaId", id, "mediaType", "song"), "{not json", 10},
		"no such song":       {query("f", "json", "mediaId", "tr-999999", "mediaType", "song"), sonosBody, 70},
	} {
		if code := errorCode(t, postDecision(t, l, c.q, c.body)); code != c.code {
			t.Errorf("%s: code = %v, want %v", name, code, c.code)
		}
	}

	// And a GET, which is not how this endpoint is asked.
	if rec := get(t, l, "getTranscodeDecision", query("f", "json", "mediaId", id, "mediaType", "song")); strings.Contains(rec.Body.String(), "transcodeDecision") {
		t.Errorf("a GET was answered with a decision: %s", rec.Body.String())
	}
}

// TestTranscodeStreamRedeemsTheParams: what the decision wrote reaches ffmpeg
// as the same Plan, with the offset.
func TestTranscodeStreamRedeemsTheParams(t *testing.T) {
	t.Parallel()
	l := newLibrary(t)
	f := l.add(t, "music/01 Hunter.flac", hiFi())

	rec := get(t, l, "getTranscodeStream", query("mediaId", songIDOf(f.ID), "mediaType", "song",
		"transcodeParams", "v1-aac-mp4-192000-48000-2-0", "offset", "30"))
	if rec.Code != http.StatusOK || rec.Body.String() != "transcoded to aac" {
		t.Fatalf("getTranscodeStream = %d %q", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Content-Type") != "audio/mp4" || rec.Header().Get("Accept-Ranges") != "none" {
		t.Errorf("headers = %v", rec.Header())
	}
	plans, offsets := l.transcoder.calls()
	if len(plans) != 1 || !plans[0].Fragmented || plans[0].Bitrate != 192_000 || offsets[0].Seconds() != 30 {
		t.Errorf("ffmpeg was asked for %+v at %v", plans, offsets)
	}
}

// TestTranscodeStreamErrorsAreStatusCodes, as the extension specifies.
func TestTranscodeStreamErrorsAreStatusCodes(t *testing.T) {
	t.Parallel()
	l := newLibrary(t)
	f := l.add(t, "music/01 Hunter.flac", hiFi())
	id := songIDOf(f.ID)
	good := "v1-mp3-mp3-128000-0-0-0"

	for name, c := range map[string]struct {
		q    string
		want int
	}{
		"no mediaType":         {query("mediaId", id, "transcodeParams", good), http.StatusBadRequest},
		"a podcast":            {query("mediaId", id, "mediaType", "podcast", "transcodeParams", good), http.StatusNotFound},
		"params never written": {query("mediaId", id, "mediaType", "song", "transcodeParams", "v1-mp3-mp3-999999-0-0-0"), http.StatusBadRequest},
		"no such song":         {query("mediaId", "tr-999999", "mediaType", "song", "transcodeParams", good), http.StatusNotFound},
	} {
		if rec := get(t, l, "getTranscodeStream", c.q); rec.Code != c.want {
			t.Errorf("%s: %d, want %d", name, rec.Code, c.want)
		}
	}

	l.transcoder.err = media.ErrBusy
	rec := get(t, l, "getTranscodeStream", query("mediaId", id, "mediaType", "song", "transcodeParams", good))
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") == "" {
		t.Errorf("a full house = %d, Retry-After %q; want 503 with one", rec.Code, rec.Header().Get("Retry-After"))
	}
	l.transcoder.err = errors.New("ffmpeg: no")
	if rec := get(t, l, "getTranscodeStream", query("mediaId", id, "mediaType", "song", "transcodeParams", good)); rec.Code != http.StatusInternalServerError {
		t.Errorf("a failing ffmpeg = %d, want 500", rec.Code)
	}

	head := request(t, "getTranscodeStream", query("mediaId", id, "mediaType", "song", "transcodeParams", good))
	head.Method = http.MethodHead
	l.transcoder.err = nil
	hr := httptest.NewRecorder()
	l.ServeHTTP(hr, head)
	if hr.Header().Get("Content-Type") != "audio/mpeg" {
		t.Errorf("HEAD Content-Type = %q", hr.Header().Get("Content-Type"))
	}
}

// TestTranscodeStreamWhenTheLibraryFails: a track that cannot be looked up is
// a 500, not a 404 that would tell a client the song is gone.
func TestTranscodeStreamWhenTheLibraryFails(t *testing.T) {
	t.Parallel()
	l := newLibrary(t)
	f := l.add(t, "music/01 Hunter.flac", hiFi())
	b := breaking{music: l.meta, tree: l.files, lists: music.New(l.meta), art: l.art, fail: "TrackByFile"}
	h := subsonic.Handler(prefix, serverVersion, l.verifier, b, b, b, b, l.transcoder)

	rec := get(t, h, "getTranscodeStream", query("mediaId", songIDOf(f.ID), "mediaType", "song",
		"transcodeParams", "v1-mp3-mp3-128000-0-0-0"))
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("getTranscodeStream over a failing library = %d, want 500", rec.Code)
	}
}
