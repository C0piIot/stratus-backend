package web

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/C0piIot/stratus-backend/internal/db"
	"github.com/C0piIot/stratus-backend/internal/media"
)

// infoPrefix is where what is known about one file is asked for. Unlike the
// rename and delete forms, this page is also a fragment: the listing opens it
// under the row rather than navigating away, because looking at a file's
// details is something somebody does to several files in a row.
const infoPrefix = "/info/"

// infoFragment is what htmx asks for from a listing: the same facts in a row of
// the table they were opened from, without the document around them.
const infoFragment = "detail"

// infoView is one file and what is known about it: the facts the file row
// carries, the facts an extractor found, and the sentence that stands in for
// the second half when there is none.
type infoView struct {
	Name  string
	Href  string
	Back  string
	Facts []fact
	Media []fact
	Note  string
}

// info is what the index and the file row say about one thing.
func (h *handler) info(w http.ResponseWriter, r *http.Request, user string) {
	target, f, ok := h.targetOf(w, r, user)
	if !ok {
		return
	}

	iv := infoView{
		Name:  path.Base(target),
		Href:  href(target),
		Back:  href(db.ParentOf(target)),
		Facts: fileFacts(f),
	}
	if f.IsDir {
		iv.Note = "A folder is a row and nothing else: there are no bytes here for the indexer to read."
	} else {
		iv.Media, iv.Note = h.extracted(r, f)
	}

	v := view{Title: iv.Name, User: user, Info: &iv}
	if r.Header.Get("HX-Request") == "true" {
		h.renderTemplate(w, http.StatusOK, pageInfo, infoFragment, v)
		return
	}
	h.render(w, http.StatusOK, pageInfo, v)
}

// extracted is what the indexer made of f: the facts, and the sentence that
// says why there are none or why these are not the whole story.
//
// The two are not alternatives. A row written by an older extractor still
// describes the bytes that are there, so it is shown and said to be stale; a
// row written from bytes that have since been replaced describes a file that is
// gone, so it is not shown at all.
func (h *handler) extracted(r *http.Request, f db.File) ([]fact, string) {
	m, err := h.indexing.Index.MediaByFile(r.Context(), f.ID)
	switch {
	case errors.Is(err, db.ErrNotFound):
		return nil, "Nothing has read this file yet. The indexer works in the background; /status says how far it has got."
	case err != nil:
		// The judgement the listing's marks make, for the same reason: this is
		// half a page about a file, and refusing to render the half that came
		// out of the file row would be the worse page.
		slog.ErrorContext(r.Context(), "reading what was indexed", "path", f.Path, "err", err)
		return nil, "What the indexer found could not be read just now."
	case m.ETag != f.ETag:
		return nil, "This file has been replaced since it was read, so what was extracted describes bytes that are gone. It is queued to be read again."
	case !m.Indexed():
		return nil, unreadable(m)
	case m.Version < media.Version:
		return mediaFacts(m), fmt.Sprintf(
			"Read by extractor %d, where this build has %d: it is queued to be read again, and may have more to say afterwards.",
			m.Version, media.Version)
	case m.Kind == db.KindOther:
		// After the version, deliberately: a file is filed by its first bytes
		// now, and the kind a better extractor gives it is exactly what #146
		// was about -- a camcorder's recording that had been "other" since the
		// day it arrived.
		return mediaFacts(m), "There is nothing in a file of this kind for the indexer to extract."
	}
	return mediaFacts(m), ""
}

// unreadable says what happened to a file nothing could parse, and whether
// anything will try again -- which is the difference between a verdict on the
// file and a verdict on our reach (#157).
func unreadable(m db.Media) string {
	if m.RetryAt.IsZero() {
		return "Nothing could be extracted from this file: " + m.Error
	}
	return "Reading this file did not work: " + m.Error + ". It will be tried again after " + when(m.RetryAt) + "."
}

// fact is one line of the page: everything is already rendered, so the template
// prints and decides nothing.
type fact struct {
	Label string
	Value string
}

// facts collects them. add drops what is not known, which is the whole of
// "empty fields are omitted rather than printed as a zero": an unknown bitrate
// and a bitrate of nothing are the same value in the row and must not be the
// same line on the page.
type facts []fact

func (f *facts) add(label, value string) {
	if value != "" {
		*f = append(*f, fact{Label: label, Value: value})
	}
}

// fileFacts is what the file row itself says, which is all there is for
// anything the indexer does not read.
func fileFacts(f db.File) []fact {
	var out facts
	if f.IsDir {
		out.add("Modified", when(f.MTime))
		return out
	}
	out.add("Type", f.MIMEType)
	out.add("Size", humanSize(f.Size)+" ("+strconv.FormatInt(f.Size, 10)+" bytes)")
	out.add("Modified", when(f.MTime))
	out.add("ETag", f.ETag)
	return out
}

// mediaFacts is the extracted half, by kind. A kind with no extractor of its
// own still says when it was looked at, which is what tells "there was nothing
// in it" from "nothing has been near it".
func mediaFacts(m db.Media) []fact {
	var out facts
	switch m.Kind {
	case db.KindImage:
		out.add("Taken", when(m.TakenAt))
		out.add("Dimensions", dimensions(m.Width, m.Height))
		out.add("Camera", m.Camera)
		out.add("Location", coordinates(m.GPS))
	case db.KindAudio:
		out.add("Duration", duration(m.DurationMS))
		out.add("Codec", codec(m.Codec, m.CodecProfile, 0))
		out.add("Bitrate", bitrate(m.Bitrate))
		out.add("Sample rate", sampleRate(m.SampleRate))
		out.add("Channels", channels(m.Channels))
		out.add("Bit depth", bitDepth(m.BitDepth))
		out.add("Title", m.Title)
		out.add("Artist", m.Artist)
		out.add("Album", m.Album)
		out.add("Album artist", otherName(m.AlbumArtist, m.Artist))
		out.add("Genre", m.Genre)
		out.add("Track", count(m.TrackNo))
		out.add("Disc", count(m.DiscNo))
		out.add("Year", count(m.Year))
	case db.KindVideo:
		out.add("Duration", duration(m.DurationMS))
		out.add("Dimensions", dimensions(m.Width, m.Height))
		out.add("Recorded", when(m.TakenAt))
		out.add("Picture", codec(m.Codec, m.CodecProfile, m.Level))
		out.add("Frame rate", frameRate(m.FrameRate))
		out.add("Bitrate", bitrate(m.Bitrate))
		out.add("Bit depth", bitDepth(m.BitDepth))
		out.add("Colour", colour(m))
		out.add("Sound", sound(m))
	}
	out.add("Read", when(m.IndexedAt))
	return out
}

// when is a time somebody reads, and nothing at all for the zero one -- which
// is not a time but the absence of one.
func when(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format("2 January 2006, 15:04")
}

func dimensions(w, h int) string {
	if w <= 0 || h <= 0 {
		return ""
	}
	return fmt.Sprintf("%d × %d", w, h)
}

// duration is minutes and seconds, with hours in front of them only when there
// are any: 3:07 for a song and 1:52:30 for a film.
func duration(ms int64) string {
	if ms <= 0 {
		return ""
	}
	total := time.Duration(ms) * time.Millisecond
	hours, minutes, seconds := int(total.Hours()), int(total.Minutes())%60, int(total.Seconds())%60
	if hours > 0 {
		return fmt.Sprintf("%d:%02d:%02d", hours, minutes, seconds)
	}
	return fmt.Sprintf("%d:%02d", minutes, seconds)
}

// codec is the stream in the words ffprobe used, with the level spelled the way
// the specification writes it: 41 is 4.1.
func codec(name, profile string, level int) string {
	if name == "" {
		return ""
	}
	out := name
	if profile != "" {
		out += " " + profile
	}
	if level > 0 {
		out += fmt.Sprintf(" level %d.%d", level/10, level%10)
	}
	return out
}

func bitrate(bps int) string {
	switch {
	case bps <= 0:
		return ""
	case bps >= 1_000_000:
		return strconv.FormatFloat(float64(bps)/1e6, 'f', 1, 64) + " Mbps"
	default:
		return strconv.Itoa(bps/1000) + " kbps"
	}
}

func sampleRate(hz int) string {
	if hz <= 0 {
		return ""
	}
	return strconv.FormatFloat(float64(hz)/1000, 'f', -1, 64) + " kHz"
}

// frameRate takes what the row holds, which is frames per thousand seconds so
// that 29.97 is an integer.
func frameRate(per1000s int) string {
	if per1000s <= 0 {
		return ""
	}
	return strconv.FormatFloat(float64(per1000s)/1000, 'f', -1, 64) + " fps"
}

func channels(n int) string {
	switch n {
	case 0:
		return ""
	case 1:
		return "mono"
	case 2:
		return "stereo"
	default:
		return strconv.Itoa(n) + " channels"
	}
}

func bitDepth(bits int) string {
	if bits <= 0 {
		return ""
	}
	return strconv.Itoa(bits) + "-bit"
}

// colour is ffprobe's three names and the Dolby Vision profile: what tells HDR
// from SDR, and the reason the row carries them at all (#50).
func colour(m db.Media) string {
	parts := make([]string, 0, 4)
	for _, p := range []string{m.ColorPrimaries, m.ColorTransfer, m.ColorSpace} {
		if p != "" {
			parts = append(parts, p)
		}
	}
	if m.DoViProfile > 0 {
		parts = append(parts, "Dolby Vision profile "+strconv.Itoa(m.DoViProfile))
	}
	return strings.Join(parts, ", ")
}

// sound is a video's audio track, which is the half that most often needs a
// transcode while the picture does not.
func sound(m db.Media) string {
	parts := make([]string, 0, 3)
	for _, p := range []string{m.AudioCodec, channels(m.Channels), sampleRate(m.SampleRate)} {
		if p != "" {
			parts = append(parts, p)
		}
	}
	return strings.Join(parts, ", ")
}

func count(n int) string {
	if n <= 0 {
		return ""
	}
	return strconv.Itoa(n)
}

// otherName is a tag worth a line only when it says something the line above it
// did not: an album artist equal to the track's artist is the common case and
// repeating it is noise.
func otherName(name, same string) string {
	if name == same {
		return ""
	}
	return name
}

// coordinates are five decimal places, which is about a metre -- enough to say
// where a photograph was taken and no more precision than EXIF deserves.
func coordinates(g *db.GPS) string {
	if g == nil {
		return ""
	}
	return fmt.Sprintf("%.5f, %.5f", g.Latitude, g.Longitude)
}
