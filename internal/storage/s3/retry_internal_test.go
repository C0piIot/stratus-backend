package s3

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// TestAPutSurvivesOneInternalError is STRATUS-BACKEND-5: B2 answers a PUT with
// InternalError now and then, and a body read straight off the request could
// not be sent a second time, so the first one was a 500 to the client.
func TestAPutSurvivesOneInternalError(t *testing.T) {
	t.Parallel()

	const body = "the bytes of a track"
	var puts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPut:
			got, _ := io.ReadAll(r.Body)
			if puts.Add(1) == 1 {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = io.WriteString(w, `<Error><Code>InternalError</Code><Message>internal incident</Message></Error>`)
				return
			}
			// Over plain http the client signs the body in chunks around it.
			if !strings.Contains(string(got), body) {
				t.Errorf("retried body = %q, want %q", got, body)
			}
			w.Header().Set("ETag", `"x"`)
		case http.MethodHead:
			w.Header().Set("ETag", `"x"`)
			w.Header().Set("Content-Length", "20")
			w.Header().Set("Last-Modified", time.Now().UTC().Format(http.TimeFormat))
		}
	}))
	t.Cleanup(srv.Close)

	u, _ := url.Parse(srv.URL)
	client, err := minio.New(u.Host, &minio.Options{
		Creds:  credentials.NewStaticV4("k", "s", ""),
		Region: "us-east-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	store := &Store{client: client, core: minio.Core{Client: client}, bucket: "bucket", partSize: defaultPartSize}

	// A reader with nothing but Read, which is what a request body is.
	r := io.MultiReader(strings.NewReader(body))
	if _, err := store.Put(t.Context(), "audio/x.opus", r, int64(len(body))); err != nil {
		t.Fatalf("Put = %v, want the retry to land it", err)
	}
	if n := puts.Load(); n != 2 {
		t.Errorf("PUTs = %d, want 2", n)
	}
}
