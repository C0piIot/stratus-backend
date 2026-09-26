package media

import (
	"encoding/binary"
	"errors"
	"io"
	"iter"
	"math"
	"path"
	"strings"
	"time"

	"github.com/C0piIot/stratus-backend/internal/db"
)

// Reading a video's shape out of the container, in Go and over ranges.
//
// ffprobe needs a local file -- it seeks, and an MP4 whose moov atom sits at
// the end cannot be read from a stream -- so probing a video meant copying it
// first: on S3, downloading a four-gigabyte recording to learn that it is four
// minutes of 4K. This reads the same four facts out of the same boxes with
// three ranged reads of a few hundred kilobytes, on any backend and at any
// size, which is what #48 was about.
//
// It is the argument internal/media/embedded.go already makes for cover art and
// internal/media/image.go for EXIF, applied to the one case where the file is
// measured in gigabytes rather than megabytes. The atom walker it uses is the
// same one.
//
// **ffprobe is still the answer for everything else**, and for anything here
// that is not certain: Matroska, AVI, WMV and MPEG-TS are copied and probed as
// before, and so is an MP4 whose codec this does not recognise. A wrong duration
// is worse than a slow one.

// errNotRead means the readers here cannot answer for this file, which is not a
// failure: the caller decides what to do instead, which is ffprobe over a local
// copy when the file is small enough to be worth copying and nothing at all
// when it is not.
//
// Shared by both readers -- this one and the Matroska one next door -- because
// it is the same sentence in both cases: ask somebody else.
var errNotRead = errors.New("media: not a container this reads")

// maxMoov bounds what will be pulled into memory. A moov atom carries the
// sample tables, so it grows with the length of the recording: a couple of
// hundred kilobytes for a phone video, a few megabytes for hours of 4K. Beyond
// this something is wrong, or it is cheaper to let ffprobe have the file.
const maxMoov = 32 << 20

// isobmffExtensions are the names this reader claims. QuickTime and MP4 are the
// same box structure, which is also why HEIC is read through the mov demuxer.
// Not .m4a: an audio file's metadata is in tags this does not read, and it is
// megabytes rather than gigabytes, so there is nothing to win.
var isobmffExtensions = []string{".mp4", ".m4v", ".mov"}

// fourccCodecs maps a sample entry onto the name ffprobe reports, because the
// two paths write the same column and a row must not say which one produced it.
// A format that is not here is not guessed at: the file goes to ffprobe.
var fourccCodecs = map[string]string{
	"avc1": "h264",
	"avc3": "h264",
	"hvc1": "hevc",
	"hev1": "hevc",
	"av01": "av1",
	"vp09": "vp9",
	"mp4v": "mpeg4",
}

// isobmff reports whether probeVideo will try this name.
func isobmff(name string) bool {
	ext := strings.ToLower(path.Ext(name))
	for _, want := range isobmffExtensions {
		if ext == want {
			return true
		}
	}
	return false
}

// probeVideo reads duration, dimensions, codec, rotation and creation time out
// of an ISOBMFF file, or returns errNotRead for the caller to fall back on
// ffprobe.
//
// size is the file's, which is what says where the top-level boxes end.
func probeVideo(r io.ReadSeeker, size int64) (db.Media, error) {
	moov, err := readMoov(r, size)
	if err != nil {
		return db.Media{}, err
	}

	header, ok := box(moov, "mvhd")
	if !ok {
		return db.Media{}, errNotRead
	}
	duration, taken, ok := movieHeader(header)
	if !ok {
		return db.Media{}, errNotRead
	}

	m := db.Media{Kind: db.KindVideo, DurationMS: duration, TakenAt: taken}
	// The whole file over its duration, which is how ffprobe states a
	// container's bitrate and what a network has to carry.
	m.Bitrate = int(size * 8 * 1000 / duration)

	var picture, sound bool
	for name, body := range atoms(moov) {
		if name != "trak" {
			continue
		}
		switch handler(body) {
		case "vide":
			if picture {
				continue
			}
			if !videoTrack(body, &m) {
				return db.Media{}, errNotRead
			}
			picture = true
		case "soun":
			// The first one, as ffprobe's first audio stream is. What this
			// cannot name stays unknown rather than sending the whole film to
			// ffprobe: a copy is what this reader exists to avoid.
			if !sound {
				soundTrack(body, &m)
				sound = true
			}
		}
	}
	if !picture {
		// Sound with an .mp4 name, or a file with no track at all. Neither is
		// this reader's to answer for.
		return db.Media{}, errNotRead
	}
	return m, nil
}

// readMoov returns the movie box, whole.
//
// Finding it costs one ranged read per top-level box, because a box says how
// long it is before it says anything else: mdat -- the gigabytes -- is stepped
// over by arithmetic and never read. Then moov is read in one go rather than
// walked in place, because every seek backwards is another request and it is
// small enough to hold.
func readMoov(r io.ReadSeeker, size int64) ([]byte, error) {
	_, length, err := findMoov(r, size)
	if err != nil {
		return nil, err
	}

	moov := make([]byte, length)
	if _, err := io.ReadFull(r, moov); err != nil {
		return nil, errNotRead
	}
	return moov, nil
}

// findMoov leaves the reader at the first byte of the movie box and says where
// that is and how long it runs.
//
// The offset is what the window in window.go needs and the body is what the
// prober needs, so the walk is here and the reading is one caller up.
func findMoov(r io.ReadSeeker, size int64) (offset, length int64, err error) {
	if _, serr := r.Seek(0, io.SeekStart); serr != nil {
		return 0, 0, errNotRead
	}
	length, err = findAtom(r, size, "moov")
	if err != nil {
		return 0, 0, errNotRead
	}
	if length <= 0 || length > maxMoov {
		return 0, 0, errNotRead
	}

	at, err := r.Seek(0, io.SeekCurrent)
	if err != nil {
		return 0, 0, errNotRead
	}
	return at, length, nil
}

// movieHeader reads the duration and the creation time out of mvhd.
func movieHeader(body []byte) (durationMS int64, taken time.Time, ok bool) {
	var created uint64
	var timescale, duration uint64

	switch {
	case len(body) >= 4 && body[0] == 1 && len(body) >= 32:
		created = binary.BigEndian.Uint64(body[4:12])
		timescale = uint64(binary.BigEndian.Uint32(body[20:24]))
		duration = binary.BigEndian.Uint64(body[24:32])
	case len(body) >= 20 && body[0] == 0:
		created = uint64(binary.BigEndian.Uint32(body[4:8]))
		timescale = uint64(binary.BigEndian.Uint32(body[12:16]))
		duration = uint64(binary.BigEndian.Uint32(body[16:20]))
	default:
		return 0, time.Time{}, false
	}

	if timescale == 0 || duration == 0 {
		// A duration this cannot state is the case ffprobe is for: it can scan
		// the packets and this cannot.
		return 0, time.Time{}, false
	}
	// Milliseconds, and the multiplication first so that a short clip does not
	// round to nothing.
	durationMS = int64(duration * 1000 / timescale) //nolint:gosec // bounded by the file's own header
	return durationMS, movieTime(created), true
}

// epoch1904 is the distance from this format's epoch -- midnight, 1 January
// 1904, UTC -- to the Unix one.
const epoch1904 = 2_082_844_800

// movieTime turns one of this format's timestamps into a time, and zero into
// "unknown" rather than into 1904.
func movieTime(seconds uint64) time.Time {
	if seconds <= epoch1904 {
		return time.Time{}
	}
	return time.Unix(int64(seconds-epoch1904), 0).UTC() //nolint:gosec // checked above
}

// handler reads what kind of track this is: "vide" and "soun" are the two
// this reads. A sound track's tkhd has the dimensions of nothing.
func handler(trak []byte) string {
	hdlr, ok := box(trak, "mdia", "hdlr")
	if !ok || len(hdlr) < 12 {
		return ""
	}
	return string(hdlr[8:12])
}

// videoTrack fills in what a video track knows about itself, and reports
// whether all of it was there.
func videoTrack(trak []byte, m *db.Media) bool {
	tkhd, ok := box(trak, "tkhd")
	if !ok {
		return false
	}
	m.Orientation = trackOrientation(tkhd)

	stsd, ok := box(trak, "mdia", "minf", "stbl", "stsd")
	if !ok || len(stsd) < 8 {
		return false
	}
	// A sample description is a count and then boxes, so the first entry is one
	// and its name is the format.
	for format, entry := range atoms(stsd[8:]) {
		codec, known := fourccCodecs[format]
		if !known {
			// Deliberately not a guess. The row would say h264 about something
			// else, and nothing downstream could tell.
			return false
		}
		if len(entry) < 28 {
			return false
		}
		m.Codec = codec
		// The coded size, which is what ffprobe reports for a stream. tkhd
		// carries the display size instead, and the two differ on anamorphic
		// video.
		m.Width = int(binary.BigEndian.Uint16(entry[24:26]))
		m.Height = int(binary.BigEndian.Uint16(entry[26:28]))
		if m.Width <= 0 || m.Height <= 0 {
			return false
		}
		if len(entry) > visualSampleEntry {
			pictureConf(entry[visualSampleEntry:], m)
		}
		m.FrameRate = frameRate(trak)
		return true
	}
	return false
}

// visualSampleEntry is how long the fixed part of a picture's sample entry
// is. The boxes after it are the codec's configuration.
const visualSampleEntry = 78

// pictureConf reads profile, level and depth out of whichever configuration
// record the entry carries.
func pictureConf(children []byte, m *db.Media) {
	for name, body := range atoms(children) {
		var c streamConf
		var ok bool
		switch name {
		case "avcC":
			c, ok = avcConf(body)
		case "hvcC":
			c, ok = hevcConf(body)
		case "vpcC":
			// A full box: version and flags before the record.
			if len(body) > 4 {
				c, ok = vp9Conf(body[4:])
			}
		case "av1C":
			c, ok = av1Conf(body)
		default:
			continue
		}
		if ok {
			m.CodecProfile, m.Level, m.BitDepth = c.profile, c.level, c.depth
		}
		return
	}
}

// frameRate is frames per thousand seconds: the samples the track holds over
// the track's own duration, which is how ffprobe arrives at an average.
func frameRate(trak []byte) int {
	mdhd, ok := box(trak, "mdia", "mdhd")
	if !ok || len(mdhd) < 4 {
		return 0
	}
	var timescale, duration uint64
	switch {
	case mdhd[0] == 1 && len(mdhd) >= 32:
		timescale = uint64(binary.BigEndian.Uint32(mdhd[20:24]))
		duration = binary.BigEndian.Uint64(mdhd[24:32])
	case mdhd[0] == 0 && len(mdhd) >= 20:
		timescale = uint64(binary.BigEndian.Uint32(mdhd[12:16]))
		duration = uint64(binary.BigEndian.Uint32(mdhd[16:20]))
	}
	if timescale == 0 || duration == 0 {
		return 0
	}

	stts, ok := box(trak, "mdia", "minf", "stbl", "stts")
	if !ok || len(stts) < 8 {
		return 0
	}
	entries := int(binary.BigEndian.Uint32(stts[4:8]))
	var frames uint64
	for i := range entries {
		at := 8 + i*8
		if at+8 > len(stts) {
			return 0
		}
		frames += uint64(binary.BigEndian.Uint32(stts[at : at+4]))
	}
	return int(frames * timescale * 1000 / duration) //nolint:gosec // bounded by the file's own header
}

// soundCodecs maps a sound sample entry onto ffprobe's name. mp4a is not
// here: it is a family, and the descriptor inside says which member.
var soundCodecs = map[string]string{
	"ac-3": "ac3",
	"ec-3": "eac3",
	"Opus": "opus",
	"fLaC": "flac",
	"alac": "alac",
	".mp3": "mp3",
}

// soundTrack reads a sound track's codec, channels and rate, leaving unknown
// whatever its entry does not state in a form this reads.
func soundTrack(trak []byte, m *db.Media) {
	stsd, ok := box(trak, "mdia", "minf", "stbl", "stsd")
	if !ok || len(stsd) < 8 {
		return
	}
	for format, entry := range atoms(stsd[8:]) {
		if len(entry) < 28 {
			return
		}
		// A QuickTime entry grows with its version, and version 2 moves the
		// rate and the channel count into fields of its own.
		children, channels, rate := 28, int(binary.BigEndian.Uint16(entry[16:18])), int(binary.BigEndian.Uint16(entry[24:26]))
		switch binary.BigEndian.Uint16(entry[8:10]) {
		case 1:
			children = 44
		case 2:
			if len(entry) < 64 {
				return
			}
			children = 64
			rate = int(math.Float64frombits(binary.BigEndian.Uint64(entry[32:40])))
			channels = int(binary.BigEndian.Uint32(entry[40:44]))
		}
		var rest []byte
		if len(entry) > children {
			rest = entry[children:]
		}

		codec := soundCodecs[format]
		switch format {
		case "mp4a":
			var asc []byte
			codec, asc = mp4aCodec(rest)
			if n := aacChannels(asc); codec == "aac" && n > 0 {
				channels = n
			}
		case "ac-3":
			if dac3, ok := box(rest, "dac3"); ok {
				channels = ac3Channels(dac3)
			}
		case "ec-3":
			channels = 0
			if dec3, ok := box(rest, "dec3"); ok {
				channels = eac3Channels(dec3)
			}
		case "Opus":
			if dops, ok := box(rest, "dOps"); ok && len(dops) >= 2 {
				channels = int(dops[1])
			}
		}
		if codec == "" {
			return
		}
		m.AudioCodec, m.Channels, m.SampleRate = codec, channels, rate
		return
	}
}

// mp4aCodec reads an esds box: which codec the objectTypeIndication names,
// and the decoder's own configuration, which for AAC is where the channels are.
func mp4aCodec(children []byte) (string, []byte) {
	esds, ok := box(children, "esds")
	if !ok || len(esds) < 4 {
		return "", nil
	}
	b := esds[4:]

	tag, body, _ := descriptor(b)
	if tag != 0x03 || len(body) < 3 {
		return "", nil
	}
	flags := body[2]
	i := 3
	if flags&0x80 != 0 {
		i += 2
	}
	if flags&0x40 != 0 {
		if i >= len(body) {
			return "", nil
		}
		i += 1 + int(body[i])
	}
	if flags&0x20 != 0 {
		i += 2
	}
	if i > len(body) {
		return "", nil
	}
	tag, config, _ := descriptor(body[i:])
	if tag != 0x04 || len(config) < 13 {
		return "", nil
	}

	var codec string
	switch config[0] {
	case 0x40, 0x66, 0x67, 0x68:
		codec = "aac"
	case 0x69, 0x6b:
		codec = "mp3"
	}
	tag, asc, _ := descriptor(config[13:])
	if tag != 0x05 {
		asc = nil
	}
	return codec, asc
}

// descriptor reads one MPEG-4 descriptor: a tag, a length in up to four bytes
// of seven bits each, and that many bytes.
func descriptor(b []byte) (tag byte, body, rest []byte) {
	if len(b) < 2 {
		return 0, nil, nil
	}
	tag = b[0]
	size, i := 0, 1
	for ; i < len(b) && i <= 4; i++ {
		size = size<<7 | int(b[i]&0x7f)
		if b[i]&0x80 == 0 {
			i++
			break
		}
	}
	if i+size > len(b) {
		return 0, nil, nil
	}
	return tag, b[i : i+size], b[i+size:]
}

// ac3Channels reads acmod and lfeon out of a dac3 box.
func ac3Channels(dac3 []byte) int {
	if len(dac3) < 3 {
		return 0
	}
	bits := uint32(dac3[0])<<16 | uint32(dac3[1])<<8 | uint32(dac3[2])
	acmod := (bits >> 11) & 0x07
	lfe := int((bits >> 10) & 0x01)
	return acmodChannels[acmod] + lfe
}

// eac3Channels reads a dec3 box, but only the simple shape of one: a single
// independent substream with no dependents. Anything richer is unknown.
func eac3Channels(dec3 []byte) int {
	if len(dec3) < 5 {
		return 0
	}
	if dec3[1]&0x07 != 0 { // num_ind_sub is one less than the count
		return 0
	}
	bits := uint32(dec3[2])<<16 | uint32(dec3[3])<<8 | uint32(dec3[4])
	acmod := (bits >> 9) & 0x07
	lfe := int((bits >> 8) & 0x01)
	if (bits>>1)&0x0f != 0 { // num_dep_sub
		return 0
	}
	return acmodChannels[acmod] + lfe
}

// acmodChannels is how many full channels each AC-3 coding mode carries.
var acmodChannels = [8]int{2, 1, 2, 3, 3, 4, 4, 5}

// trackOrientation reads the display matrix and maps it onto the EXIF
// orientation values, so that a photograph and a video rotated the same way are
// stored the same way.
//
// Only the four square rotations are recognised, which is all a camera writes.
// The values are 16.16 fixed point, and the interesting four are a, b, c and d:
// a phone recording a portrait video writes (0, 1, -1, 0), which is the frame
// to be turned a quarter turn clockwise for display -- EXIF 6.
func trackOrientation(tkhd []byte) int {
	if len(tkhd) < 4 {
		return 0
	}
	// version and flags, then the times, the track id and the duration -- which
	// the version widens from four bytes to eight -- and then sixteen bytes of
	// layer, group and volume before the matrix.
	offset := 4 + 20 + 16
	if tkhd[0] == 1 {
		offset = 4 + 32 + 16
	}
	if len(tkhd) < offset+36 {
		return 0
	}

	const one = 1 << 16
	a := int32(binary.BigEndian.Uint32(tkhd[offset : offset+4]))     //nolint:gosec // fixed point, signed
	b := int32(binary.BigEndian.Uint32(tkhd[offset+4 : offset+8]))   //nolint:gosec // fixed point, signed
	c := int32(binary.BigEndian.Uint32(tkhd[offset+12 : offset+16])) //nolint:gosec // fixed point, signed
	d := int32(binary.BigEndian.Uint32(tkhd[offset+16 : offset+20])) //nolint:gosec // fixed point, signed

	switch {
	case a == 0 && b == one && c == -one && d == 0:
		return 6
	case a == -one && b == 0 && c == 0 && d == -one:
		return 3
	case a == 0 && b == -one && c == one && d == 0:
		return 8
	default:
		return 0
	}
}

// atoms walks the boxes in a buffer. Stops at the first malformed one rather
// than guessing: a size that runs past the end is a file this reader has no
// business interpreting.
func atoms(buf []byte) iter.Seq2[string, []byte] {
	return func(yield func(string, []byte) bool) {
		for i := 0; i+8 <= len(buf); {
			size := int64(binary.BigEndian.Uint32(buf[i : i+4]))
			name := string(buf[i+4 : i+8])
			header := int64(8)

			switch size {
			case 1:
				if i+16 > len(buf) {
					return
				}
				size = int64(binary.BigEndian.Uint64(buf[i+8 : i+16])) //nolint:gosec // bounded below
				header = 16
			case 0:
				size = int64(len(buf) - i)
			}
			if size < header || int64(i)+size > int64(len(buf)) {
				return
			}

			if !yield(name, buf[int64(i)+header:int64(i)+size]) {
				return
			}
			i += int(size)
		}
	}
}

// box walks a path of container boxes and returns the body of the last one.
// Named for what it returns rather than for the format's word, since a test
// helper next door already builds atoms.
func box(buf []byte, path ...string) ([]byte, bool) {
	for _, want := range path {
		found := false
		for name, body := range atoms(buf) {
			if name == want {
				buf, found = body, true
				break
			}
		}
		if !found {
			return nil, false
		}
	}
	return buf, true
}
