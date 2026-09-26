// Package report sends the server's errors to Sentry, when an install asks it
// to with STRATUS_SENTRY_DSN (#222).
//
// It is a slog.Handler rather than calls scattered through the code: every
// failure worth knowing about is already logged at error level, so that line is
// the one place an error gets noticed, and one written tomorrow is reported
// without anybody remembering to.
package report

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/getsentry/sentry-go"
)

// Budget, because Sentry's free tier has no rate limit of its own to set and a
// month's quota is a few thousand events: one error repeating every second
// would spend it in an afternoon and blind the rest of the month. The same
// error is sent once per window, and never more than windowCap of any kind.
const (
	window    = time.Hour
	windowCap = 30
)

// NewClient connects to the project dsn names. release is the build version,
// so an issue says which build it was first and last seen in.
func NewClient(dsn, release string) (*sentry.Client, error) {
	return sentry.NewClient(sentry.ClientOptions{
		Dsn:     dsn,
		Release: "stratus@" + release,
	})
}

// Handler passes every record to next and reports the ones at error level.
type Handler struct {
	next   slog.Handler
	hub    *sentry.Hub
	budget *budget
	attrs  []slog.Attr
	group  string
}

// New reports through client and logs through next.
func New(next slog.Handler, client *sentry.Client) *Handler {
	return &Handler{
		next:   next,
		hub:    sentry.NewHub(client, sentry.NewScope()),
		budget: &budget{seen: map[string]struct{}{}},
	}
}

// Enabled says yes to an error whatever next thinks, so that STRATUS_LOG_LEVEL
// set above error quiets the log without also quieting the report.
func (h *Handler) Enabled(ctx context.Context, level slog.Level) bool {
	return level >= slog.LevelError || h.next.Enabled(ctx, level)
}

// Handle implements slog.Handler.
func (h *Handler) Handle(ctx context.Context, r slog.Record) error {
	if r.Level >= slog.LevelError {
		h.capture(r)
	}
	if !h.next.Enabled(ctx, r.Level) {
		return nil
	}
	return h.next.Handle(ctx, r)
}

// WithAttrs implements slog.Handler, keeping the attributes for the report too.
func (h *Handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	c := *h
	c.next = h.next.WithAttrs(attrs)
	c.attrs = append(c.attrs[:len(c.attrs):len(c.attrs)], h.prefixed(attrs)...)
	return &c
}

// WithGroup implements slog.Handler; the report flattens a group into its keys.
func (h *Handler) WithGroup(name string) slog.Handler {
	c := *h
	c.next = h.next.WithGroup(name)
	c.group = h.group + name + "."
	return &c
}

func (h *Handler) prefixed(attrs []slog.Attr) []slog.Attr {
	if h.group == "" {
		return attrs
	}
	out := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		out[i] = slog.Attr{Key: h.group + a.Key, Value: a.Value}
	}
	return out
}

// capture turns a record into an exception: the message is its title and the
// err attribute its subtitle, which is how the log already reads.
func (h *Handler) capture(r slog.Record) {
	fields := make(map[string]any, len(h.attrs)+r.NumAttrs())
	var reason string
	add := func(a slog.Attr) {
		// Resolved, so that a config.Secret arrives redacted here exactly as
		// it does in the log.
		v := a.Value.Resolve().String()
		if a.Key == "err" {
			reason = v
		}
		fields[a.Key] = v
	}
	for _, a := range h.attrs {
		add(a)
	}
	r.Attrs(func(a slog.Attr) bool {
		add(slog.Attr{Key: h.group + a.Key, Value: a.Value})
		return true
	})

	if !h.budget.allow(r.Message+"\x00"+reason, r.Time) {
		return
	}

	event := sentry.NewEvent()
	event.Level = sentry.LevelError
	event.Timestamp = r.Time
	event.Exception = []sentry.Exception{{
		Type:       r.Message,
		Value:      reason,
		Stacktrace: callerStack(),
	}}
	event.Contexts["log"] = fields
	h.hub.CaptureEvent(event)
}

// callerStack is the stack of whoever logged, without the frames of the logger.
// Sentry groups on it, so leaving slog on top would make every error look like
// the same one. Inside a recover the SDK already cuts it at the panic.
func callerStack() *sentry.Stacktrace {
	st := sentry.NewStacktrace()
	if st == nil {
		return nil
	}
	frames := st.Frames
	// This package and then slog, and only when slog is there: inside a
	// recover the SDK has already cut the stack at the panic, and what is left
	// on top is the code that panicked, which may be in this package too.
	n := len(frames)
	for n > 0 && strings.HasSuffix(frames[n-1].Module, "/internal/report") {
		n--
	}
	if n > 0 && frames[n-1].Module == "log/slog" {
		for n > 0 && frames[n-1].Module == "log/slog" {
			n--
		}
		frames = frames[:n]
	}
	st.Frames = frames
	return st
}

type budget struct {
	mu      sync.Mutex
	seen    map[string]struct{}
	started time.Time
	spent   int
}

func (b *budget) allow(key string, now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	if now.Sub(b.started) >= window {
		b.started, b.spent = now, 0
		clear(b.seen)
	}
	if _, ok := b.seen[key]; ok || b.spent >= windowCap {
		return false
	}
	b.seen[key] = struct{}{}
	b.spent++
	return true
}
