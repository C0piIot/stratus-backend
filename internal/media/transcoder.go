package media

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/C0piIot/stratus-backend/internal/db"
)

// Running a transcode: one ffmpeg per playback, writing into the response.
//
// It lives as long as the track does -- ffmpeg encodes faster than anybody
// listens, fills the pipe and then waits on it -- so what bounds it is not the
// CPU it takes, which is little, but how many can be alive at once. When they
// all are, ErrBusy says so and the caller decides what a client hears instead.

// ErrBusy means every transcode this machine allows is already running.
var ErrBusy = errors.New("media: every transcode slot is taken")

// Transcoder starts ffmpeg over a file the loopback serves.
type Transcoder struct {
	ffmpeg string
	source *Loopback
	slots  chan struct{}
}

// NewTranscoder sizes itself to the machine, the way the thumbnails do.
func NewTranscoder(ffmpeg string, source *Loopback) *Transcoder {
	slots := transcodes()
	// Said at startup for the reason the thumbnails say theirs: nobody set it.
	slog.Info("transcodes", "at_once", slots)
	return newTranscoder(ffmpeg, source, slots)
}

func newTranscoder(ffmpeg string, source *Loopback, slots int) *Transcoder {
	return &Transcoder{ffmpeg: ffmpeg, source: source, slots: make(chan struct{}, slots)}
}

// Transcode starts producing p from f, offset into it, and returns the output
// once ffmpeg has written its first byte -- so that a file it cannot read is an
// error here, while the caller can still answer with one, and not an empty 200.
// Closing what it returns stops ffmpeg, and so does ctx ending, which for a
// request is the client going away.
func (t *Transcoder) Transcode(ctx context.Context, f db.File, p Plan, offset time.Duration) (io.ReadCloser, error) {
	select {
	case t.slots <- struct{}{}:
	default:
		return nil, ErrBusy
	}

	url, revoke := t.source.Grant(f)
	ctx, cancel := context.WithCancel(ctx)
	release := sync.OnceFunc(func() {
		cancel()
		revoke()
		<-t.slots
	})

	// The binary is resolved once at startup and never comes from a request;
	// every other argument is built here from a Plan.
	//nolint:gosec // see above
	cmd := exec.CommandContext(ctx, t.ffmpeg, transcodeArgs(url, p, offset)...)
	stderr := &boundedBuffer{max: 4 << 10}
	cmd.Stderr = stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		release()
		return nil, fmt.Errorf("ffmpeg: %w", err)
	}
	if err := cmd.Start(); err != nil {
		release()
		return nil, fmt.Errorf("ffmpeg: %w", err)
	}

	out := &transcodeOutput{r: bufio.NewReaderSize(stdout, 32<<10), cmd: cmd, release: release}
	if _, err := out.r.Peek(1); err != nil {
		_ = out.Close()
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return nil, fmt.Errorf("ffmpeg: %s", msg)
		}
		return nil, fmt.Errorf("ffmpeg produced nothing for %q: %w", f.Path, err)
	}
	return out, nil
}

// transcodeArgs is the whole of what a transcode asks of ffmpeg, split out so
// it can be read and tested without a binary.
func transcodeArgs(url string, p Plan, offset time.Duration) []string {
	args := []string{"-hide_banner", "-v", "error", "-nostdin"}
	if offset > 0 {
		// Before -i, which jumps rather than decoding everything on the way:
		// this is what transcodeOffset is, and it has to be quick, since it is
		// what dragging the bar does.
		args = append(args, "-ss", strconv.FormatFloat(offset.Seconds(), 'f', -1, 64))
	}
	args = append(args,
		// Nothing but the loopback. The URL is ours, and a file whose
		// container names another -- an HLS playlist, a reference movie --
		// must not send ffmpeg anywhere else.
		"-protocol_whitelist", "http,tcp",
		"-i", url,
		// The first audio stream and nothing else: a cover is a video stream to
		// ffmpeg, and it would try to put it in an MP3.
		"-map", "0:a:0", "-vn", "-sn", "-dn",
		"-c:a", p.Encoder,
	)
	if p.Bitrate > 0 {
		args = append(args, "-b:a", strconv.Itoa(p.Bitrate))
	}
	if p.SampleRate > 0 {
		args = append(args, "-ar", strconv.Itoa(p.SampleRate))
	}
	if p.Channels > 0 {
		args = append(args, "-ac", strconv.Itoa(p.Channels))
	}
	return append(args, "-f", p.Muxer, "-")
}

// transcodeOutput is ffmpeg's stdout, and closing it is ending the process.
type transcodeOutput struct {
	r       *bufio.Reader
	cmd     *exec.Cmd
	release func()
	once    sync.Once
}

func (o *transcodeOutput) Read(p []byte) (int, error) { return o.r.Read(p) }

// Close stops ffmpeg if it is still going and gives its slot back. A process
// that was killed exits with an error, which is what was asked for and not
// worth reporting.
func (o *transcodeOutput) Close() error {
	o.once.Do(func() {
		o.release()
		_ = o.cmd.Wait()
	})
	return nil
}

// boundedBuffer keeps the start of what ffmpeg says: its first error is the
// one worth reporting, and a process that loops on a warning must not grow
// this without limit.
type boundedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
	max int
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if room := b.max - b.buf.Len(); room > 0 {
		b.buf.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

func (b *boundedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
