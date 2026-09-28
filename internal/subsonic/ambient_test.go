package subsonic_test

import (
	"net/http/httptest"
	"testing"

	"github.com/C0piIot/stratus-backend/internal/auth"
)

// A request with no u on its query string is authenticated by what a browser
// carries (#234): the session auth.Session recognised upstream, or Basic.

func TestASignedInPageNeedsNoCredentials(t *testing.T) {
	t.Parallel()
	h := server(t)
	req := request(t, "ping", "c=stratus-web&f=json")
	req = req.WithContext(auth.WithUser(req.Context(), username))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if env := response(t, rec); env["status"] != "ok" {
		t.Errorf("ping from a signed-in page = %v", env)
	}
}

func TestBasicAuthenticatesSubsonic(t *testing.T) {
	t.Parallel()
	h := server(t)

	req := request(t, "ping", "c=stratus-tests&f=json")
	req.SetBasicAuth(username, password)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if env := response(t, rec); env["status"] != "ok" {
		t.Errorf("ping over Basic = %v", env)
	}

	req = request(t, "ping", "c=stratus-tests&f=json")
	req.SetBasicAuth(username, "not it")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if code := errorCode(t, rec); code != 40 {
		t.Errorf("a wrong Basic password = code %v, want 40", code)
	}
}

// TestBasicFromAnotherSiteIsNotAuthorized: this protocol changes state over
// GET, so a browser's cached Basic on a link from somebody else's page must
// not count. 50, because the credentials were never what was wrong.
func TestBasicFromAnotherSiteIsNotAuthorized(t *testing.T) {
	t.Parallel()
	req := request(t, "deletePlaylist", "c=stratus-tests&f=json&id=1", "Sec-Fetch-Site", "cross-site")
	req.SetBasicAuth(username, password)
	rec := httptest.NewRecorder()
	server(t).ServeHTTP(rec, req)
	if code := errorCode(t, rec); code != 50 {
		t.Errorf("cross-site Basic = code %v, want 50", code)
	}
}

func TestNoCredentialsAtAllStillAsksForU(t *testing.T) {
	t.Parallel()
	rec := get(t, server(t), "ping", "c=stratus-tests&f=json")
	if code := errorCode(t, rec); code != 10 {
		t.Errorf("no credentials = code %v, want 10", code)
	}
}
