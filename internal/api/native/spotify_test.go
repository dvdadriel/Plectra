package native

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/plectra/plectra/internal/metadata"
)

// fakeLink stands in for the Spotify provider: the API layer must be testable
// without credentials or a network.
type fakeLink struct {
	configured bool
	linked     bool
	state      string
	exchanged  string
	exchangeer error
}

func (f *fakeLink) Configured() bool            { return f.configured }
func (f *fakeLink) Linked(context.Context) bool { return f.linked }
func (f *fakeLink) StartAuth() string           { return "https://accounts.example/authorize?state=" + f.state }
func (f *fakeLink) CheckState(s string) bool    { return s != "" && s == f.state }
func (f *fakeLink) Exchange(_ context.Context, code string) error {
	f.exchanged = code
	return f.exchangeer
}
func (f *fakeLink) ImportPlaylists(context.Context) (metadata.ImportResult, error) {
	return metadata.ImportResult{}, nil
}
func (f *fakeLink) ImportLiked(context.Context) (metadata.ImportResult, error) {
	return metadata.ImportResult{}, nil
}

func handlerWith(link SpotifyLink) http.Handler {
	return (&API{spotify: link}).Handler()
}

func do(h http.Handler, method, target string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(method, target, nil))
	return w
}

func TestSpotifyStatusReportsWhatIsActuallyTrue(t *testing.T) {
	cases := []struct {
		name       string
		link       SpotifyLink
		configured bool
		linked     bool
	}{
		{"no provider at all", nil, false, false},
		{"credentials missing", &fakeLink{}, false, false},
		{"configured, not linked", &fakeLink{configured: true}, true, false},
		{"linked", &fakeLink{configured: true, linked: true}, true, true},
	}
	for _, c := range cases {
		w := do(handlerWith(c.link), http.MethodGet, "/api/spotify/status")
		if w.Code != http.StatusOK {
			t.Fatalf("%s: status = %d", c.name, w.Code)
		}
		body := w.Body.String()
		want := `{"configured":` + boolText(c.configured) + `,"linked":` + boolText(c.linked) + "}\n"
		if body != want {
			t.Errorf("%s: body = %q, want %q", c.name, body, want)
		}
	}
}

func boolText(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func TestLoginWithoutCredentialsExplainsItself(t *testing.T) {
	w := do(handlerWith(&fakeLink{}), http.MethodGet, "/api/spotify/login")
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", w.Code)
	}
	// The message has to name what is missing; a bare 503 sends the user hunting.
	if body := w.Body.String(); body == "" || !contains(body, "spotify-id") {
		t.Fatalf("body = %q, want it to name the missing flags", body)
	}
}

func TestLoginRedirectsToTheProviderURL(t *testing.T) {
	link := &fakeLink{configured: true, state: "abc123"}
	w := do(handlerWith(link), http.MethodGet, "/api/spotify/login")
	if w.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", w.Code)
	}
	loc, err := url.Parse(w.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	if loc.Host != "accounts.example" || loc.Query().Get("state") != "abc123" {
		t.Fatalf("redirect = %q", w.Header().Get("Location"))
	}
}

// The state check is what stops a stray callback from handing this server a
// code it never asked for.
func TestCallbackRejectsAForgedState(t *testing.T) {
	link := &fakeLink{configured: true, state: "real-state"}
	h := handlerWith(link)

	w := do(h, http.MethodGet, "/api/spotify/callback?code=xyz&state=forged")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("forged state: status = %d, want 400", w.Code)
	}
	if link.exchanged != "" {
		t.Fatalf("the code was exchanged despite a bad state: %q", link.exchanged)
	}

	w = do(h, http.MethodGet, "/api/spotify/callback?code=xyz")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("missing state: status = %d, want 400", w.Code)
	}
}

func TestCallbackExchangesTheCodeAndReturnsHome(t *testing.T) {
	link := &fakeLink{configured: true, state: "real-state"}
	w := do(handlerWith(link), http.MethodGet, "/api/spotify/callback?code=xyz&state=real-state")
	if w.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", w.Code)
	}
	if link.exchanged != "xyz" {
		t.Fatalf("exchanged %q, want the callback code", link.exchanged)
	}
	if loc := w.Header().Get("Location"); loc != "/" {
		t.Fatalf("redirected to %q, want the UI root", loc)
	}
}

func TestCallbackReportsSpotifysOwnRefusal(t *testing.T) {
	link := &fakeLink{configured: true, state: "s"}
	w := do(handlerWith(link), http.MethodGet, "/api/spotify/callback?error=access_denied&state=s")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	if link.exchanged != "" {
		t.Fatal("a refused login still tried to exchange a code")
	}
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
