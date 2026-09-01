package auth_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/C0piIot/stratus-backend/internal/auth"
)

// protected is a handler that records whether it was ever reached, which is the
// assertion that matters: a 401 with the body still served would be worse than
// no authentication at all.
func protected(reached *bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		*reached = true
		w.WriteHeader(http.StatusOK)
	})
}

func TestBasicAllowsTheConfiguredUser(t *testing.T) {
	t.Parallel()
	var reached bool
	h := auth.Basic("Stratus", credentials(t), protected(&reached))

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/files/", nil)
	req.SetBasicAuth(username, examplePassword)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if !reached {
		t.Error("the handler was not reached")
	}
}

func TestBasicRefuses(t *testing.T) {
	t.Parallel()
	creds := credentials(t)

	tests := []struct {
		name  string
		setup func(r *http.Request)
	}{
		{name: "no credentials at all", setup: func(*http.Request) {}},
		{name: "wrong password", setup: func(r *http.Request) { r.SetBasicAuth(username, "not it") }},
		{name: "wrong username", setup: func(r *http.Request) { r.SetBasicAuth("someone", examplePassword) }},
		{
			name:  "a header that is not Basic",
			setup: func(r *http.Request) { r.Header.Set("Authorization", `Digest username="edu"`) },
		},
		{
			name:  "Basic with something that is not base64",
			setup: func(r *http.Request) { r.Header.Set("Authorization", "Basic not-base64!") },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var reached bool
			h := auth.Basic("Stratus", creds, protected(&reached))

			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/files/", nil)
			tt.setup(req)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401", rec.Code)
			}
			if reached {
				t.Error("the handler ran anyway")
			}
			// Without the challenge a client has no way to know what to send,
			// and rclone and DAVx5 both wait for it before offering a password.
			if got := rec.Header().Get("WWW-Authenticate"); !strings.HasPrefix(got, `Basic realm="Stratus"`) {
				t.Errorf("WWW-Authenticate = %q, want a Basic challenge", got)
			}
			// RFC 7617: without this, a password with an accent in it is
			// decoded differently by different clients.
			if got := rec.Header().Get("WWW-Authenticate"); !strings.Contains(got, `charset="UTF-8"`) {
				t.Errorf("WWW-Authenticate = %q, want the UTF-8 charset", got)
			}
		})
	}
}

// TestBasicRealmCannotBreakTheHeader covers the one place a value reaches a
// header verbatim.
func TestBasicRealmCannotBreakTheHeader(t *testing.T) {
	t.Parallel()
	var reached bool
	h := auth.Basic(`Stra"tus\`, auth.Credentials{}, protected(&reached))

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	got := rec.Header().Get("WWW-Authenticate")
	if strings.Count(got, `"`) != 4 {
		t.Errorf("WWW-Authenticate = %q, want exactly the two quoted values", got)
	}
}

// TestBasicRefusesWhenNothingIsConfigured is the fail-closed case: an install
// with no credentials must not become an open server the moment a protocol
// surface is wired behind this.
func TestBasicRefusesWhenNothingIsConfigured(t *testing.T) {
	t.Parallel()
	var reached bool
	h := auth.Basic("Stratus", auth.Credentials{}, protected(&reached))

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/files/", nil)
	req.SetBasicAuth(username, examplePassword)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized || reached {
		t.Errorf("status = %d, reached = %v; want 401 and no handler", rec.Code, reached)
	}
}
