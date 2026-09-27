package media

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/C0piIot/stratus-backend/internal/db"
	"github.com/C0piIot/stratus-backend/internal/storage/disk"
)

func TestEncodeFor(t *testing.T) {
	t.Parallel()
	film := func(mut func(*db.Media)) db.Media {
		m := db.Media{Kind: db.KindVideo, Codec: "hevc", Width: 3840, Height: 2160, DurationMS: 60_000,
			FrameRate: 29_970, AudioCodec: "aac", Channels: 2}
		if mut != nil {
			mut(&m)
		}
		return m
	}
	for name, c := range map[string]struct {
		m    db.Media
		want Encode
		ok   bool
	}{
		"4K brought to 1080p, sound copied": {film(nil), Encode{Width: 1920, Height: 1080, CopyAudio: true}, true},
		"an iPhone's HLG, upright": {film(func(m *db.Media) {
			m.Width, m.Height, m.Orientation = 1920, 1080, 6
			m.ColorPrimaries, m.ColorTransfer, m.ColorSpace, m.DoViProfile = "bt2020", "arib-std-b67", "bt2020nc", 8
		}), Encode{Width: 1080, Height: 1920, CopyAudio: true, Tonemap: true, Primaries: "bt2020", Transfer: "arib-std-b67", Space: "bt2020nc"}, true},
		"PQ with its colour unstated but for the curve": {film(func(m *db.Media) { m.ColorTransfer = "smpte2084" }),
			Encode{Width: 1920, Height: 1080, CopyAudio: true, Tonemap: true, Primaries: "bt2020", Transfer: "smpte2084", Space: "bt2020nc"}, true},
		"AC-3 sound becomes AAC, 60 fps a higher level": {film(func(m *db.Media) { m.AudioCodec, m.FrameRate = "ac3", 59_940 }),
			Encode{Width: 1920, Height: 1080, HighRate: true}, true},
		"a small film is not enlarged":      {film(func(m *db.Media) { m.Width, m.Height = 640, 480 }), Encode{Width: 640, Height: 480, CopyAudio: true}, true},
		"Dolby Vision profile 5 is refused": {film(func(m *db.Media) { m.DoViProfile = 5 }), Encode{}, false},
		"a film not read yet":               {film(func(m *db.Media) { m.Width = 0 }), Encode{}, false},
		"a track is not a film":             {film(func(m *db.Media) { m.Kind = db.KindAudio }), Encode{}, false},
	} {
		got, err := EncodeFor(c.m)
		if got != c.want || (err == nil) != c.ok {
			t.Errorf("%s:\n got  %+v, %v\n want %+v", name, got, err, c.want)
		}
	}
}

func TestNeedsEncode(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		m    db.Media
		want bool
	}{
		"eight-bit SDR H.264 at 1080p":   {db.Media{Codec: "h264", BitDepth: 8, Width: 1920, Height: 1080}, false},
		"an upright phone film at 1080p": {db.Media{Codec: "h264", BitDepth: 8, Width: 1920, Height: 1080, Orientation: 6}, false},
		"HEVC":                           {db.Media{Codec: "hevc", BitDepth: 8, Width: 1280, Height: 720}, true},
		"4K H.264":                       {db.Media{Codec: "h264", BitDepth: 8, Width: 3840, Height: 2160}, true},
		"ten-bit H.264":                  {db.Media{Codec: "h264", BitDepth: 10, Width: 1280, Height: 720}, true},
		"HDR H.264":                      {db.Media{Codec: "h264", BitDepth: 8, Width: 1280, Height: 720, ColorTransfer: "arib-std-b67"}, true},
	} {
		if got := NeedsEncode(c.m); got != c.want {
			t.Errorf("%s: %v", name, got)
		}
	}
}

func TestEncodedSegmentsAndNames(t *testing.T) {
	t.Parallel()
	got := EncodedSegments(sec(14.5))
	want := []Segment{{Start: 0, Length: sec(6)}, {Start: sec(6), Length: sec(6)}, {Start: sec(12), Length: sec(2.5)}}
	if !slices.Equal(got, want) {
		t.Errorf("EncodedSegments = %v", got)
	}
	s := Segment{Start: sec(12), Length: sec(2.5)}
	if EncodedName(s) != "h12000-2500.ts" {
		t.Fatalf("EncodedName = %q", EncodedName(s))
	}
	if back, err := ParseEncoded(EncodedName(s)); err != nil || back != s {
		t.Errorf("ParseEncoded = %v, %v", back, err)
	}
	for _, bad := range []string{"", "12000-2500.ts", "h12000-2500", "hx-1.ts", "h0-0.ts", "h0-7000.ts", "h-5-100.ts", "0-6000-150-0.ts"} {
		if _, err := ParseEncoded(bad); err == nil {
			t.Errorf("ParseEncoded(%q) was accepted", bad)
		}
	}
}

func TestMasterPlaylistAndCodecs(t *testing.T) {
	t.Parallel()
	m := db.Media{Codec: "hevc", CodecProfile: "Main 10", Level: 150, Width: 3840, Height: 2160, Bitrate: 20_000_000, AudioCodec: "aac", Channels: 2}
	e, _ := EncodeFor(db.Media{Kind: db.KindVideo, Width: 3840, Height: 2160, DurationMS: 1, AudioCodec: "aac"})
	got := MasterPlaylist([]Variant{RemuxVariant(m, "?hls=copy.m3u8"), EncodeVariant(m, e, "?hls=h264.m3u8")})
	want := "#EXTM3U\n#EXT-X-VERSION:3\n" +
		"#EXT-X-STREAM-INF:BANDWIDTH=20000000,RESOLUTION=3840x2160,CODECS=\"hvc1.2.4.L150.B0,mp4a.40.2\"\n?hls=copy.m3u8\n" +
		"#EXT-X-STREAM-INF:BANDWIDTH=8192000,RESOLUTION=1920x1080,CODECS=\"avc1.640028,mp4a.40.2\"\n?hls=h264.m3u8\n"
	if got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
	for c, want := range map[[2]string]string{
		{"Constrained Baseline", "30"}: "avc1.42401e", {"Main", "31"}: "avc1.4d001f", {"High", "41"}: "avc1.640029",
		{"High 10", "51"}: "avc1.6e0033", {"", "0"}: "avc1.640029",
	} {
		level := map[string]int{"30": 30, "31": 31, "41": 41, "51": 51, "0": 0}[c[1]]
		if got := avcCodecs(c[0], level); got != want {
			t.Errorf("avcCodecs(%q, %d) = %q, want %q", c[0], level, got, want)
		}
	}
	if got := hevcCodecs("Main", 0); got != "hvc1.1.6.L120.B0" {
		t.Errorf("hevcCodecs = %q", got)
	}
	if v := RemuxVariant(db.Media{Codec: "h264", CodecProfile: "High", Level: 40, AudioCodec: "mp3"}, "u"); v.Codecs != "avc1.640028,mp4a.40.34" {
		t.Errorf("mp3 in a remux = %q", v.Codecs)
	}
}

func TestEncodeArgs(t *testing.T) {
	t.Parallel()
	plain := encodeArgs(Encode{Width: 1280, Height: 720}, Segment{Start: sec(6), Length: sec(6)}, 2)("u")
	for _, want := range []string{"-ss", "6", "-to", "12", "scale=1280:720,format=yuv420p", "libx264", "-threads", "2", "aac"} {
		if !slices.Contains(plain, want) {
			t.Errorf("plain args lack %q: %q", want, plain)
		}
	}
	hdr := encodeArgs(Encode{Width: 1920, Height: 1080, Tonemap: true, Primaries: "bt2020", Transfer: "arib-std-b67", Space: "bt2020nc", CopyAudio: true},
		Segment{Length: sec(6)}, 1)("u")
	vf := hdr[slices.Index(hdr, "-vf")+1]
	if !strings.HasPrefix(vf, "setparams=color_primaries=bt2020:color_trc=arib-std-b67:colorspace=bt2020nc") ||
		!strings.Contains(vf, "zscale=w=1920:h=1080:t=linear") || !strings.Contains(vf, "tonemap=hable") || slices.Contains(hdr, "-ss") {
		t.Errorf("HDR -vf = %q", vf)
	}
	if !slices.Contains(hdr, "copy") {
		t.Errorf("sound already AAC was re-encoded: %q", hdr)
	}
}

func TestEncodeSlots(t *testing.T) {
	t.Parallel()
	const mib = 1 << 20
	for name, c := range map[string]struct {
		cpus  int
		limit int64
		want  int
	}{
		"four cores and plenty": {4, 8192 * mib, 2},
		"one core":              {1, 1024 * mib, 1},
		"eight cores, 512 MB":   {8, 512 * mib, 1},
		"a limit unread":        {6, 0, 3},
	} {
		if got := encodeSlots(c.cpus, c.limit); got != c.want {
			t.Errorf("%s: %d, want %d", name, got, c.want)
		}
	}
}

// TestEncodeForReal: each HDR fixture and the ten-bit one re-encoded by the
// binary, where there is one, into eight-bit H.264 in MPEG-TS -- and a second
// request answered from the store without ffmpeg at all.
func TestEncodeForReal(t *testing.T) {
	t.Parallel()
	ffmpeg := realFFmpeg(t)
	blobs, err := disk.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = blobs.Close() })
	tr := newTranscoder(ffmpeg, loopback(t), 4)
	enc := &Encoder{tr: tr, blobs: blobs, slots: make(chan struct{}, 2), threads: 1}

	for name, m := range map[string]db.Media{
		"main10.mp4": {Kind: db.KindVideo, Codec: "hevc", Width: 64, Height: 64, DurationMS: 1000, AudioCodec: "aac"},
		"hlg.mp4":    {Kind: db.KindVideo, Codec: "hevc", Width: 64, Height: 64, DurationMS: 1000, ColorTransfer: "arib-std-b67"},
		"pq.mkv":     {Kind: db.KindVideo, Codec: "hevc", Width: 64, Height: 64, DurationMS: 1000, ColorTransfer: "smpte2084"},
	} {
		e, err := EncodeFor(m)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		f := db.File{Path: name, BlobKey: name}
		s := EncodedSegments(time.Second)[0]
		first := readAll(t, enc, f, e, s)
		if len(first) < 376 || len(first)%188 != 0 || first[0] != 0x47 {
			t.Errorf("%s: %d bytes that are not MPEG-TS", name, len(first))
		}
		// A second time, the store answers -- which a stub in ffmpeg's place
		// proves, since it cannot make anything.
		cached := &Encoder{tr: newTranscoder(stubFFmpeg(t), loopback(t), 1), blobs: blobs, slots: make(chan struct{}, 1), threads: 1}
		if again := readAll(t, cached, f, e, s); string(again) != string(first) {
			t.Errorf("%s: a second request was not answered from the store", name)
		}
		if ffprobe, err := exec.LookPath("ffprobe"); err == nil {
			path := filepath.Join(t.TempDir(), "seg.ts")
			if err := os.WriteFile(path, first, 0o600); err != nil {
				t.Fatal(err)
			}
			out, _ := exec.CommandContext(t.Context(), ffprobe, "-v", "error", "-select_streams", "v:0",
				"-show_entries", "stream=codec_name,pix_fmt,color_transfer", "-of", "csv=p=0", path).Output()
			if got := strings.TrimSpace(string(out)); !strings.HasPrefix(got, "h264,yuv420p") {
				t.Errorf("%s: the segment is %q, want eight-bit H.264", name, got)
			}
		}
	}
}

func readAll(t *testing.T, enc *Encoder, f db.File, e Encode, s Segment) []byte {
	t.Helper()
	out, err := enc.Segment(t.Context(), f, e, s)
	if err != nil {
		t.Fatalf("%s: %v", f.Path, err)
	}
	defer func() { _ = out.Close() }()
	b, err := io.ReadAll(out)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
