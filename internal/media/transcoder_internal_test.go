package media

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/C0piIot/stratus-backend/internal/db"
)

// fixtureOpener serves testdata as if it were the blob store: the key is a
// file's name there.
type fixtureOpener struct{}

func (fixtureOpener) OpenFile(_ context.Context, f db.File) (io.ReadSeekCloser, error) {
	b, err := os.ReadFile(filepath.Join("testdata", f.BlobKey))
	if err != nil {
		return nil, err
	}
	return nopCloser{bytes.NewReader(b)}, nil
}

type nopCloser struct{ io.ReadSeeker }

func (nopCloser) Close() error { return nil }

func loopback(t *testing.T) *Loopback {
	t.Helper()
	l, err := NewLoopback(t.Context(), fixtureOpener{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l
}

// fetch asks the loopback for url and reads the whole answer.
func fetch(t *testing.T, url string, header http.Header) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range header {
		req.Header[k] = v
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

// TestLoopbackServesAGrantAndNothingElse: one file per token, by range, for as
// long as the grant stands, and only on the loopback interface.
func TestLoopbackServesAGrantAndNothingElse(t *testing.T) {
	t.Parallel()
	l := loopback(t)

	if !strings.HasPrefix(l.base, "http://127.0.0.1:") {
		t.Fatalf("listening at %s, want the loopback interface only", l.base)
	}

	url, revoke := l.Grant(db.File{Path: "music/tone.flac", BlobKey: "tone.flac"})
	code, body := fetch(t, url, http.Header{"Range": {"bytes=0-3"}})
	if code != http.StatusPartialContent || body != "fLaC" {
		t.Errorf("a range of a granted file = %d %q, want 206 and the FLAC magic", code, body)
	}

	if code, _ := fetch(t, l.base+strings.Repeat("0", 32), nil); code != http.StatusNotFound {
		t.Errorf("a token never granted = %d, want 404", code)
	}

	revoke()
	if code, _ := fetch(t, url, nil); code != http.StatusNotFound {
		t.Errorf("a revoked token = %d, want 404", code)
	}

	other, _ := l.Grant(db.File{Path: "gone", BlobKey: "not-there"})
	if code, _ := fetch(t, other, nil); code != http.StatusInternalServerError {
		t.Errorf("a grant whose blob cannot be opened = %d, want 500", code)
	}
}

// TestTranscodeArgs is the command, which is the part worth reading twice.
func TestTranscodeArgs(t *testing.T) {
	t.Parallel()
	p := Plan{Encoder: "libopus", Muxer: "ogg", Bitrate: 96_000, SampleRate: 48_000, Channels: 2}
	got := transcodeArgs("http://127.0.0.1:1/t", p, 1500*time.Millisecond)
	want := []string{
		"-hide_banner", "-v", "error", "-nostdin",
		"-ss", "1.5",
		"-protocol_whitelist", "http,tcp",
		"-i", "http://127.0.0.1:1/t",
		"-map", "0:a:0", "-vn", "-sn", "-dn",
		"-c:a", "libopus", "-b:a", "96000", "-ar", "48000", "-ac", "2",
		"-f", "ogg", "-",
	}
	if !slices.Equal(got, want) {
		t.Errorf("got  %q\nwant %q", got, want)
	}

	frag := transcodeArgs("u", Plan{Encoder: "aac", Muxer: "ipod", Fragmented: true, BitDepth: 16}, 0)
	if !slices.Contains(frag, "frag_keyframe+empty_moov+default_base_moof") || !slices.Contains(frag, "s16") {
		t.Errorf("a fragmented, sixteen-bit plan should say both: %q", frag)
	}

	lossless := transcodeArgs("u", Plan{Encoder: "flac", Muxer: "flac"}, 0)
	if slices.Contains(lossless, "-ss") || slices.Contains(lossless, "-b:a") {
		t.Errorf("no offset and no bitrate should say neither: %q", lossless)
	}
}

// TestTranscodeIsBoundedAndReleased: a full house is ErrBusy, and closing a
// transcode -- even one that failed -- gives its slot back.
func TestTranscodeIsBoundedAndReleased(t *testing.T) {
	t.Parallel()
	tr := newTranscoder(stubFFmpeg(t), loopback(t), 1)
	f := db.File{Path: "tone.flac", BlobKey: "tone.flac"}

	tr.slots <- struct{}{}
	if _, err := tr.Transcode(t.Context(), f, targetPlan("mp3"), 0); !errors.Is(err, ErrBusy) {
		t.Fatalf("with every slot taken, err = %v, want ErrBusy", err)
	}
	<-tr.slots

	_, err := tr.Transcode(t.Context(), f, targetPlan("mp3"), 0)
	if err == nil || !strings.Contains(err.Error(), "this is the stub") {
		t.Fatalf("a failing ffmpeg: err = %v, want what it said on stderr", err)
	}
	if n := len(tr.slots); n != 0 {
		t.Errorf("a failed transcode kept its slot: %d taken", n)
	}
}

func targetPlan(format string) Plan {
	t := targets[format]
	return Plan{Format: format, Encoder: t.encoder, Muxer: t.muxer, MIME: t.mime, Bitrate: t.fallback}
}

// TestTranscodeForReal is the path end to end with the binary, where there is
// one: each format out of a FLAC, an m4a whose index is at the end -- which is
// what the loopback is for, since a pipe cannot seek to it -- and an offset
// that has to come out as about half the bytes.
func TestTranscodeForReal(t *testing.T) {
	t.Parallel()
	ffmpeg := realFFmpeg(t)
	tr := newTranscoder(ffmpeg, loopback(t), 8)

	size := func(name string, p Plan, offset time.Duration) int {
		t.Helper()
		out, err := tr.Transcode(t.Context(), db.File{Path: name, BlobKey: name}, p, offset)
		if err != nil {
			t.Fatalf("%s to %s: %v", name, p.Format, err)
		}
		defer func() { _ = out.Close() }()
		b, err := io.ReadAll(out)
		if err != nil {
			t.Fatalf("%s to %s: %v", name, p.Format, err)
		}
		return len(b)
	}

	for _, format := range []string{"mp3", "opus", "aac", "flac"} {
		if n := size("tone.flac", targetPlan(format), 0); n < 1000 {
			t.Errorf("tone.flac to %s: %d bytes", format, n)
		}
	}
	if n := size("moov-last.m4a", targetPlan("mp3"), 0); n < 1000 {
		t.Errorf("an m4a with its moov last: %d bytes", n)
	}

	// An MP4 down a pipe is only possible in fragments; asserted by its boxes,
	// since a non-fragmented one would fail to write at all or put moov last.
	mp4Plan := Plan{Format: "aac", Encoder: "aac", Muxer: "ipod", MIME: "audio/mp4", Container: "mp4", Fragmented: true, Bitrate: 128_000}
	out, err := tr.Transcode(t.Context(), db.File{Path: "tone.flac", BlobKey: "tone.flac"}, mp4Plan, 0)
	if err != nil {
		t.Fatalf("fragmented mp4: %v", err)
	}
	b, _ := io.ReadAll(out)
	_ = out.Close()
	if len(b) < 12 || string(b[4:8]) != "ftyp" || !bytes.Contains(b, []byte("moof")) {
		t.Errorf("fragmented mp4 = %d bytes starting %q, want ftyp and fragments", len(b), b[:min(len(b), 12)])
	}

	flac16 := Plan{Format: "flac", Encoder: "flac", Muxer: "flac", MIME: "audio/flac", Container: "flac", BitDepth: 16}
	if n := size("tone.flac", flac16, 0); n < 1000 {
		t.Errorf("flac at sixteen bits: %d bytes", n)
	}

	whole := size("tone.flac", targetPlan("mp3"), 0)
	half := size("tone.flac", targetPlan("mp3"), time.Second)
	if half >= whole*3/4 {
		t.Errorf("starting a second into two gave %d of %d bytes", half, whole)
	}
	if len(tr.slots) != 0 {
		t.Errorf("%d slots still taken after every transcode was closed", len(tr.slots))
	}
}

// TestClosingATranscodeStopsIt: a client that goes away mid-track must not
// leave ffmpeg encoding into a pipe nobody reads.
func TestClosingATranscodeStopsIt(t *testing.T) {
	t.Parallel()
	tr := newTranscoder(realFFmpeg(t), loopback(t), 1)
	out, err := tr.Transcode(t.Context(), db.File{Path: "tone.flac", BlobKey: "tone.flac"}, targetPlan("flac"), 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
	if len(tr.slots) != 0 {
		t.Error("closing a transcode half read did not give its slot back")
	}
}
