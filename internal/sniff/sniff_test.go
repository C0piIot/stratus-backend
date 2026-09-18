package sniff_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/C0piIot/stratus-backend/internal/db"
	"github.com/C0piIot/stratus-backend/internal/sniff"
)

// TestSniffRealFiles is the claim that matters, made against files rather than
// against a table copied out of the same source as the code: the fixtures are
// the ones the rest of the project already uses, and two of them are exactly
// the cases a name gets wrong.
func TestSniffRealFiles(t *testing.T) {
	t.Parallel()

	tests := []struct {
		fixture  string
		wantMIME string
		wantKind db.Kind
	}{
		// HEIF through the same box structure an MP4 uses. Telling them apart
		// is the brand and nothing else, which is the whole reason this package
		// reads twelve bytes rather than four.
		{"../media/testdata/tiny.heic", "image/heic", db.KindImage},
		{"../media/testdata/moov-last.mp4", "video/mp4", db.KindVideo},
		{"../media/testdata/moov-first.mp4", "video/mp4", db.KindVideo},
		{"../../scripts/testdata/cover.jpg", "image/jpeg", db.KindImage},
	}

	for _, tt := range tests {
		t.Run(filepath.Base(tt.fixture), func(t *testing.T) {
			t.Parallel()
			mimeType, kind := sniff.Sniff(head(t, tt.fixture))
			if !strings.HasPrefix(mimeType, tt.wantMIME) {
				t.Errorf("mime = %q, want %q", mimeType, tt.wantMIME)
			}
			if kind != tt.wantKind {
				t.Errorf("kind = %q, want %q", kind, tt.wantKind)
			}
		})
	}
}

// TestSniffByBrand covers the ISOBMFF brands no fixture in this repository has,
// because one container holds a photograph, a film and a track and only these
// four bytes say which.
func TestSniffByBrand(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		mime string
		kind db.Kind
	}{
		"heic": {"image/heic", db.KindImage},
		"mif1": {"image/heic", db.KindImage},
		"avif": {"image/avif", db.KindImage},
		"qt  ": {"video/quicktime", db.KindVideo},
		"M4A ": {"audio/mp4", db.KindAudio},
		"isom": {"video/mp4", db.KindVideo},
		"mp42": {"video/mp4", db.KindVideo},
	}

	for brand, want := range tests {
		t.Run(strings.TrimSpace(brand), func(t *testing.T) {
			t.Parallel()
			mimeType, kind := sniff.Sniff(isobmff(brand))
			if mimeType != want.mime || kind != want.kind {
				t.Errorf("brand %q = %q/%q, want %q/%q", brand, mimeType, kind, want.mime, want.kind)
			}
		})
	}

	// A brand nothing here knows is ISOBMFF and nothing more. Answering "video"
	// because most of them are is the guess this package exists to refuse.
	if mimeType, kind := sniff.Sniff(isobmff("zzzz")); mimeType != "" || kind != db.KindOther {
		t.Errorf("an unknown brand = %q/%q, want nothing", mimeType, kind)
	}
}

// TestSniffTransportStream is the format with no header at all: what identifies
// it is a sync byte at the start of every 188-byte packet, which is also why
// the window is 512 bytes and not 64.
func TestSniffTransportStream(t *testing.T) {
	t.Parallel()

	stream := make([]byte, 512)
	for i := range stream {
		if i%188 == 0 {
			stream[i] = 0x47
		}
	}
	if mimeType, kind := sniff.Sniff(stream); mimeType != "video/mp2t" || kind != db.KindVideo {
		t.Errorf("a transport stream = %q/%q", mimeType, kind)
	}

	// One sync byte and then nothing at 188 is not a transport stream, and this
	// is the case that matters: a single 0x47 is the letter G.
	if _, kind := sniff.Sniff([]byte("Good morning, this is a text file.")); kind == db.KindVideo {
		t.Error("a sentence beginning with G was read as a transport stream")
	}
}

// TestSniffTypeScript is the collision this issue is named for: .ts is a
// transport stream and a source file, and only the bytes can say which.
func TestSniffTypeScript(t *testing.T) {
	t.Parallel()

	source := "import { Stratus } from './stratus'\n\nexport const backup = async () => {}\n"
	mimeType, kind := sniff.Sniff([]byte(source))
	if !strings.HasPrefix(mimeType, "text/plain") {
		t.Errorf("mime = %q, want text", mimeType)
	}
	if kind != db.KindOther {
		t.Errorf("kind = %q, want nothing to extract from source code", kind)
	}
}

// TestSniffSaysNothing: an answer this package cannot give is the name's to
// give, so it has to be distinguishable from a wrong one.
func TestSniffSaysNothing(t *testing.T) {
	t.Parallel()

	for name, head := range map[string][]byte{
		"nothing at all":  nil,
		"three bytes":     {0x00, 0x01, 0x02},
		"a truncated box": []byte("\x00\x00\x00\x18ftyp"),
		"random noise":    bytes.Repeat([]byte{0xA5, 0x5A}, 64),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if mimeType, kind := sniff.Sniff(head); mimeType != "" || kind != db.KindOther {
				t.Errorf("%s = %q/%q, want nothing", name, mimeType, kind)
			}
		})
	}
}

// TestSniffTheOtherContainers covers what the standard library does not know
// and a media library is full of.
func TestSniffTheOtherContainers(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		head []byte
		mime string
		kind db.Kind
	}{
		"matroska": {append([]byte{0x1A, 0x45, 0xDF, 0xA3}, "\x42\x82\x88matroska"...), "video/x-matroska", db.KindVideo},
		"webm":     {append([]byte{0x1A, 0x45, 0xDF, 0xA3}, "\x42\x82\x84webm"...), "video/webm", db.KindVideo},
		"flac":     {[]byte("fLaC\x00\x00\x00\x22"), "audio/flac", db.KindAudio},
		"ogg":      {[]byte("OggS\x00\x02\x00\x00"), "audio/ogg", db.KindAudio},
		"tiff":     {[]byte("II*\x00\x08\x00\x00\x00"), "image/tiff", db.KindImage},
		"mpeg-ps":  {[]byte{0x00, 0x00, 0x01, 0xBA, 0x44, 0x00, 0x04, 0x00}, "video/mpeg", db.KindVideo},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			mimeType, kind := sniff.Sniff(tt.head)
			if mimeType != tt.mime || kind != tt.kind {
				t.Errorf("%s = %q/%q, want %q/%q", name, mimeType, kind, tt.mime, tt.kind)
			}
		})
	}
}

// TestSniffFallsBackToTheLibrary covers what the standard library answers for,
// which is the half of this that nobody has to maintain: the kinds it names and
// the text it recognises.
func TestSniffFallsBackToTheLibrary(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		head []byte
		mime string
		kind db.Kind
	}{
		"gif":   {[]byte("GIF89a\x01\x00"), "image/gif", db.KindImage},
		"png":   {[]byte("\x89PNG\r\n\x1a\n"), "image/png", db.KindImage},
		"webp":  {append([]byte("RIFF\x24\x00\x00\x00"), "WEBPVP8 "...), "image/webp", db.KindImage},
		"wav":   {append([]byte("RIFF\x24\x00\x00\x00"), "WAVEfmt "...), "audio/wave", db.KindAudio},
		"avi":   {append([]byte("RIFF\x24\x00\x00\x00"), "AVI LIST"...), "video/avi", db.KindVideo},
		"mp3":   {[]byte("ID3\x04\x00\x00\x00\x00\x00\x00"), "audio/mpeg", db.KindAudio},
		"pdf":   {[]byte("%PDF-1.7\n%\xe2\xe3\xcf\xd3"), "application/pdf", db.KindOther},
		"utf-8": {[]byte("una fotografía, en texto plano\n"), "text/plain", db.KindOther},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			mimeType, kind := sniff.Sniff(tt.head)
			if !strings.HasPrefix(mimeType, tt.mime) {
				t.Errorf("%s = %q, want %q", name, mimeType, tt.mime)
			}
			if kind != tt.kind {
				t.Errorf("%s kind = %q, want %q", name, kind, tt.kind)
			}
		})
	}

	// A window onto a file cuts the last character in half, and a document is
	// still a document. The head here ends mid-rune on purpose.
	whole := []byte("una fotografía tomada en verano — con guion largo")
	if _, kind := sniff.Sniff(whole[:len(whole)-1]); kind != db.KindOther {
		t.Error("a truncated final rune made a text file unreadable")
	}
	if mimeType, _ := sniff.Sniff(whole[:len(whole)-1]); !strings.HasPrefix(mimeType, "text/plain") {
		t.Errorf("a truncated final rune = %q, want text", mimeType)
	}
}

// isobmff is the first twelve bytes of a file in that container: a box length,
// the word ftyp, and the brand that says what kind of file it is.
func isobmff(brand string) []byte {
	head := []byte{0x00, 0x00, 0x00, 0x18}
	head = append(head, "ftyp"...)
	return append(head, brand...)
}

func head(t *testing.T, path string) []byte {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return body[:min(len(body), sniff.HeadSize)]
}
