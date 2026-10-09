package web_test

import (
	"context"
	"io"
	"net/http"
	"testing"

	"github.com/C0piIot/stratus-backend/internal/auth"
	"github.com/C0piIot/stratus-backend/internal/files"
	"github.com/C0piIot/stratus-backend/internal/media"
	"github.com/C0piIot/stratus-backend/internal/web"
)

// atCapacity is a generator with every slot taken and its queue full, which is
// the one answer a real one cannot be made to give on demand.
type atCapacity struct{}

func (atCapacity) File(context.Context, string, string, int) (io.ReadCloser, int64, error) {
	return nil, 0, media.ErrBusy
}

func (atCapacity) Cover(context.Context, string, string, int) (io.ReadCloser, int64, error) {
	return nil, 0, media.ErrBusy
}

// TestAPictureThatIsNotReadyIsNotAPictureThatIsMissing: a grid on a small
// machine asks for more thumbnails than it can make at once, and what the ones
// over the line get has to be a status a client will come back from. A 404
// would not be: it says there is no picture of this file, and a browser never
// asks again.
func TestAPictureThatIsNotReadyIsNotAPictureThatIsMissing(t *testing.T) {
	t.Parallel()
	blobs, meta := backends(t)
	s := files.New(blobs, meta)
	homework(t, s, meta)

	creds := credentials()
	h := web.Handler(version, buildDate, creds, auth.NewSessions(creds, auth.DefaultSessionTTL),
		auth.NewShares(creds), s, atCapacity{}, meta, indexing(meta), nil, web.Video{})
	cookie := signIn(t, h)

	for _, target := range []string{
		"/thumb/photo.jpg?size=96",
		"/music/AC_DC/High%20Voltage/?cover=96",
	} {
		rec := get(t, h, target, cookie)
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s at capacity = %d, want 503", target, rec.Code)
		}
		if rec.Header().Get("Retry-After") == "" {
			t.Errorf("%s said to come back without saying when", target)
		}
	}
}
