package report

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/getsentry/sentry-go"

	"github.com/C0piIot/stratus-backend/internal/config"
)

// transport keeps what would have been sent.
type transport struct {
	mu     sync.Mutex
	events []*sentry.Event
}

func (t *transport) Configure(sentry.ClientOptions)        {}
func (t *transport) Flush(time.Duration) bool              { return true }
func (t *transport) FlushWithContext(context.Context) bool { return true }
func (t *transport) Close()                                {}
func (t *transport) SendEvent(e *sentry.Event) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.events = append(t.events, e)
}

func (t *transport) sent() []*sentry.Event {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]*sentry.Event(nil), t.events...)
}

func newLogger(t *testing.T, level slog.Level) (*slog.Logger, *transport, *bytes.Buffer) {
	t.Helper()
	tr := &transport{}
	client, err := sentry.NewClient(sentry.ClientOptions{
		Dsn:       "https://public@sentry.invalid/1",
		Transport: tr,
	})
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	next := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: level})
	return slog.New(New(next, client)), tr, &buf
}

func TestAnErrorIsReportedAndStillLogged(t *testing.T) {
	t.Parallel()
	log, tr, buf := newLogger(t, slog.LevelInfo)

	log.With("task", "sweep").Error("collecting orphan blobs",
		"err", errors.New("bucket unreachable"), "dsn", config.Secret("s3://k:hunter2@x/b"))

	sent := tr.sent()
	if len(sent) != 1 {
		t.Fatalf("sent %d events, want 1", len(sent))
	}
	ex := sent[0].Exception[0]
	if ex.Type != "collecting orphan blobs" || ex.Value != "bucket unreachable" {
		t.Errorf("exception = %q / %q", ex.Type, ex.Value)
	}
	fields := sent[0].Contexts["log"]
	if fields["task"] != "sweep" {
		t.Errorf("an attribute added with With is missing: %v", fields)
	}
	if strings.Contains(fields["dsn"].(string), "hunter2") {
		t.Errorf("a secret reached the report: %v", fields["dsn"])
	}
	if !strings.Contains(buf.String(), "collecting orphan blobs") {
		t.Error("the error was reported but not logged")
	}

	// Grouping is on the stack, so its top has to be whoever logged and not
	// the logger: otherwise every error would be one issue.
	frames := ex.Stacktrace.Frames
	if top := frames[len(frames)-1]; top.Function != "TestAnErrorIsReportedAndStillLogged" {
		t.Errorf("the stack ends in %s.%s", top.Module, top.Function)
	}
}

func TestOnlyErrorsAreReported(t *testing.T) {
	t.Parallel()
	log, tr, _ := newLogger(t, slog.LevelDebug)

	log.Info("indexed media", "files", 3)
	log.Warn("skipped collecting orphan blobs")

	if n := len(tr.sent()); n != 0 {
		t.Errorf("sent %d events for lines below error", n)
	}
}

// A log level above error quiets the log, and must not quiet the report with it.
func TestAnErrorIsReportedWhenTheLogIsQuieter(t *testing.T) {
	t.Parallel()
	log, tr, buf := newLogger(t, slog.LevelError+4)

	log.Error("indexing media", "err", "boom")

	if n := len(tr.sent()); n != 1 {
		t.Errorf("sent %d events, want 1", n)
	}
	if buf.Len() != 0 {
		t.Errorf("logged below the configured level: %s", buf.String())
	}
}

func TestTheSameErrorIsSentOncePerWindow(t *testing.T) {
	t.Parallel()
	log, tr, _ := newLogger(t, slog.LevelInfo)

	for range 5 {
		log.Error("indexing media", "err", "store unreachable")
	}
	log.Error("indexing media", "err", "something else")

	if n := len(tr.sent()); n != 2 {
		t.Errorf("sent %d events, want one per distinct error", n)
	}
}

func TestTheBudgetCapsDistinctErrors(t *testing.T) {
	t.Parallel()
	b := &budget{seen: map[string]struct{}{}}
	now := time.Now()

	allowed := 0
	for i := range windowCap * 2 {
		if b.allow(strings.Repeat("x", i+1), now) {
			allowed++
		}
	}
	if allowed != windowCap {
		t.Errorf("allowed %d, want %d", allowed, windowCap)
	}
	if !b.allow("x", now.Add(window)) {
		t.Error("a new window did not reset the budget")
	}
}

func TestAPanicIsReportedFromWhereItHappened(t *testing.T) {
	t.Parallel()
	log, tr, _ := newLogger(t, slog.LevelInfo)

	func() {
		defer func() {
			if p := recover(); p != nil {
				log.Error("panic in the background", "err", p)
			}
		}()
		explode()
	}()

	frames := tr.sent()[0].Exception[0].Stacktrace.Frames
	if top := frames[len(frames)-1]; top.Function != "explode" {
		t.Errorf("the stack ends in %s, want the function that panicked", top.Function)
	}
}

func explode() { panic("boom") }
