package media

import (
	"bytes"
	"encoding/binary"
	"errors"
	"strings"
	"testing"
)

// The fixtures are built here rather than committed, because what these parsers
// get wrong is a length or an offset and a hand-built file is the only kind
// where the test can put the picture at a byte it chose.
//
// Every case carries a payload that is not a valid image: what is under test is
// finding the bytes, and a real JPEG would only make the fixtures larger. The
// decode is tested where it happens, in thumb_test.go.

const payload = "PICTURE BYTES"

// bigPayload is longer than 127 bytes, which is the only length at which a
// synchsafe size and a plain big-endian one differ. Every fixture below 128
// bytes encodes to the same four bytes either way, so a parser reading v2.4
// sizes as v2.3 passes a small case and lands mid-frame on a real file.
var bigPayload = strings.Repeat("PICTURE ", 40)

func TestFLACCover(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		file []byte
		want string
	}{
		{
			name: "one picture",
			file: flacFile(flacBlock(6, flacPicked(frontCover, payload), true)),
			want: payload,
		},
		{
			// The blocks before it are what a real file has, and skipping them
			// by their length is the whole job.
			name: "after the blocks a real file has",
			file: flacFile(
				flacBlock(0, bytes.Repeat([]byte{1}, 34), false), // STREAMINFO
				flacBlock(4, []byte("VORBIS COMMENT PRETEND"), false),
				flacBlock(6, flacPicked(frontCover, payload), false),
				flacBlock(1, make([]byte, 512), true), // PADDING
			),
			want: payload,
		},
		{
			// A file with the back cover first: the front is preferred rather
			// than whichever comes first, because a record often carries both.
			name: "the front is preferred over the back",
			file: flacFile(
				flacBlock(6, flacPicked(4, "THE BACK"), false),
				flacBlock(6, flacPicked(frontCover, payload), true),
			),
			want: payload,
		},
		{
			// And with no front declared, the first is all there is.
			name: "no front cover declared",
			file: flacFile(flacBlock(6, flacPicked(4, payload), true)),
			want: payload,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := embeddedCover(bytes.NewReader(tt.file), "a.flac")
			if err != nil {
				t.Fatalf("embeddedCover: %v", err)
			}
			if string(got) != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestFLACWithNoCover(t *testing.T) {
	t.Parallel()

	tests := map[string][]byte{
		"no picture block":  flacFile(flacBlock(0, bytes.Repeat([]byte{1}, 34), true)),
		"not a FLAC at all": []byte("ID3\x04\x00\x00\x00\x00\x00\x00"),
		"truncated":         []byte("fLaC\x86\x00\x00\x20"),
		"empty":             {},
		// A picture block whose length field runs past the block.
		"a lying length": flacFile(flacBlock(6, flacPictureBody(frontCover, payload, 1<<20), true)),
	}

	for name, file := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := embeddedCover(bytes.NewReader(file), "a.flac"); !errors.Is(err, ErrNoEmbeddedCover) {
				t.Errorf("embeddedCover = %v, want ErrNoEmbeddedCover", err)
			}
		})
	}
}

func TestID3Cover(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		file []byte
		want string
	}{
		{
			name: "v2.3, latin-1 description",
			file: id3File(3, apicFrame(0, frontCover, "a cover", payload)),
			want: payload,
		},
		{
			// v2.4 writes frame sizes synchsafe and v2.3 writes them plain.
			// Reading one as the other lands mid-frame.
			name: "v2.4, synchsafe frame sizes",
			file: id3File(4, apicFrame(0, frontCover, "a cover", bigPayload)),
			want: bigPayload,
		},
		{
			// UTF-16 terminates the description with two zero bytes, so a
			// parser that always reads one starts the picture a byte early.
			name: "v2.3, UTF-16 description",
			file: id3File(3, apicFrame(1, frontCover, "\xff\xfe", payload)),
			want: payload,
		},
		{
			name: "an empty description",
			file: id3File(3, apicFrame(0, frontCover, "", payload)),
			want: payload,
		},
		{
			// The frames before it are what a real tag has.
			name: "after the frames a real tag has",
			file: id3File(4,
				textFrame("TIT2", "Hunter"),
				textFrame("TPE1", "Björk"),
				apicFrame(0, frontCover, "", bigPayload),
			),
			want: bigPayload,
		},
		{
			name: "the front is preferred over the back",
			file: id3File(4,
				apicFrame(0, 4, "back", strings.Repeat("THE BACK ", 30)),
				apicFrame(0, frontCover, "front", bigPayload),
			),
			want: bigPayload,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := embeddedCover(bytes.NewReader(tt.file), "a.mp3")
			if err != nil {
				t.Fatalf("embeddedCover: %v", err)
			}
			if string(got) != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestID3WithNoCover(t *testing.T) {
	t.Parallel()

	unsynchronised := id3File(4, apicFrame(0, frontCover, "", payload))
	unsynchronised[5] |= 0x80

	tests := map[string][]byte{
		"no APIC frame": id3File(4, textFrame("TIT2", "Hunter")),
		"no tag at all": []byte("fLaC\x80\x00\x00\x22"),
		"truncated":     []byte("ID3\x04\x00\x00"),
		// ID3v2.2 names frames with three characters, which is a different
		// layout rather than a special case. Refused rather than misread.
		"version 2.2": id3File(2, apicFrame(0, frontCover, "", payload)),
		// Unsynchronisation rewrites the bytes of the whole tag, so a picture
		// lifted out of one without undoing it is corrupt.
		"unsynchronised": unsynchronised,
	}

	for name, file := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := embeddedCover(bytes.NewReader(file), "a.mp3"); !errors.Is(err, ErrNoEmbeddedCover) {
				t.Errorf("embeddedCover = %v, want ErrNoEmbeddedCover", err)
			}
		})
	}
}

func TestMP4Cover(t *testing.T) {
	t.Parallel()

	// The data box carries a type -- 13 is JPEG -- and four reserved bytes
	// before the picture.
	data := atom("data", append([]byte{0, 0, 0, 13, 0, 0, 0, 0}, payload...)...)
	covr := atom("covr", data...)
	full := atom("moov", atom("udta", metaAtom(atom("ilst", covr...)...)...)...)

	tests := []struct {
		name string
		file []byte
	}{
		{name: "the usual tree", file: full},
		{
			// A real file has the media data before or after the metadata, and
			// it is far larger than anything else. Skipping it by its length is
			// the job.
			name: "beside a big mdat",
			file: append(atom("mdat", make([]byte, 4096)...), full...),
		},
		{
			// Other boxes in ilst, which is where the rest of the tags live.
			name: "among the other tags",
			file: atom("moov", atom("udta", metaAtom(atom("ilst",
				append(atom("\xa9nam", atom("data", []byte("Hunter")...)...), covr...)...)...)...)...),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := embeddedCover(bytes.NewReader(tt.file), "a.m4a")
			if err != nil {
				t.Fatalf("embeddedCover: %v", err)
			}
			if string(got) != payload {
				t.Errorf("got %q, want %q", got, payload)
			}
		})
	}
}

func TestMP4WithNoCover(t *testing.T) {
	t.Parallel()

	tests := map[string][]byte{
		"no covr": atom("moov", atom("udta",
			metaAtom(atom("ilst", atom("\xa9nam", []byte("x")...)...)...)...)...),
		"no moov":        atom("mdat", make([]byte, 64)...),
		"no udta":        atom("moov", atom("mvhd", make([]byte, 32)...)...),
		"empty":          {},
		"a header alone": {0, 0, 0, 8},
		// A size field claiming more than the file holds.
		"a lying size": {0, 0, 0xff, 0, 'm', 'o', 'o', 'v'},
		// A size smaller than its own header, which would loop forever if it
		// were not refused.
		"an impossible size": {0, 0, 0, 4, 'm', 'o', 'o', 'v', 0, 0, 0, 0},
	}

	for name, file := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := embeddedCover(bytes.NewReader(file), "a.m4a"); !errors.Is(err, ErrNoEmbeddedCover) {
				t.Errorf("embeddedCover = %v, want ErrNoEmbeddedCover", err)
			}
		})
	}
}

// TestEmbeddedCoverOfSomethingElse: a format whose tags this does not read is
// answered as having no picture rather than as a failure, which is what it is.
func TestEmbeddedCoverOfSomethingElse(t *testing.T) {
	t.Parallel()

	// Vorbis and Opus keep theirs base64-encoded inside a comment, which is not
	// read yet, and a .wav has nowhere to keep one.
	for _, name := range []string{"a.ogg", "a.opus", "a.wav", "a.wma", "a"} {
		if _, err := embeddedCover(bytes.NewReader([]byte("whatever")), name); !errors.Is(err, ErrNoEmbeddedCover) {
			t.Errorf("embeddedCover(%q) = %v, want ErrNoEmbeddedCover", name, err)
		}
	}
}

func TestSynchsafe(t *testing.T) {
	t.Parallel()

	// Seven bits a byte, so that a size can never contain a byte that looks
	// like the start of an MPEG frame.
	tests := map[int64][]byte{
		0:         {0, 0, 0, 0},
		1:         {0, 0, 0, 1},
		128:       {0, 0, 1, 0},
		257:       {0, 0, 2, 1},
		268435455: {0x7f, 0x7f, 0x7f, 0x7f},
	}
	for want, b := range tests {
		if got := synchsafe(b); got != want {
			t.Errorf("synchsafe(%v) = %d, want %d", b, got, want)
		}
	}
}

// --- fixtures ---------------------------------------------------------------

func flacFile(blocks ...[]byte) []byte {
	out := []byte("fLaC")
	for _, b := range blocks {
		out = append(out, b...)
	}
	return out
}

func flacBlock(kind byte, body []byte, last bool) []byte {
	header := kind
	if last {
		header |= 0x80
	}
	n := len(body)
	return append([]byte{header, byte(n >> 16), byte(n >> 8), byte(n)}, body...)
}

func flacPicked(kind uint32, picture string) []byte {
	return flacPictureBody(kind, picture, len(picture))
}

// flacPictureWithLength writes a declared length that need not match the bytes,
// which is what the malformed case needs.
func flacPictureBody(kind uint32, picture string, declared int) []byte {
	var out bytes.Buffer
	be := func(n uint32) { _ = binary.Write(&out, binary.BigEndian, n) }

	be(kind)
	be(uint32(len("image/jpeg")))
	out.WriteString("image/jpeg")
	be(uint32(len("a description")))
	out.WriteString("a description")
	be(600) // width
	be(600) // height
	be(24)  // depth
	be(0)   // colours
	be(uint32(declared))
	out.WriteString(picture)
	return out.Bytes()
}

func id3File(major byte, frames ...frame) []byte {
	var body []byte
	for _, f := range frames {
		body = append(body, id3Frame(f.id, major, f.body)...)
	}
	// Padding, which every real tag has and which the parser must read as the
	// end of the frames rather than as another one.
	body = append(body, make([]byte, 16)...)

	header := []byte{'I', 'D', '3', major, 0, 0}
	n := len(body)
	header = append(header,
		byte(n>>21)&0x7f, byte(n>>14)&0x7f, byte(n>>7)&0x7f, byte(n)&0x7f)
	return append(header, body...)
}

func id3Frame(id string, major byte, body []byte) []byte {
	out := []byte(id)
	n := len(body)
	if major >= 4 {
		out = append(out, byte(n>>21)&0x7f, byte(n>>14)&0x7f, byte(n>>7)&0x7f, byte(n)&0x7f)
	} else {
		out = binary.BigEndian.AppendUint32(out, uint32(n))
	}
	return append(append(out, 0, 0), body...)
}

// apicFrame builds an APIC frame. It returns the body rather than encoded bytes
// because the size encoding depends on the tag version, which only id3File
// knows: v2.4 writes sizes synchsafe and v2.3 writes them plain.
func apicFrame(encoding, kind byte, description, picture string) frame {
	var body bytes.Buffer
	body.WriteByte(encoding)
	body.WriteString("image/jpeg")
	body.WriteByte(0)
	body.WriteByte(kind)
	body.WriteString(description)
	body.WriteByte(0)
	if encoding == 1 || encoding == 2 {
		body.WriteByte(0)
	}
	body.WriteString(picture)
	return frame{id: "APIC", body: body.Bytes()}
}

func textFrame(id, text string) frame {
	return frame{id: id, body: append([]byte{0}, text...)}
}

// frame defers the size encoding until the file knows its version, which is why
// the fixtures pass frames around as this rather than as bytes.
type frame struct {
	id   string
	body []byte
}

func atom(name string, body ...byte) []byte {
	out := binary.BigEndian.AppendUint32(nil, uint32(8+len(body)))
	out = append(out, name...)
	return append(out, body...)
}

// metaAtom is the one container with a version and flags before its children.
func metaAtom(body ...byte) []byte {
	return atom("meta", append([]byte{0, 0, 0, 0}, body...)...)
}

// TestReaderStopsAtTheEnd is the bounds check, tested directly because it is
// what stands between a corrupt length field and a panic. Every parser above
// leans on the first read past the end setting err and every one after it being
// a no-op, so a caller checks once at the end instead of five times.
func TestReaderStopsAtTheEnd(t *testing.T) {
	t.Parallel()

	tests := map[string]func(*reader){
		"a byte past the end":      func(r *reader) { r.byte(); r.byte(); r.byte() },
		"a uint32 past the end":    func(r *reader) { r.uint32() },
		"a skip past the end":      func(r *reader) { r.skip(10) },
		"a negative skip":          func(r *reader) { r.skip(-1) },
		"taking past the end":      func(r *reader) { r.take(10) },
		"taking a negative length": func(r *reader) { r.take(-1) },
		"a string with no end":     func(r *reader) { r.until(1) },
	}

	for name, read := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := &reader{buf: []byte{1, 2}}
			read(r)
			if r.err == nil {
				t.Error("reading past the end reported no error")
			}
			// And the error survives: a second read must not clear it.
			r.byte()
			if !errors.Is(r.err, ErrNoEmbeddedCover) {
				t.Errorf("err = %v", r.err)
			}
		})
	}
}

// TestMP4ExtendedSize covers the 64-bit length real files use for the mdat of a
// long recording, which the walk has to skip by the right number of bytes.
func TestMP4ExtendedSize(t *testing.T) {
	t.Parallel()

	data := atom("data", append([]byte{0, 0, 0, 13, 0, 0, 0, 0}, payload...)...)
	covr := atom("covr", data...)
	moov := atom("moov", atom("udta", metaAtom(atom("ilst", covr...)...)...)...)

	// An mdat whose size is 1, meaning the real one is the eight bytes that
	// follow, before the metadata.
	body := make([]byte, 512)
	mdat := binary.BigEndian.AppendUint32(nil, 1)
	mdat = append(mdat, "mdat"...)
	mdat = binary.BigEndian.AppendUint64(mdat, uint64(16+len(body)))
	mdat = append(mdat, body...)

	got, err := embeddedCover(bytes.NewReader(append(mdat, moov...)), "a.m4a")
	if err != nil {
		t.Fatalf("embeddedCover: %v", err)
	}
	if string(got) != payload {
		t.Errorf("got %q, want %q", got, payload)
	}
}

// TestID3ExtendedHeader is the other optional header, which sits between the
// tag header and the first frame and whose own size is encoded differently in
// v2.3 and v2.4.
func TestID3ExtendedHeader(t *testing.T) {
	t.Parallel()

	for _, major := range []byte{3, 4} {
		file := id3File(major, apicFrame(0, frontCover, "", payload))
		// Announce an extended header and splice one in. The two versions
		// disagree about what the size field counts, which is the whole point
		// of the case: v2.3 excludes itself, so six means six more bytes after
		// the four of the field; v2.4 includes itself, so six means two more.
		file[5] |= 0x40
		extended := []byte{0, 0, 0, 6, 0, 0, 0, 0, 0, 0}
		if major >= 4 {
			extended = []byte{0, 0, 0, 6, 1, 0}
		}
		spliced := append(append(append([]byte{}, file[:10]...), extended...), file[10:]...)
		// And the tag size grows by what was spliced in.
		n := len(spliced) - 10
		spliced[6] = byte(n>>21) & 0x7f
		spliced[7] = byte(n>>14) & 0x7f
		spliced[8] = byte(n>>7) & 0x7f
		spliced[9] = byte(n) & 0x7f

		got, err := embeddedCover(bytes.NewReader(spliced), "a.mp3")
		if err != nil {
			t.Fatalf("v2.%d: %v", major, err)
		}
		if string(got) != payload {
			t.Errorf("v2.%d: got %q", major, got)
		}
	}
}
