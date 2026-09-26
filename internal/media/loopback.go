package media

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/C0piIot/stratus-backend/internal/db"
)

// Handing a blob to ffmpeg without copying it first.
//
// ffmpeg opens files and seeks in them, and the storage port streams. A pipe
// is no answer: an m4a whose moov atom sits at the end cannot be read from one
// at all. So ffmpeg reads over HTTP, which it can seek in by asking for a range
// (build/ffmpeg/Dockerfile enables the protocol for this), from a listener on
// the loopback interface that serves one file per token, through the same
// seekable reader and the same http.ServeContent the protocol surfaces serve a
// track with. It works for a disk and a bucket alike, and nothing is ever
// written to the local disk.
//
// It is not a surface. It listens on 127.0.0.1 and nowhere else, a token is
// 128 random bits naming one file, and the token lives exactly as long as the
// transcode that asked for it: ffmpeg makes several requests for a file it has
// to seek in, so a token is not spent by its first one.

// Opener is what the loopback reads a file through: internal/files.
type Opener interface {
	OpenFile(ctx context.Context, f db.File) (io.ReadSeekCloser, error)
}

// Loopback serves files to the ffmpeg processes this server starts.
type Loopback struct {
	files Opener
	base  string
	srv   *http.Server

	mu     sync.Mutex
	grants map[string]db.File
}

// NewLoopback listens on a free port of the loopback interface.
func NewLoopback(ctx context.Context, files Opener) (*Loopback, error) {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listen on the loopback interface for ffmpeg: %w", err)
	}
	l := &Loopback{files: files, base: "http://" + ln.Addr().String() + "/", grants: map[string]db.File{}}
	l.srv = &http.Server{Handler: http.HandlerFunc(l.serve), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := l.srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("the loopback listener for ffmpeg stopped", "err", err)
		}
	}()
	return l, nil
}

// Grant makes f readable at the URL it returns until revoke is called.
func (l *Loopback) Grant(f db.File) (url string, revoke func()) {
	var b [16]byte
	_, _ = rand.Read(b[:]) // crypto/rand does not fail
	token := hex.EncodeToString(b[:])

	l.mu.Lock()
	l.grants[token] = f
	l.mu.Unlock()
	return l.base + token, func() {
		l.mu.Lock()
		delete(l.grants, token)
		l.mu.Unlock()
	}
}

func (l *Loopback) serve(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimPrefix(r.URL.Path, "/")
	l.mu.Lock()
	f, ok := l.grants[token]
	l.mu.Unlock()
	if !ok || (r.Method != http.MethodGet && r.Method != http.MethodHead) {
		http.NotFound(w, r)
		return
	}

	body, err := l.files.OpenFile(r.Context(), f)
	if err != nil {
		http.Error(w, "the file could not be opened", http.StatusInternalServerError)
		return
	}
	defer func() { _ = body.Close() }()
	http.ServeContent(w, r, path.Base(f.Path), f.MTime, body)
}

// Close stops the listener.
func (l *Loopback) Close() error { return l.srv.Close() }
