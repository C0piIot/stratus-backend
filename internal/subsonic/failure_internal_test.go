package subsonic

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http/httptest"
	"testing"
)

// TestAGoneClientIsNotAFailure: a read cancelled because whoever asked hung
// up is debug, and anything else -- including a cancellation that is not the
// request's -- is still an error.
func TestAGoneClientIsNotAFailure(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	gone := httptest.NewRequestWithContext(ctx, "GET", "/rest/getCoverArt", nil)
	cancel()
	here := httptest.NewRequestWithContext(t.Context(), "GET", "/rest/getCoverArt", nil)
	wrapped := fmt.Errorf("read cover: %w", context.Canceled)

	if got := failureLevel(gone, wrapped); got != slog.LevelDebug {
		t.Errorf("a client that hung up is logged at %v, want debug", got)
	}
	if got := failureLevel(gone, errors.New("bucket said no")); got != slog.LevelError {
		t.Errorf("a real failure after the client left is logged at %v, want error", got)
	}
	if got := failureLevel(here, wrapped); got != slog.LevelError {
		t.Errorf("a cancellation that is not the client's is logged at %v, want error", got)
	}
}
