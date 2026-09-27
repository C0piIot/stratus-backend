package media

import (
	"encoding/binary"
	"errors"
	"io"
	"slices"
	"time"

	"github.com/C0piIot/stratus-backend/internal/db"
)

// Where a film's keyframes are, read out of the container's own index.
//
// A segment that copies the picture can only begin on a keyframe: anything
// else would start with frames that refer to one it does not have. So an HLS
// playlist for a remux (hls.go) is built from the keyframes, and it has to be
// built whole before the first segment is asked for, or a player could not
// seek. Every container this reads keeps that index already -- an MP4 in its
// sample table, a Matroska in its Cues -- so finding them is a few ranged
// reads of the head, and never a pass over the film.
//
// Times are presentation times, the ones ffmpeg seeks by: an MP4's decode
// times are moved by their composition offsets and by the edit list, exactly
// as ffmpeg moves them, or a seek aimed at a keyframe would land on the one
// before it.

// Index is a film's keyframes, in order, and how long it runs -- and what a
// segment needs to end cleanly, which is how many frames lie between one
// keyframe and the next in decode order. ffmpeg cuts a copied stream by decode
// time, so a segment ended by a timestamp takes the next keyframe and the
// frames around it with it; ended by a count, it stops exactly.
//
// An MP4 knows the count outright: KeySamples is each keyframe's place in
// decode order, and Samples the total. A Matroska file knows only times, and
// FrameDuration is what turns a gap between keyframes into a count -- a
// constant frame rate, which is what DefaultDuration states. With neither,
// the film is not segmented.
type Index struct {
	Keyframes     []time.Duration
	Duration      time.Duration
	KeySamples    []int
	Samples       int
	FrameDuration time.Duration
}

// errNoIndex means the container does not say where its keyframes are.
var errNoIndex = errors.New("media: the container has no keyframe index this reads")

// KeyframesOf reads the index of a file the in-place readers claim.
func KeyframesOf(r io.ReadSeeker, f db.File) (Index, error) {
	switch {
	case matroska(f.Path):
		return mkvKeyframes(r, f.Size)
	case isobmff(f.Path):
		return mp4Keyframes(r, f.Size)
	}
	return Index{}, errNoIndex
}

func mp4Keyframes(r io.ReadSeeker, size int64) (Index, error) {
	moov, err := readMoov(r, size)
	if err != nil {
		return Index{}, err
	}
	for name, trak := range atoms(moov) {
		if name != "trak" || handler(trak) != "vide" {
			continue
		}
		return trackKeyframes(trak)
	}
	return Index{}, errNoIndex
}

// trackKeyframes walks a picture track's sample table.
func trackKeyframes(trak []byte) (Index, error) {
	mdhd, ok := box(trak, "mdia", "mdhd")
	if !ok || len(mdhd) < 20 {
		return Index{}, errNoIndex
	}
	var timescale, duration uint64
	if mdhd[0] == 1 {
		if len(mdhd) < 32 {
			return Index{}, errNoIndex
		}
		timescale, duration = uint64(binary.BigEndian.Uint32(mdhd[20:24])), binary.BigEndian.Uint64(mdhd[24:32])
	} else {
		timescale, duration = uint64(binary.BigEndian.Uint32(mdhd[12:16])), uint64(binary.BigEndian.Uint32(mdhd[16:20]))
	}
	if timescale == 0 {
		return Index{}, errNoIndex
	}
	stbl, ok := box(trak, "mdia", "minf", "stbl")
	if !ok {
		return Index{}, errNoIndex
	}

	stts, ok := runs(stbl, "stts", false)
	if !ok {
		return Index{}, errNoIndex
	}
	// No ctts means every sample is shown when it is decoded.
	ctts, _ := runs(stbl, "ctts", true)
	var sync []uint32
	if stss, ok := box(stbl, "stss"); ok && len(stss) >= 8 {
		n := int(binary.BigEndian.Uint32(stss[4:8]))
		for i := range n {
			at := 8 + i*4
			if at+4 > len(stss) {
				return Index{}, errNoIndex
			}
			sync = append(sync, binary.BigEndian.Uint32(stss[at:at+4]))
		}
	}
	shift := editShift(trak)

	toTime := func(ts int64) time.Duration {
		return time.Duration(ts * int64(time.Second) / int64(timescale)) //nolint:gosec // bounded by the file's own header
	}
	var keys []time.Duration
	var keySamples []int
	var dts int64
	sample := uint32(1)
	ci, cLeft := 0, uint32(0)
	next := 0
	for _, run := range stts {
		for range run.count {
			var offset int64
			if len(ctts) > 0 {
				for cLeft == 0 && ci < len(ctts) {
					cLeft = ctts[ci].count
					ci++
				}
				if cLeft > 0 {
					offset = ctts[ci-1].value
					cLeft--
				}
			}
			// With no stss every sample is a keyframe, which is what the
			// format says and what an all-intra recording is.
			if sync == nil || (next < len(sync) && sync[next] == sample) {
				keys = append(keys, max(0, toTime(dts+offset-shift)))
				keySamples = append(keySamples, int(sample-1))
				next++
			}
			dts += run.value
			sample++
		}
	}
	if len(keys) == 0 || !slices.IsSorted(keys) {
		// Keyframes shown out of the order they are decoded in are not a thing
		// an encoder writes, and a count between them would mean nothing.
		return Index{}, errNoIndex
	}
	return Index{
		Keyframes: keys, KeySamples: keySamples, Samples: int(sample - 1),
		Duration: toTime(int64(duration) - shift), //nolint:gosec // bounded by the file's own header
	}, nil
}

// run is one entry of stts or ctts: so many samples, each this long or offset
// by this much.
type run struct {
	count uint32
	value int64
}

// runs reads a table of (count, value) pairs. ctts's value is signed in
// version 1 and read as such in both, since a version 0 offset above 2^31 is
// not a thing a muxer writes.
func runs(stbl []byte, name string, signed bool) ([]run, bool) {
	b, ok := box(stbl, name)
	if !ok || len(b) < 8 {
		return nil, false
	}
	n := int(binary.BigEndian.Uint32(b[4:8]))
	out := make([]run, 0, min(n, len(b)/8))
	for i := range n {
		at := 8 + i*8
		if at+8 > len(b) {
			return nil, false
		}
		v := int64(binary.BigEndian.Uint32(b[at+4 : at+8]))
		if signed {
			v = int64(int32(binary.BigEndian.Uint32(b[at+4 : at+8]))) //nolint:gosec // signed on purpose
		}
		out = append(out, run{count: binary.BigEndian.Uint32(b[at : at+4]), value: v})
	}
	return out, true
}

// editShift is the media time the edit list says presentation starts at,
// which ffmpeg subtracts from every timestamp -- it is how an encoder hides
// the frames B-frames need decoded before the first one is shown.
func editShift(trak []byte) int64 {
	elst, ok := box(trak, "edts", "elst")
	if !ok || len(elst) < 8 {
		return 0
	}
	n := int(binary.BigEndian.Uint32(elst[4:8]))
	width := 12
	if elst[0] == 1 {
		width = 20
	}
	for i := range n {
		at := 8 + i*width
		if at+width > len(elst) {
			return 0
		}
		var mediaTime int64
		if elst[0] == 1 {
			mediaTime = int64(binary.BigEndian.Uint64(elst[at+8 : at+16])) //nolint:gosec // signed on purpose
		} else {
			mediaTime = int64(int32(binary.BigEndian.Uint32(elst[at+4 : at+8]))) //nolint:gosec // signed on purpose
		}
		// -1 is an empty edit, a pause before the film; the first real one
		// is where it begins.
		if mediaTime >= 0 {
			return mediaTime
		}
	}
	return 0
}

// The Matroska elements the index needs beyond what mkv.go reads.
const (
	idSeekHead     = 0x114D9B74
	idSeek         = 0x4DBB
	idSeekID       = 0x53AB
	idSeekPosition = 0x53AC
	idCues         = 0x1C53BB6B
	idCuePoint     = 0xBB
	idCueTime      = 0xB3
	idCueTrackPos  = 0xB7
	idCueTrack     = 0xF7
	idTrackNumber  = 0xD7
)

// maxCues bounds the Cues element read whole: a cue per keyframe of a
// two-hour film is tens of kilobytes, and beyond this it is not an index.
const maxCues = 16 << 20

// mkvKeyframes finds the Cues -- directly when a muxer put them before the
// film, through the SeekHead when it put them after, which is the usual case
// and why stepping over clusters one header at a time is not how this looks.
func mkvKeyframes(r io.ReadSeeker, size int64) (Index, error) {
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return Index{}, errNoIndex
	}
	id, length, err := element(r)
	if err != nil || id != idEBMLHeader {
		return Index{}, errNoIndex
	}
	if _, err = r.Seek(length, io.SeekCurrent); err != nil {
		return Index{}, errNoIndex
	}
	if id, _, err = element(r); err != nil || id != idSegment {
		return Index{}, errNoIndex
	}
	segment, err := r.Seek(0, io.SeekCurrent)
	if err != nil {
		return Index{}, errNoIndex
	}

	scale := uint64(defaultTimecodeScale)
	var m db.Media
	var track uint64
	var frame time.Duration
	var cues []byte
	cuesAt := int64(-1)
	for range 64 {
		id, length, err := element(r)
		if err != nil || id == idCluster {
			break
		}
		switch id {
		case idInfo, idTracks, idSeekHead, idCues:
			limit := int64(maxElement)
			if id == idCues {
				limit = maxCues
			}
			if length > limit {
				return Index{}, errNoIndex
			}
			body := make([]byte, length)
			if _, err := io.ReadFull(r, body); err != nil {
				return Index{}, errNoIndex
			}
			switch id {
			case idInfo:
				segmentInfo(body, &m)
				for eid, data := range elements(body) {
					if eid == idTimecodeScale && uinteger(data) > 0 {
						scale = uinteger(data)
					}
				}
			case idTracks:
				track, frame = videoTrackOf(body)
			case idSeekHead:
				if pos, ok := seekTo(body, idCues); ok {
					cuesAt = segment + pos
				}
			case idCues:
				cues = body
			}
		default:
			if _, err := r.Seek(length, io.SeekCurrent); err != nil {
				return Index{}, errNoIndex
			}
		}
	}

	if cues == nil && cuesAt > 0 && cuesAt < size {
		if _, err := r.Seek(cuesAt, io.SeekStart); err != nil {
			return Index{}, errNoIndex
		}
		id, length, err := element(r)
		if err != nil || id != idCues || length > maxCues {
			return Index{}, errNoIndex
		}
		cues = make([]byte, length)
		if _, err := io.ReadFull(r, cues); err != nil {
			return Index{}, errNoIndex
		}
	}
	if cues == nil || m.DurationMS == 0 {
		return Index{}, errNoIndex
	}

	var keys []time.Duration
	for id, point := range elements(cues) {
		if id != idCuePoint {
			continue
		}
		var at uint64
		onTrack := track == 0
		for pid, data := range elements(point) {
			switch pid {
			case idCueTime:
				at = uinteger(data)
			case idCueTrackPos:
				for tid, tdata := range elements(data) {
					if tid == idCueTrack && uinteger(tdata) == track {
						onTrack = true
					}
				}
			}
		}
		if onTrack {
			keys = append(keys, time.Duration(at*scale)) //nolint:gosec // bounded by the file's own header
		}
	}
	if len(keys) == 0 {
		return Index{}, errNoIndex
	}
	slices.Sort(keys)
	return Index{
		Keyframes: slices.Compact(keys), Duration: time.Duration(m.DurationMS) * time.Millisecond,
		FrameDuration: frame,
	}, nil
}

// videoTrackOf is the number the Cues name the first picture track by, and
// how long each of its frames is when the track says.
func videoTrackOf(tracks []byte) (uint64, time.Duration) {
	for id, entry := range elements(tracks) {
		if id != idTrackEntry {
			continue
		}
		var kind, number, frame uint64
		for eid, data := range elements(entry) {
			switch eid {
			case idTrackType:
				kind = uinteger(data)
			case idTrackNumber:
				number = uinteger(data)
			case idDefaultDur:
				frame = uinteger(data)
			}
		}
		if kind == trackTypeVideo {
			return number, time.Duration(frame) //nolint:gosec // bounded by the file's own header
		}
	}
	return 0, 0
}

// seekTo finds where the SeekHead says an element is, relative to the start of
// the segment's data.
func seekTo(head []byte, want uint32) (int64, bool) {
	for id, seek := range elements(head) {
		if id != idSeek {
			continue
		}
		var target uint32
		var pos uint64
		for sid, data := range elements(seek) {
			switch sid {
			case idSeekID:
				for _, b := range data {
					target = target<<8 | uint32(b)
				}
			case idSeekPosition:
				pos = uinteger(data)
			}
		}
		if target == want {
			return int64(pos), true //nolint:gosec // bounded by the file's own header
		}
	}
	return 0, false
}
