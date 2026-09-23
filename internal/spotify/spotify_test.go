package spotify

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/plectra/plectra/internal/store"
)

// fakeSpotify stands in for accounts.spotify.com and api.spotify.com. It checks
// what the client sends as much as it answers, because a PKCE exchange that
// omits the verifier would otherwise pass every test here.
type fakeSpotify struct {
	*httptest.Server

	challenge   string // recorded from /authorize
	lastForm    url.Values
	refreshes   int
	accessToken string
}

func newFake(t *testing.T) *fakeSpotify {
	t.Helper()
	f := &fakeSpotify{accessToken: "access-1"}
	mux := http.NewServeMux()

	mux.HandleFunc("/authorize", func(w http.ResponseWriter, r *http.Request) {
		f.challenge = r.URL.Query().Get("code_challenge")
	})

	mux.HandleFunc("/api/token", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		f.lastForm = r.PostForm
		if r.PostForm.Get("grant_type") == "refresh_token" {
			f.refreshes++
			f.accessToken = fmt.Sprintf("access-%d", f.refreshes+1)
		}
		json.NewEncoder(w).Encode(map[string]any{
			"access_token":  f.accessToken,
			"refresh_token": "refresh-1",
			"expires_in":    3600,
		})
	})

	mux.HandleFunc("/v1/me", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"display_name": "Listener"})
	})

	// Paged deliberately: the first page points at the second, so a client that
	// ignores `next` comes up short and the test notices.
	mux.HandleFunc("/v1/me/playlists", func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			http.Error(w, "no bearer", 401)
			return
		}
		if r.URL.Query().Get("page") == "2" {
			json.NewEncoder(w).Encode(map[string]any{
				"items": []any{map[string]any{"id": "p2", "name": "Second"}},
			})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"items": []any{map[string]any{"id": "p1", "name": "First"}},
			"next":  f.URL + "/v1/me/playlists?page=2",
		})
	})

	mux.HandleFunc("/v1/playlists/{id}/tracks", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("id") == "p2" {
			// An editorial list Spotify refuses to serve. One failure must not
			// lose the playlists that did work.
			http.Error(w, `{"error":{"message":"Not found."}}`, 404)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"items": []any{
			map[string]any{"track": map[string]any{
				"name":    "Aurora",
				"album":   map[string]any{"name": "First Light"},
				"artists": []any{map[string]any{"name": "Alpha Band"}},
			}},
			map[string]any{"track": nil}, // a removed track, as Spotify sends it
		}})
	})

	mux.HandleFunc("/v1/me/tracks", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"items": []any{
			map[string]any{"track": map[string]any{
				"name":    "Dusk",
				"album":   map[string]any{"name": "Second Wind"},
				"artists": []any{map[string]any{"name": "Beta Choir"}},
			}},
		}})
	})

	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Close)
	return f
}

func newService(t *testing.T) (*Service, *fakeSpotify, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	f := newFake(t)
	s := New(st, "client-1", "http://127.0.0.1:4533/api/spotify/callback")
	s.AuthBase = f.URL
	s.APIBase = f.URL + "/v1"
	return s, f, st
}

// connect walks the whole login the way the browser does.
func connect(t *testing.T, s *Service, f *fakeSpotify) {
	t.Helper()
	authURL := s.AuthURL()
	if _, err := http.Get(authURL); err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(authURL)
	if err := s.Exchange(context.Background(), u.Query().Get("state"), "the-code"); err != nil {
		t.Fatalf("Exchange: %v", err)
	}
}

func TestLoginStoresTheAccountAndSendsAPKCEVerifier(t *testing.T) {
	s, f, st := newService(t)
	connect(t, s, f)

	if f.challenge == "" {
		t.Error("no code_challenge was sent — this would not be PKCE")
	}
	if got := f.lastForm.Get("code_verifier"); got == "" {
		t.Error("the exchange sent no code_verifier")
	}
	if got := f.lastForm.Get("client_secret"); got != "" {
		t.Errorf("a client secret was sent (%q); PKCE must not need one", got)
	}

	connected, who := s.Connected(context.Background())
	if !connected || who != "Listener" {
		t.Errorf("Connected() = %v, %q; want true, %q", connected, who, "Listener")
	}
	// The refresh token is what survives a restart.
	if tok, _ := st.Setting(context.Background(), store.KeySpotifyRefreshToken); tok != "refresh-1" {
		t.Errorf("stored refresh token = %q, want refresh-1", tok)
	}
}

// A code arriving with a state this process never issued is not our login.
func TestExchangeRejectsAnUnknownState(t *testing.T) {
	s, _, _ := newService(t)
	err := s.Exchange(context.Background(), "made-up", "code")
	if err == nil {
		t.Fatal("an unknown state was accepted")
	}
	if !strings.Contains(err.Error(), "did not start here") {
		t.Errorf("error = %v, want it to say the login did not start here", err)
	}
}

func TestPlaylistsFollowPagingAndSurviveOneRefusal(t *testing.T) {
	s, f, _ := newService(t)
	connect(t, s, f)

	lists, refused, err := s.Playlists(context.Background())
	if err != nil {
		t.Fatalf("Playlists: %v", err)
	}
	// Two pages were served; the second page's playlist is the refused one, so
	// finding it named in `refused` also proves paging happened.
	if len(lists) != 1 || lists[0].Name != "First" {
		t.Fatalf("readable playlists = %+v, want just First", lists)
	}
	if len(lists[0].Tracks) != 1 {
		t.Errorf("first playlist has %d tracks, want 1 (the nil one dropped)", len(lists[0].Tracks))
	}
	if len(refused) != 1 || refused[0] != "Second" {
		t.Errorf("refused = %v, want [Second] — a playlist Spotify would not serve must be named", refused)
	}
	if got := lists[0].Tracks[0]; got.Name != "Aurora" || got.Artists[0] != "Alpha Band" {
		t.Errorf("track = %+v", got)
	}
}

func TestLikedReadsSavedSongs(t *testing.T) {
	s, f, _ := newService(t)
	connect(t, s, f)

	liked, err := s.Liked(context.Background())
	if err != nil {
		t.Fatalf("Liked: %v", err)
	}
	if len(liked) != 1 || liked[0].Name != "Dusk" {
		t.Errorf("liked = %+v, want one saved song", liked)
	}
}

// After a restart only the refresh token is left; the first call must mint a
// new access token from it rather than asking the user to log in again.
func TestExpiredAccessTokenIsRefreshed(t *testing.T) {
	s, f, st := newService(t)
	connect(t, s, f)

	fresh := New(st, "client-1", s.RedirectURI)
	fresh.AuthBase, fresh.APIBase = f.URL, f.URL+"/v1"

	if _, err := fresh.Liked(context.Background()); err != nil {
		t.Fatalf("Liked after restart: %v", err)
	}
	if f.refreshes != 1 {
		t.Errorf("refreshes = %d, want 1", f.refreshes)
	}
	if got := f.lastForm.Get("refresh_token"); got != "refresh-1" {
		t.Errorf("refreshed with %q, want the stored token", got)
	}

	// A token still valid must not be traded in again on the next call.
	if _, err := fresh.Liked(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.refreshes != 1 {
		t.Errorf("refreshes = %d after a second call; a live token was thrown away", f.refreshes)
	}
}

func TestTokenExpiryIsRespected(t *testing.T) {
	s, f, _ := newService(t)
	connect(t, s, f)
	if _, err := s.Liked(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.refreshes != 0 {
		t.Fatalf("refreshed while the token was still fresh")
	}

	s.Now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	if _, err := s.Liked(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.refreshes != 1 {
		t.Errorf("refreshes = %d after the token expired, want 1", f.refreshes)
	}
}

func TestDisconnectForgetsTheAccount(t *testing.T) {
	s, f, _ := newService(t)
	connect(t, s, f)

	if err := s.Disconnect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if connected, _ := s.Connected(context.Background()); connected {
		t.Error("still connected after Disconnect")
	}
	if _, err := s.Liked(context.Background()); err == nil {
		t.Error("Liked worked after disconnecting")
	}
}

func TestWithoutAClientIDTheServiceSaysSo(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if New(st, "", "http://x/cb").Configured() {
		t.Error("Configured() is true with no client id")
	}
}
