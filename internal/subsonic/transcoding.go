package subsonic

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/C0piIot/stratus-backend/internal/media"
)

// The transcoding extension (#50): a client describes what it can play, the
// server chooses, and the client streams what was chosen.
//
// getTranscodeDecision is a POST with the client's capabilities as JSON in the
// body -- the extension's own design, since a list of profiles does not fit in
// a query string -- and the credentials on the query string as always. What
// comes back is internal/media's decision, rendered: whether the file can be
// played as it is, and if not, what it would be transcoded into and a
// transcodeParams to fetch that with.
//
// getTranscodeStream redeems it. Its errors are HTTP status codes, as the
// extension says, not an envelope: a client streaming media reads a status and
// nothing else. When every transcode slot is taken that is a 503 rather than
// the original stream sends, because this client has already been told
// whether it could play the original and chose not to.

// maxClientInfo bounds the body. A ClientInfo is a handful of short lists; a
// megabyte of it is not a client describing itself.
const maxClientInfo = 64 << 10

type clientInfo struct {
	Name                       string `json:"name"`
	Platform                   string `json:"platform"`
	MaxAudioBitrate            int    `json:"maxAudioBitrate"`
	MaxTranscodingAudioBitrate int    `json:"maxTranscodingAudioBitrate"`
	DirectPlayProfiles         []struct {
		Containers       []string `json:"containers"`
		AudioCodecs      []string `json:"audioCodecs"`
		Protocols        []string `json:"protocols"`
		MaxAudioChannels int      `json:"maxAudioChannels"`
	} `json:"directPlayProfiles"`
	TranscodingProfiles []struct {
		Container        string `json:"container"`
		AudioCodec       string `json:"audioCodec"`
		Protocol         string `json:"protocol"`
		MaxAudioChannels int    `json:"maxAudioChannels"`
	} `json:"transcodingProfiles"`
	CodecProfiles []struct {
		Type        string `json:"type"`
		Name        string `json:"name"`
		Limitations []struct {
			Name       string   `json:"name"`
			Comparison string   `json:"comparison"`
			Values     []string `json:"values"`
			Required   bool     `json:"required"`
		} `json:"limitations"`
	} `json:"codecProfiles"`
}

// capabilities is the payload in internal/media's terms, which are the same
// ones by design. Only audio codec profiles exist in version 1.
func (c clientInfo) capabilities() media.Capabilities {
	caps := media.Capabilities{MaxBitrate: c.MaxAudioBitrate, MaxTranscodeBitrate: c.MaxTranscodingAudioBitrate}
	for _, p := range c.DirectPlayProfiles {
		caps.Direct = append(caps.Direct, media.DirectProfile{
			Containers: p.Containers, Codecs: p.AudioCodecs, Protocols: p.Protocols, MaxChannels: p.MaxAudioChannels,
		})
	}
	for _, p := range c.TranscodingProfiles {
		caps.Targets = append(caps.Targets, media.TargetProfile{
			Container: p.Container, Codec: p.AudioCodec, Protocol: p.Protocol, MaxChannels: p.MaxAudioChannels,
		})
	}
	for _, p := range c.CodecProfiles {
		if p.Type != "AudioCodec" {
			continue
		}
		cp := media.CodecProfile{Codec: p.Name}
		for _, l := range p.Limitations {
			cp.Limits = append(cp.Limits, media.Limit{Name: l.Name, Comparison: l.Comparison, Values: l.Values, Required: l.Required})
		}
		caps.Codecs = append(caps.Codecs, cp)
	}
	return caps
}

type transcodeDecision struct {
	CanDirectPlay   bool           `xml:"canDirectPlay,attr" json:"canDirectPlay"`
	CanTranscode    bool           `xml:"canTranscode,attr" json:"canTranscode"`
	TranscodeReason []string       `xml:"transcodeReason" json:"transcodeReason,omitempty"`
	ErrorReason     string         `xml:"errorReason,attr,omitempty" json:"errorReason,omitempty"`
	TranscodeParams string         `xml:"transcodeParams,attr,omitempty" json:"transcodeParams,omitempty"`
	SourceStream    *streamDetails `xml:"sourceStream" json:"sourceStream"`
	TranscodeStream *streamDetails `xml:"transcodeStream,omitempty" json:"transcodeStream,omitempty"`
}

// streamDetails carries every field, zeros included: a client reads them as
// numbers, and a missing one is a parse it may not expect to make.
type streamDetails struct {
	Protocol        string `xml:"protocol,attr" json:"protocol"`
	Container       string `xml:"container,attr" json:"container"`
	Codec           string `xml:"codec,attr" json:"codec"`
	AudioChannels   int    `xml:"audioChannels,attr" json:"audioChannels"`
	AudioBitrate    int    `xml:"audioBitrate,attr" json:"audioBitrate"`
	AudioProfile    string `xml:"audioProfile,attr" json:"audioProfile"`
	AudioSamplerate int    `xml:"audioSamplerate,attr" json:"audioSamplerate"`
	AudioBitdepth   int    `xml:"audioBitdepth,attr" json:"audioBitdepth"`
}

func details(s media.Stream) *streamDetails {
	return &streamDetails{
		Protocol: s.Protocol, Container: s.Container, Codec: s.Codec, AudioChannels: s.Channels,
		AudioBitrate: s.Bitrate, AudioProfile: s.Profile, AudioSamplerate: s.SampleRate, AudioBitdepth: s.BitDepth,
	}
}

func (h *handler) transcodeDecision(w http.ResponseWriter, r *http.Request, username string) {
	q := r.URL.Query()
	if apiErr := mediaTypeOf(q.Get("mediaType")); apiErr != nil {
		h.fail(w, r, *apiErr)
		return
	}
	id := q.Get("mediaId")
	if id == "" {
		h.fail(w, r, apiError{errMissingParam, "the mediaId parameter is required"})
		return
	}

	var info clientInfo
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxClientInfo)).Decode(&info); err != nil {
		h.fail(w, r, apiError{errMissingParam, "the body must be the client's capabilities as JSON"})
		return
	}
	t, apiErr := h.audioTrack(r, username, id)
	if apiErr != nil {
		h.fail(w, r, *apiErr)
		return
	}

	d := media.DecideFor(t.File, t.Media, info.capabilities())
	out := &transcodeDecision{
		CanDirectPlay: d.Direct, CanTranscode: d.Transcode, ErrorReason: d.Error, SourceStream: details(d.Source),
	}
	if !d.Direct {
		out.TranscodeReason = d.Reasons
	}
	if d.Transcode {
		out.TranscodeParams = d.Plan.Params()
		out.TranscodeStream = details(d.Target)
	}
	env := h.ok()
	env.TranscodeDecision = out
	h.write(w, r, env)
}

// mediaTypeOf accepts what this server has: songs. A podcast is a real media
// type in the extension and one there is nothing of here.
func mediaTypeOf(t string) *apiError {
	switch t {
	case "song":
		return nil
	case "":
		return &apiError{errMissingParam, "the mediaType parameter is required"}
	}
	return ptr(notFound(t))
}

func (h *handler) transcodeStream(w http.ResponseWriter, r *http.Request, username string) {
	q := r.URL.Query()
	switch q.Get("mediaType") {
	case "song":
	case "":
		http.Error(w, "the mediaType parameter is required", http.StatusBadRequest)
		return
	default:
		http.Error(w, "there is no media of that type here", http.StatusNotFound)
		return
	}
	plan, err := media.PlanFromParams(q.Get("transcodeParams"))
	if err != nil {
		http.Error(w, "transcodeParams is not one getTranscodeDecision gave", http.StatusBadRequest)
		return
	}
	t, apiErr := h.audioTrack(r, username, q.Get("mediaId"))
	switch {
	case apiErr != nil && apiErr.Code == errNotFound:
		http.Error(w, apiErr.Message, http.StatusNotFound)
		return
	case apiErr != nil:
		http.Error(w, apiErr.Message, http.StatusInternalServerError)
		return
	}

	if r.Method == http.MethodHead {
		transcodeHeaders(w.Header(), plan, -1)
		return
	}
	offset := time.Duration(intParam(q, "offset", 0)) * time.Second
	out, err := h.transcoder.Transcode(r.Context(), t.File, plan, offset)
	switch {
	case errors.Is(err, media.ErrBusy):
		w.Header().Set("Retry-After", "5")
		http.Error(w, "every transcode this server allows is running", http.StatusServiceUnavailable)
		return
	case err != nil:
		slog.ErrorContext(r.Context(), "subsonic: cannot transcode a track", "path", t.File.Path, "err", err)
		http.Error(w, "the track could not be transcoded", http.StatusInternalServerError)
		return
	}
	defer func() { _ = out.Close() }()
	transcodeHeaders(w.Header(), plan, -1)
	_, _ = io.Copy(w, out)
}
