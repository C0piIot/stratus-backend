package media

import (
	"encoding/binary"
	"io"
	"iter"
	"math"
	"path"
	"strings"
	"time"

	"github.com/C0piIot/stratus-backend/internal/db"
)

// Reading a Matroska file's shape out of its head, over ranges.
//
// The twin of internal/media/mp4.go and the same argument: a film in a bucket
// costs a few kilobytes to describe rather than a few gigabytes to download.
// What makes it possible here is that Matroska states its duration -- measured,
// in #145: the first megabyte of a thirty-second file gives the same 30.0
// seconds the whole file does, while AVI and MPEG-TS give 7.5 and 6.9, because
// those have no duration to state and ffprobe works it out from what it can
// reach. That is why this reads Matroska and nothing else.
//
// EBML is not ISOBMFF but it is not a different idea either: an element is an
// identifier, a length and its data, with the first two written as
// variable-length integers. Info and Tracks sit in the first few hundred bytes
// of every file a muxer writes -- they have to, or the file could not be played
// -- and the clusters that hold the frames are stepped over by arithmetic.

// The identifiers this reads. Written out rather than looked up: there are
// eleven of them and a table would be longer than the switch that uses it.
const (
	idEBMLHeader    = 0x1A45DFA3
	idSegment       = 0x18538067
	idInfo          = 0x1549A966
	idTimecodeScale = 0x2AD7B1
	idDuration      = 0x4489
	idDateUTC       = 0x4461
	idTracks        = 0x1654AE6B
	idTrackEntry    = 0xAE
	idTrackType     = 0x83
	idCodecID       = 0x86
	idVideo         = 0xE0
	idPixelWidth    = 0xB0
	idPixelHeight   = 0xBA
	idProjection    = 0x7670
	idCluster       = 0x1F43B675
)

// trackTypeVideo is what a picture track says it is. Audio is 2, and a film
// carries both.
const trackTypeVideo = 1

// defaultTimecodeScale is the nanosecond unit durations are counted in when the
// file does not say, which is the specification's default and what every muxer
// writes anyway.
const defaultTimecodeScale = 1_000_000

// epoch2001 is the distance from Matroska's epoch -- midnight, 1 January 2001,
// UTC -- to the Unix one.
const epoch2001 = 978_307_200

// maxElement bounds what will be pulled into memory. Info and Tracks are a few
// hundred bytes; anything claiming more than this is a file this reader has no
// business interpreting.
const maxElement = 8 << 20

// matroskaExtensions are the names this reader claims.
var matroskaExtensions = []string{".mkv", ".webm"}

// codecIDs maps the strings Matroska stores onto the names ffprobe reports,
// because the two paths write the same column and a row must not say which one
// produced it. Anything else is not guessed at: the file goes to ffprobe.
//
// V_MS/VFW/FOURCC is deliberately absent. It carries another codec's fourcc
// inside a structure this does not read, so the honest answer is to stand
// aside.
var codecIDs = map[string]string{
	"V_MPEG4/ISO/AVC":  "h264",
	"V_MPEGH/ISO/HEVC": "hevc",
	"V_VP8":            "vp8",
	"V_VP9":            "vp9",
	"V_AV1":            "av1",
	"V_MPEG4/ISO/ASP":  "mpeg4",
	"V_MPEG2":          "mpeg2video",
	"V_MPEG1":          "mpeg1video",
	"V_THEORA":         "theora",
}

// matroska reports whether probeMatroska will try this name.
func matroska(name string) bool {
	ext := strings.ToLower(path.Ext(name))
	for _, want := range matroskaExtensions {
		if ext == want {
			return true
		}
	}
	return false
}

// probeMatroska reads duration, dimensions, codec and creation time out of a
// Matroska or WebM file, or returns errNotRead for the caller to decide what to
// do instead.
func probeMatroska(r io.ReadSeeker, size int64) (db.Media, error) {
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return db.Media{}, errNotRead
	}

	// The header says this is EBML at all, and is stepped over: what is in it
	// is the doctype and version numbers, and the sniffer has already agreed
	// with all of that.
	id, length, err := element(r)
	if err != nil || id != idEBMLHeader {
		return db.Media{}, errNotRead
	}
	if _, err := r.Seek(length, io.SeekCurrent); err != nil {
		return db.Media{}, errNotRead
	}

	if id, _, err := element(r); err != nil || id != idSegment {
		return db.Media{}, errNotRead
	}

	// Inside the segment, header by header. The clusters are the film and are
	// never read: in practice the loop stops at the first one, because Info and
	// Tracks come before it in any file that can be played.
	var m db.Media
	var haveInfo, haveTrack bool
	for range 64 {
		id, length, err := element(r)
		if err != nil {
			break
		}

		switch id {
		case idCluster:
			// The film itself. Info and Tracks come before the first cluster in
			// any file that can be played, so arriving here means the answer is
			// not in this file -- and the loop below stops rather than reading
			// what it came to avoid reading.

		case idInfo, idTracks:
			if length > maxElement {
				return db.Media{}, errNotRead
			}
			body := make([]byte, length)
			if _, err := io.ReadFull(r, body); err != nil {
				return db.Media{}, errNotRead
			}
			if id == idInfo {
				haveInfo = segmentInfo(body, &m)
			} else {
				haveTrack = videoTrackOf(body, &m)
			}

		default:
			if _, err := r.Seek(length, io.SeekCurrent); err != nil {
				return db.Media{}, errNotRead
			}
			continue
		}

		if id == idCluster || (haveInfo && haveTrack) {
			break
		}
	}

	if !haveInfo || !haveTrack {
		return db.Media{}, errNotRead
	}
	m.Kind = db.KindVideo
	return m, nil
}

// segmentInfo reads the duration and the recording date, and reports whether
// there was a duration to read: a file that does not state one is exactly the
// case this reader must not answer for.
func segmentInfo(body []byte, m *db.Media) bool {
	scale := float64(defaultTimecodeScale)
	var duration float64

	for id, data := range elements(body) {
		switch id {
		case idTimecodeScale:
			if n := uinteger(data); n > 0 {
				scale = float64(n)
			}
		case idDuration:
			duration = double(data)
		case idDateUTC:
			m.TakenAt = matroskaTime(data)
		}
	}

	if duration <= 0 {
		return false
	}
	// Duration counts in units of the timecode scale, which counts in
	// nanoseconds.
	m.DurationMS = int64(duration * scale / 1e6)
	return m.DurationMS > 0
}

// videoTrackOf fills in the first picture track and reports whether it was
// whole.
func videoTrackOf(tracks []byte, m *db.Media) bool {
	for id, entry := range elements(tracks) {
		if id != idTrackEntry {
			continue
		}

		var codec string
		var video []byte
		kind := int64(0)
		for id, data := range elements(entry) {
			switch id {
			case idTrackType:
				kind = int64(uinteger(data)) //nolint:gosec // one byte in every file there is
			case idCodecID:
				codec = strings.TrimRight(string(data), "\x00")
			case idVideo:
				video = data
			}
		}
		if kind != trackTypeVideo || video == nil {
			continue
		}

		name, known := codecIDs[codec]
		if !known {
			// Not guessed at, for the reason the MP4 reader gives: the row
			// would say h264 about something else and nothing downstream could
			// tell.
			return false
		}
		return videoSettings(video, name, m)
	}
	return false
}

// videoSettings reads the size out of a track's video settings.
func videoSettings(video []byte, codec string, m *db.Media) bool {
	var width, height uint64
	for id, data := range elements(video) {
		switch id {
		case idPixelWidth:
			width = uinteger(data)
		case idPixelHeight:
			height = uinteger(data)
		case idProjection:
			// Where Matroska keeps rotation, among other things. Answering
			// without reading it would hand back a portrait film as landscape,
			// which is the silent wrong answer #148 removed from the other
			// side, so the file goes to ffprobe instead.
			return false
		}
	}

	m.Codec = codec
	m.Width, m.Height = int(width), int(height) //nolint:gosec // bounded by the file's own header
	return width > 0 && height > 0
}

// element reads one element header at the current position: its identifier and
// how many bytes of data follow.
func element(r io.Reader) (uint32, int64, error) {
	id, err := vint(r, true)
	if err != nil {
		return 0, 0, err
	}
	length, err := vint(r, false)
	if err != nil {
		return 0, 0, err
	}
	if length < 0 {
		// An unknown length, which is what a live broadcast writes. There is
		// nothing to step over with, so this reader stops.
		return 0, 0, errNotRead
	}
	return uint32(id), length, nil //nolint:gosec // an identifier is four bytes at most
}

// vint reads a variable-length integer. The first byte says how long it is by
// where its first set bit falls; an identifier keeps that marker and a length
// strips it, which is the one difference between the two.
//
// A length whose value bits are all set means "unknown", which this reports as
// a negative so the caller can stop.
func vint(r io.Reader, keepMarker bool) (int64, error) {
	var first [1]byte
	if _, err := io.ReadFull(r, first[:]); err != nil {
		return 0, err
	}

	width, mask := 1, byte(0x80)
	for width <= 8 && first[0]&mask == 0 {
		mask >>= 1
		width++
	}
	if width > 8 {
		return 0, errNotRead
	}

	value := int64(first[0])
	if !keepMarker {
		value = int64(first[0] & (mask - 1))
	}

	rest := make([]byte, width-1)
	if _, err := io.ReadFull(r, rest); err != nil {
		return 0, err
	}
	allOnes := !keepMarker && first[0]&(mask-1) == mask-1
	for _, b := range rest {
		value = value<<8 | int64(b)
		allOnes = allOnes && b == 0xFF
	}
	if allOnes {
		return -1, nil
	}
	return value, nil
}

// elements walks the children of an element already in memory. It stops at the
// first malformed one rather than guessing, the same way the box walker does.
func elements(buf []byte) iter.Seq2[uint32, []byte] {
	return func(yield func(uint32, []byte) bool) {
		for i := 0; i < len(buf); {
			id, idLen := readVint(buf[i:], true)
			if idLen == 0 {
				return
			}
			length, sizeLen := readVint(buf[i+idLen:], false)
			if sizeLen == 0 || length < 0 {
				return
			}

			from := i + idLen + sizeLen
			if from < 0 || int64(from)+length > int64(len(buf)) {
				return
			}
			if !yield(uint32(id), buf[from:int64(from)+length]) { //nolint:gosec // four bytes at most
				return
			}
			i = from + int(length)
		}
	}
}

// readVint is vint over a buffer, returning how many bytes it used and zero
// when there are not enough.
func readVint(buf []byte, keepMarker bool) (int64, int) {
	if len(buf) == 0 {
		return 0, 0
	}

	width, mask := 1, byte(0x80)
	for width <= 8 && buf[0]&mask == 0 {
		mask >>= 1
		width++
	}
	if width > 8 || len(buf) < width {
		return 0, 0
	}

	value := int64(buf[0])
	if !keepMarker {
		value = int64(buf[0] & (mask - 1))
	}
	allOnes := !keepMarker && buf[0]&(mask-1) == mask-1
	for _, b := range buf[1:width] {
		value = value<<8 | int64(b)
		allOnes = allOnes && b == 0xFF
	}
	if allOnes {
		return -1, width
	}
	return value, width
}

// uinteger reads the big-endian unsigned integer Matroska stores in as few
// bytes as it needs.
func uinteger(data []byte) uint64 {
	var out uint64
	for _, b := range data {
		out = out<<8 | uint64(b)
	}
	return out
}

// double reads a float, which Matroska writes at either width.
func double(data []byte) float64 {
	switch len(data) {
	case 4:
		return float64(math.Float32frombits(binary.BigEndian.Uint32(data)))
	case 8:
		return math.Float64frombits(binary.BigEndian.Uint64(data))
	default:
		return 0
	}
}

// matroskaTime turns a date counted in nanoseconds from 2001 into a time, and
// zero into "unknown" rather than into 2001.
func matroskaTime(data []byte) time.Time {
	if len(data) != 8 {
		return time.Time{}
	}
	nanos := int64(binary.BigEndian.Uint64(data)) //nolint:gosec // signed in the format too
	if nanos == 0 {
		return time.Time{}
	}
	return time.Unix(epoch2001+nanos/1e9, nanos%1e9).UTC()
}
