package thumb

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"

	"github.com/stratus-private-cloud/stratus-backend/internal/media"
)

// Reading the picture out of a track, in Go and off the blob.
//
// The other way was to extend the trimmed ffmpeg with the audio demuxers and an
// image muxer and copy the stream out. This is better for a reason that has
// nothing to do with taste: the storage port reads ranges, so a parser that
// seeks takes the first few kilobytes of a FLAC and stops, while ffmpeg needs a
// local file and would media.Spool a 50 MB track out of a bucket to lift a 200 KB
// picture out of its head. internal/media/image.go reads EXIF the same way and
// for the same reason.
//
// Four containers cover a real library: FLAC's PICTURE block, ID3v2's APIC
// frame, MP4's covr atom, and the comment header of an Ogg stream -- Opus and
// Vorbis -- where the same PICTURE block is kept base64-encoded under
// METADATA_BLOCK_PICTURE. That last one is what yt-dlp writes with
// --embed-thumbnail, which makes it most of a library downloaded that way.

// ErrNoEmbeddedCover means the track carries no picture. It is not a failure:
// most tracks in most libraries do not.
var ErrNoEmbeddedCover = errors.New("media: no embedded cover")

// maxEmbeddedCover bounds what will be read out of a tag. Album art is a few
// hundred kilobytes; the length fields these formats use are 32 bits wide, and a
// corrupt one claiming four gigabytes is the cheapest way to exhaust a server.
const maxEmbeddedCover = 16 << 20

// maxTagScan bounds how far into a file the search goes. Every one of these
// formats keeps its metadata at the head, so a picture that is not in the first
// few megabytes is not a picture we are looking for -- and the bound is what
// keeps a malformed length from turning a search into a full download.
const maxTagScan = 64 << 20

// frontCover is the picture type both FLAC and ID3v2 use for the front of the
// record. A file often carries the back and the disc as well, so it is preferred
// rather than assumed: the first picture in a file is not reliably the cover.
const frontCover = 3

// embeddedCover returns the artwork inside a track, or ErrNoEmbeddedCover.
//
// The extension picks the parser. A file's magic number would be more robust
// against a misnamed file, but every one of these containers is identified by
// its head anyway and the parsers check that -- so a .flac that is really an
// MP3 fails the FLAC magic and is answered as having no picture, which is the
// honest result for a file whose name lies.
func embeddedCover(r io.ReadSeeker, name string) ([]byte, error) {
	switch strings.ToLower(path.Ext(name)) {
	case ".flac":
		return flacCover(r)
	case ".mp3", ".aac":
		return id3Cover(r)
	case ".m4a", ".mp4", ".m4v", ".mov":
		return mp4Cover(r)
	case ".opus", ".ogg", ".oga":
		return oggCover(r)
	}
	return nil, fmt.Errorf("%w: %s", ErrNoEmbeddedCover, path.Ext(name))
}

// flacCover walks the metadata blocks at the head of a FLAC.
//
// The layout is a one-byte header whose top bit says "last block" and whose
// bottom seven are the type, then a 24-bit length. Type 6 is a picture.
func flacCover(r io.ReadSeeker) ([]byte, error) {
	var magic [4]byte
	if _, err := io.ReadFull(r, magic[:]); err != nil || string(magic[:]) != "fLaC" {
		return nil, fmt.Errorf("%w: not a FLAC", ErrNoEmbeddedCover)
	}

	var first []byte
	for scanned := int64(0); scanned < maxTagScan; {
		var header [4]byte
		if _, err := io.ReadFull(r, header[:]); err != nil {
			break
		}
		last := header[0]&0x80 != 0
		kind := header[0] & 0x7f
		length := int64(header[1])<<16 | int64(header[2])<<8 | int64(header[3])
		scanned += 4 + length

		if kind != 6 {
			if _, err := r.Seek(length, io.SeekCurrent); err != nil {
				break
			}
			if last {
				break
			}
			continue
		}

		block := make([]byte, min(length, maxEmbeddedCover))
		if _, err := io.ReadFull(r, block); err != nil {
			break
		}
		kindOfPicture, picture, err := flacPicture(block)
		if err == nil {
			if kindOfPicture == frontCover {
				return picture, nil
			}
			if first == nil {
				first = picture
			}
		}
		if last {
			break
		}
	}

	if first != nil {
		// No front cover declared, so the first picture in the file it is --
		// which is what it will be in practice for a file with only one.
		return first, nil
	}
	return nil, ErrNoEmbeddedCover
}

// flacPicture reads one PICTURE block: a type, two length-prefixed strings,
// four dimensions nobody here needs, and the bytes.
func flacPicture(block []byte) (kind uint32, picture []byte, err error) {
	b := reader{buf: block}

	kind = b.uint32()
	b.skip(int64(b.uint32()))   // the MIME type, which the decoder does not need
	b.skip(int64(b.uint32()))   // the description
	b.skip(16)                  // width, height, depth, colours
	length := int64(b.uint32()) //nolint:gosec // bounded by the take below
	picture = b.take(length)

	if b.err != nil {
		return 0, nil, b.err
	}
	return kind, picture, nil
}

// id3Cover walks the frames of an ID3v2 tag looking for APIC.
func id3Cover(r io.ReadSeeker) ([]byte, error) {
	var header [10]byte
	if _, err := io.ReadFull(r, header[:]); err != nil || string(header[0:3]) != "ID3" {
		return nil, fmt.Errorf("%w: no ID3v2 tag", ErrNoEmbeddedCover)
	}

	major := header[3]
	if major < 3 {
		// ID3v2.2 names its frames with three characters instead of four, so
		// its layout is a different parser rather than a special case. Refused
		// rather than misread.
		return nil, fmt.Errorf("%w: ID3v2.%d is not read", ErrNoEmbeddedCover, major)
	}
	if header[5]&0x80 != 0 {
		// Unsynchronisation rewrites the bytes of the whole tag, so a picture
		// read out of one without undoing it is corrupt. Refusing beats
		// answering with something that will not decode.
		return nil, fmt.Errorf("%w: the tag is unsynchronised", ErrNoEmbeddedCover)
	}
	size := synchsafe(header[6:10])
	if header[5]&0x40 != 0 {
		// An extended header sits between this and the first frame, and its own
		// size is the first four bytes of it. The two versions disagree about
		// what that size counts: v2.3 writes it plain and **excludes itself**,
		// v2.4 writes it synchsafe and **includes itself**. Reading one as the
		// other lands four bytes into the first frame, where the frame id is
		// nonsense and the tag looks empty.
		var ext [4]byte
		if _, err := io.ReadFull(r, ext[:]); err != nil {
			return nil, ErrNoEmbeddedCover
		}
		rest := int64(binary.BigEndian.Uint32(ext[:]))
		if major >= 4 {
			rest = synchsafe(ext[:]) - 4
		}
		if rest < 0 {
			return nil, fmt.Errorf("%w: a malformed extended header", ErrNoEmbeddedCover)
		}
		if _, err := r.Seek(rest, io.SeekCurrent); err != nil {
			return nil, ErrNoEmbeddedCover
		}
		size -= 4 + rest
	}

	var first []byte
	for read := int64(0); read+10 <= size; {
		var frame [10]byte
		if _, err := io.ReadFull(r, frame[:]); err != nil {
			break
		}
		read += 10

		// A run of zero bytes is the padding at the end of a tag, not a frame.
		if frame[0] == 0 {
			break
		}
		// v2.4 sizes are synchsafe and v2.3's are plain. Reading one as the
		// other is off by up to a factor of two and lands mid-frame.
		length := int64(binary.BigEndian.Uint32(frame[4:8]))
		if major >= 4 {
			length = synchsafe(frame[4:8])
		}
		if length <= 0 || read+length > size {
			break
		}
		read += length

		if string(frame[0:4]) != "APIC" {
			if _, err := r.Seek(length, io.SeekCurrent); err != nil {
				break
			}
			continue
		}

		body := make([]byte, min(length, maxEmbeddedCover))
		if _, err := io.ReadFull(r, body); err != nil {
			break
		}
		kind, picture, err := apic(body)
		if err == nil {
			if kind == frontCover {
				return picture, nil
			}
			if first == nil {
				first = picture
			}
		}
	}

	if first != nil {
		return first, nil
	}
	return nil, ErrNoEmbeddedCover
}

// apic reads one APIC frame: an encoding byte, a MIME type, a picture type, a
// description in that encoding, and the bytes.
//
// The description is where this goes wrong if the encoding byte is ignored: it
// is terminated by one zero byte in Latin-1 and UTF-8 and by two in UTF-16, so
// a parser that always reads one starts the picture a byte early.
func apic(body []byte) (kind byte, picture []byte, err error) {
	b := reader{buf: body}

	encoding := b.byte()
	b.until(1) // the MIME type, always Latin-1 whatever the encoding byte says
	kind = b.byte()

	terminator := 1
	if encoding == 1 || encoding == 2 {
		terminator = 2
	}
	b.until(terminator) // the description

	picture = b.rest()
	if b.err != nil || len(picture) == 0 {
		return 0, nil, fmt.Errorf("%w: a malformed APIC frame", ErrNoEmbeddedCover)
	}
	return kind, picture, nil
}

// mp4Cover walks the atom tree to moov/udta/meta/ilst/covr/data.
func mp4Cover(r io.ReadSeeker) ([]byte, error) {
	end, err := r.Seek(0, io.SeekEnd)
	if err != nil {
		return nil, ErrNoEmbeddedCover
	}
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return nil, ErrNoEmbeddedCover
	}

	// The path is fixed, which is what makes this a walk and not a search: every
	// one of these boxes is a container whose children are boxes.
	for _, step := range []string{"moov", "udta", "meta", "ilst", "covr", "data"} {
		size, err := media.FindAtom(r, end, step)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrNoEmbeddedCover, err)
		}
		end = size
		if step == "meta" {
			// meta is the one container with a four-byte version and flags
			// before its children. Every other box here starts with one.
			if _, err := r.Seek(4, io.SeekCurrent); err != nil {
				return nil, ErrNoEmbeddedCover
			}
			end -= 4
		}
	}

	// The data box holds a type and four reserved bytes before the picture. The
	// type says JPEG or PNG and the decoder works it out anyway.
	if _, err := r.Seek(8, io.SeekCurrent); err != nil {
		return nil, ErrNoEmbeddedCover
	}
	picture := make([]byte, min(end-8, maxEmbeddedCover))
	if _, err := io.ReadFull(r, picture); err != nil {
		return nil, fmt.Errorf("%w: a truncated covr atom", ErrNoEmbeddedCover)
	}
	return picture, nil
}

// oggCover reads the comment header of the first stream in an Ogg file, and
// the METADATA_BLOCK_PICTURE comments in it.
//
// The header is the stream's second packet, after the identification one, so
// it is at the head of the file like every other tag here -- but it is cut
// into pages, and a picture of a few hundred kilobytes spans dozens of them.
// So the pages are walked and the packet put back together, from the first
// stream only: a file that multiplexes another is not a file whose second
// stream's comments are the track's.
func oggCover(r io.ReadSeeker) ([]byte, error) {
	packet, err := oggPacket(r, 1)
	if err != nil {
		return nil, err
	}

	var b reader
	switch {
	case bytes.HasPrefix(packet, []byte("OpusTags")):
		b.buf = packet[8:]
	case bytes.HasPrefix(packet, []byte("\x03vorbis")):
		b.buf = packet[7:]
	default:
		return nil, fmt.Errorf("%w: the second Ogg packet is not a comment header", ErrNoEmbeddedCover)
	}

	b.skip(int64(b.uint32le())) // the vendor string
	count := b.uint32le()
	var first []byte
	for i := uint32(0); i < count && b.err == nil; i++ {
		comment := b.take(int64(b.uint32le()))
		key, value, ok := bytes.Cut(comment, []byte("="))
		if !ok || !strings.EqualFold(string(key), "METADATA_BLOCK_PICTURE") {
			continue
		}
		block, err := base64.StdEncoding.DecodeString(string(value))
		if err != nil {
			continue
		}
		kind, picture, err := flacPicture(block)
		if err != nil {
			continue
		}
		if kind == frontCover {
			return picture, nil
		}
		if first == nil {
			first = picture
		}
	}
	if first != nil {
		return first, nil
	}
	return nil, ErrNoEmbeddedCover
}

// maxOggPacket bounds the packet oggPacket puts together: the largest picture
// this package takes, base64-encoded, and room for the comments around it.
const maxOggPacket = maxEmbeddedCover/3*4 + 1<<20

// oggPacket returns packet n of the first logical stream in an Ogg file.
//
// A page is a 27-byte header -- "OggS", a version, flags, a granule position,
// the stream's serial, a sequence number, a checksum and a count of segments
// -- then a table of that many segment lengths and the segments. A packet is
// segments laid end to end until one shorter than 255, and may carry on into
// the next page. The checksum is not verified: a page that lies about its
// lengths fails the bounds below, and one with the wrong bytes in it yields a
// picture that does not decode, which the caller already answers.
func oggPacket(r io.ReadSeeker, n int) ([]byte, error) {
	var (
		serial  uint32
		packet  []byte
		packets int
		scanned int64
	)
	for page := 0; scanned < maxTagScan; page++ {
		var header [27]byte
		if _, err := io.ReadFull(r, header[:]); err != nil || string(header[:4]) != "OggS" {
			return nil, fmt.Errorf("%w: not an Ogg page", ErrNoEmbeddedCover)
		}
		pageSerial := binary.LittleEndian.Uint32(header[14:18])
		if page == 0 {
			serial = pageSerial
		}
		lacing := make([]byte, header[26])
		if _, err := io.ReadFull(r, lacing); err != nil {
			return nil, fmt.Errorf("%w: a truncated Ogg page", ErrNoEmbeddedCover)
		}
		var length int64
		for _, l := range lacing {
			length += int64(l)
		}
		scanned += 27 + int64(len(lacing)) + length

		if pageSerial != serial {
			if _, err := r.Seek(length, io.SeekCurrent); err != nil {
				return nil, fmt.Errorf("%w: %w", ErrNoEmbeddedCover, err)
			}
			continue
		}
		body := make([]byte, length)
		if _, err := io.ReadFull(r, body); err != nil {
			return nil, fmt.Errorf("%w: a truncated Ogg page", ErrNoEmbeddedCover)
		}
		for _, l := range lacing {
			packet = append(packet, body[:l]...)
			body = body[l:]
			if len(packet) > maxOggPacket {
				return nil, fmt.Errorf("%w: an Ogg packet past %d bytes", ErrNoEmbeddedCover, maxOggPacket)
			}
			if l == 255 {
				continue
			}
			if packets == n {
				return packet, nil
			}
			packets++
			packet = nil
		}
	}
	return nil, fmt.Errorf("%w: no Ogg packet %d in the first %d bytes", ErrNoEmbeddedCover, n, maxTagScan)
}

// synchsafe decodes the seven-bits-per-byte integer ID3v2 uses so that a size
// can never contain a byte that looks like the start of an MPEG frame.
func synchsafe(b []byte) int64 {
	var n int64
	for _, c := range b {
		n = n<<7 | int64(c&0x7f)
	}
	return n
}

// reader walks a byte slice without a bounds check at every step. The first
// read past the end sets err and every one after it is a no-op, so a caller
// checks once at the end instead of five times in the middle.
type reader struct {
	buf []byte
	err error
}

func (r *reader) fail() {
	if r.err == nil {
		r.err = fmt.Errorf("%w: a truncated tag", ErrNoEmbeddedCover)
	}
}

func (r *reader) byte() byte {
	if len(r.buf) < 1 {
		r.fail()
		return 0
	}
	b := r.buf[0]
	r.buf = r.buf[1:]
	return b
}

func (r *reader) uint32() uint32 {
	if len(r.buf) < 4 {
		r.fail()
		return 0
	}
	n := binary.BigEndian.Uint32(r.buf[:4])
	r.buf = r.buf[4:]
	return n
}

// uint32le is the little-endian kind, which is what Ogg's comment headers use
// where every other format here is big-endian.
func (r *reader) uint32le() uint32 {
	if len(r.buf) < 4 {
		r.fail()
		return 0
	}
	n := binary.LittleEndian.Uint32(r.buf[:4])
	r.buf = r.buf[4:]
	return n
}

func (r *reader) skip(n int64) {
	if n < 0 || n > int64(len(r.buf)) {
		r.fail()
		return
	}
	r.buf = r.buf[n:]
}

// until advances past a string terminated by n zero bytes, and past the
// terminator itself.
func (r *reader) until(n int) {
	for i := 0; i+n <= len(r.buf); i++ {
		zero := true
		for j := range n {
			if r.buf[i+j] != 0 {
				zero = false
				break
			}
		}
		// A UTF-16 terminator is two zero bytes on an even boundary; an odd one
		// is the high byte of a character that happens to be zero.
		if zero && (n == 1 || i%2 == 0) {
			r.buf = r.buf[i+n:]
			return
		}
	}
	r.fail()
}

func (r *reader) take(n int64) []byte {
	if n < 0 || n > int64(len(r.buf)) {
		r.fail()
		return nil
	}
	out := r.buf[:n]
	r.buf = r.buf[n:]
	return out
}

func (r *reader) rest() []byte {
	out := r.buf
	r.buf = nil
	return out
}
