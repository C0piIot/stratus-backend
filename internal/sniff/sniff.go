// Package sniff says what a file is from its first bytes.
//
// It exists because the answer was being guessed from the name in three
// vocabularies at once -- the MIME type stored on a row, the kind an extractor
// works from, and the prefix a blob key is filed under -- and a name is
// something somebody typed. A camcorder's .mts was not a video here, a
// recording with no extension was nothing at all, and .ts is both a transport
// stream and a TypeScript file.
//
// A leaf: no I/O, no state, and the only import is the port that owns the Kind
// vocabulary. internal/files cannot import internal/media -- that package
// imports this one -- and the table belongs to neither of them anyway.
//
// **It answers only when it knows.** An empty MIME type and KindOther mean "ask
// the name", which is what every caller falls back to, because a wrong answer
// from the bytes is worse than no answer: the name at least says what somebody
// meant.
package sniff

import (
	"bytes"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/C0piIot/stratus-backend/internal/db"
)

// HeadSize is how much of a file is enough. It covers every signature below,
// including the transport stream's third packet marker at 376, and it is what
// http.DetectContentType reads.
const HeadSize = 512

// tsPacket is the length of an MPEG-TS packet. The format has no header at all:
// what identifies it is a sync byte at the start of every one of these.
const tsPacket = 188

// Sniff reports the MIME type and the kind of whatever these bytes are, or
// nothing when it cannot say.
//
// The standard library is the fallback rather than the first question:
// http.DetectContentType knows JPEG, PNG, GIF, WebP, MP4, WebM, AVI, MP3, Ogg
// and WAV, and does not know HEIC, HEIF, AVIF, Matroska or MPEG-TS -- which are
// most of what this project is for. What it is good at is text, which is how a
// TypeScript file called .ts stops being mistaken for a transport stream.
func Sniff(head []byte) (mimeType string, kind db.Kind) {
	if len(head) == 0 {
		return "", db.KindOther
	}

	if mime, kind, ok := known(head); ok {
		return mime, kind
	}

	// Anything the standard library is sure about. It answers
	// application/octet-stream when it is not, which is this function's own way
	// of saying nothing.
	detected := http.DetectContentType(head)
	switch {
	case detected == "" || strings.HasPrefix(detected, "application/octet-stream"):
		return "", db.KindOther
	case strings.HasPrefix(detected, "text/plain"):
		// Its idea of text is "not one of these control bytes", which calls a
		// buffer of random noise a text file. Text here means text: valid UTF-8
		// with nothing in it that a text file does not have.
		if !looksLikeText(head) {
			return "", db.KindOther
		}
	}
	return detected, kindOfMIME(detected)
}

// looksLikeText is the check the standard library's text fallback is missing.
//
// The last rune is allowed to be cut in half, because this is a window onto a
// file and not the file: three bytes of an unfinished character at the end of
// 512 is what reading part of a UTF-8 document looks like.
func looksLikeText(head []byte) bool {
	if bytes.IndexByte(head, 0) >= 0 {
		return false
	}
	for cut := range 4 {
		if utf8.Valid(head[:len(head)-cut]) {
			return true
		}
	}
	return false
}

// known is the part the standard library does not have.
func known(head []byte) (string, db.Kind, bool) {
	switch {
	case isISOBMFF(head):
		return fromBrand(head)

	case bytes.HasPrefix(head, []byte{0x1A, 0x45, 0xDF, 0xA3}):
		// EBML, which is Matroska or WebM, and the doctype says which. It is a
		// few bytes in and not at a fixed offset, so it is searched for rather
		// than indexed.
		if bytes.Contains(head, []byte("webm")) {
			return "video/webm", db.KindVideo, true
		}
		return "video/x-matroska", db.KindVideo, true

	case isTransportStream(head):
		return "video/mp2t", db.KindVideo, true

	case bytes.HasPrefix(head, []byte{0x00, 0x00, 0x01, 0xBA}):
		// An MPEG program stream, which is what a .mpg off a camera of that era
		// is. Its transport sibling is above.
		return "video/mpeg", db.KindVideo, true

	case bytes.HasPrefix(head, []byte("fLaC")):
		return "audio/flac", db.KindAudio, true

	case bytes.HasPrefix(head, []byte("OggS")):
		// Ogg carries video in principle and audio in practice: Vorbis and Opus
		// are what is in the wild, and a Theora file would be named .ogv, which
		// the extension answers for.
		return "audio/ogg", db.KindAudio, true

	case isTIFF(head):
		// Also every camera raw worth the name: DNG, CR2, NEF and ARW are TIFF
		// with private tags in them. Which one it is takes more than a
		// signature, and "an image" is the answer being asked for here.
		return "image/tiff", db.KindImage, true
	}
	return "", db.KindOther, false
}

// isISOBMFF reports the box structure HEIC, MP4 and QuickTime all share: a
// length, then the letters ftyp.
func isISOBMFF(head []byte) bool {
	return len(head) >= 12 && string(head[4:8]) == "ftyp"
}

// fromBrand reads what kind of ISOBMFF file this is.
//
// The container is the same for a photograph from an iPhone, a film and a
// music track; the brand at offset 8 is the only thing that tells them apart,
// which is exactly what an extension claims to know and sometimes does not.
func fromBrand(head []byte) (string, db.Kind, bool) {
	switch brand := string(head[8:12]); brand {
	case "heic", "heix", "heim", "heis", "hevc", "hevx", "mif1", "msf1":
		return "image/heic", db.KindImage, true
	case "avif", "avis":
		return "image/avif", db.KindImage, true
	case "qt  ":
		return "video/quicktime", db.KindVideo, true
	case "M4A ", "M4B ":
		return "audio/mp4", db.KindAudio, true
	case "isom", "iso2", "iso4", "iso5", "iso6", "mp41", "mp42", "avc1", "dash", "M4V ", "M4VP":
		return "video/mp4", db.KindVideo, true
	default:
		// A brand nothing here knows is still ISOBMFF, and saying "video"
		// because most of them are would be the guess this package exists to
		// avoid.
		return "", db.KindOther, false
	}
}

// isTransportStream looks for the sync byte at the start of three consecutive
// packets. One would match far too much; three in the right places is what the
// format actually guarantees.
func isTransportStream(head []byte) bool {
	return len(head) > 2*tsPacket &&
		head[0] == 0x47 && head[tsPacket] == 0x47 && head[2*tsPacket] == 0x47
}

func isTIFF(head []byte) bool {
	return bytes.HasPrefix(head, []byte("II*\x00")) || bytes.HasPrefix(head, []byte("MM\x00*"))
}

// kindOfMIME reads a type the standard library reported. Only the three that
// mean something to an extractor: everything else is a file this project
// stores and does not look inside.
func kindOfMIME(mimeType string) db.Kind {
	switch {
	case strings.HasPrefix(mimeType, "image/"):
		return db.KindImage
	case strings.HasPrefix(mimeType, "audio/"):
		return db.KindAudio
	case strings.HasPrefix(mimeType, "video/"):
		return db.KindVideo
	default:
		return db.KindOther
	}
}
