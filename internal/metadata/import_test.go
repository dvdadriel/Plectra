package metadata

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/plectra/plectra/internal/store"
)

func TestImportLikedMatchesLocalLibraryAndReportsMisses(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()

	local, err := st.UpsertTrack(ctx, store.Track{
		Title: "Teardrop", Artist: "Massive Attack", Album: "Mezzanine", DiscNo: 1,
		Path: "/m/teardrop.flac", FileHash: "h1",
	})
	if err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"items": []map[string]any{
				{"track": map[string]any{ // same song, noisier title
					"id": "s1", "name": "Teardrop (Remastered)",
					"artists": []map[string]string{{"name": "Massive Attack"}},
				}},
				{"track": map[string]any{ // not in the local library
					"id": "s2", "name": "Nothing Here",
					"artists": []map[string]string{{"name": "Someone Else"}},
				}},
			},
			"next": nil,
		})
	}))
	defer srv.Close()

	if err := st.SaveToken(ctx, "spotify", store.Token{
		AccessToken: "token", RefreshToken: "r", ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	sp := newTestSpotify(t, st, srv.URL)
	res, err := sp.ImportLiked(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.Matched != 1 {
		t.Fatalf("matched = %d, want 1", res.Matched)
	}
	if len(res.Unmatched) != 1 || res.Unmatched[0] != "Someone Else — Nothing Here" {
		t.Fatalf("unmatched = %v", res.Unmatched)
	}
	liked, _ := st.LikedTracks(ctx)
	if len(liked) != 1 || liked[0].ID != local {
		t.Fatalf("liked tracks = %v", liked)
	}
}

func TestExpiredTokenIsRefreshedOnce(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()

	if err := st.SaveToken(ctx, "spotify", store.Token{
		AccessToken: "stale", RefreshToken: "refresh-me", ExpiresAt: time.Now().Add(-time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	var refreshes int
	var sawBearer string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/token" {
			refreshes++
			json.NewEncoder(w).Encode(map[string]any{
				"access_token": "fresh", "expires_in": 3600,
			})
			return
		}
		sawBearer = r.Header.Get("Authorization")
		json.NewEncoder(w).Encode(map[string]any{"items": []any{}})
	}))
	defer srv.Close()

	sp := newTestSpotify(t, st, srv.URL)
	if _, err := sp.ImportLiked(ctx); err != nil {
		t.Fatal(err)
	}
	if refreshes != 1 {
		t.Fatalf("refreshes = %d, want 1", refreshes)
	}
	if sawBearer != "Bearer fresh" {
		t.Fatalf("request used %q, want the refreshed token", sawBearer)
	}
	// The refreshed token is persisted, and the refresh token survives.
	tok, err := st.LoadToken(ctx, "spotify")
	if err != nil || tok.AccessToken != "fresh" || tok.RefreshToken != "refresh-me" {
		t.Fatalf("stored token = %+v, err = %v", tok, err)
	}
}

func TestUnauthorizedIsReportedNotRetried(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	sp := newTestSpotify(t, st, "http://127.0.0.1:1") // never reached
	if _, err := sp.ImportLiked(context.Background()); err == nil {
		t.Fatal("import without a token succeeded")
	} else if !isNotAuthorized(err) {
		t.Fatalf("err = %v, want ErrNotAuthorized", err)
	}
}

func isNotAuthorized(err error) bool {
	for err != nil {
		if err == ErrNotAuthorized {
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

func newTestSpotify(t *testing.T, st *store.Store, base string) *Spotify {
	t.Helper()
	sp := NewSpotify(st, "id", "secret", "http://localhost/cb", &fakeClock{now: time.Unix(0, 0)})
	sp.BaseURL, sp.AccountsURL = base, base
	return sp
}

// Spotify's own playlists — Discover Weekly, Release Radar, editorial lists —
// are closed to applications registered after 27 November 2024, yet they still
// appear in the listing. One refusal must not cost the user every playlist they
// actually own.
func TestPlaylistSpotifyRefusesIsSkippedNotFatal(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()

	if _, err := st.UpsertTrack(ctx, store.Track{
		Title: "Teardrop", Artist: "Massive Attack", Album: "Mezzanine", DiscNo: 1,
		Path: "/m/teardrop.flac", FileHash: "h1",
	}); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/me/playlists":
			json.NewEncoder(w).Encode(map[string]any{
				"items": []map[string]any{
					{"id": "editorial", "name": "Discover Weekly"},
					{"id": "mine", "name": "Road trip"},
				},
			})
		case r.URL.Path == "/playlists/editorial/tracks":
			http.Error(w, `{"error":{"status":404,"message":"Not found."}}`, http.StatusNotFound)
		case r.URL.Path == "/playlists/mine/tracks":
			json.NewEncoder(w).Encode(map[string]any{
				"items": []map[string]any{
					{"track": map[string]any{
						"id": "s1", "name": "Teardrop",
						"artists": []map[string]string{{"name": "Massive Attack"}},
					}},
				},
			})
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	if err := st.SaveToken(ctx, "spotify", store.Token{
		AccessToken: "t", RefreshToken: "r", ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	sp := newTestSpotify(t, st, srv.URL)
	res, err := sp.ImportPlaylists(ctx)
	if err != nil {
		t.Fatalf("one refused playlist aborted the import: %v", err)
	}
	if res.Playlists != 1 || res.Matched != 1 {
		t.Fatalf("result = %+v, want the one playlist we own", res)
	}
	if len(res.Skipped) != 1 || res.Skipped[0] != "Discover Weekly" {
		t.Fatalf("skipped = %v, want Discover Weekly reported", res.Skipped)
	}

	lists, _ := st.Playlists(ctx)
	if len(lists) != 1 || lists[0].Name != "Road trip" {
		t.Fatalf("playlists = %+v, want only the user's own", lists)
	}
}

// A 403 has several causes and Spotify names the real one in the body. Guessing
// it — as an earlier version did — sends the user to fix the wrong thing.
func TestForbiddenCarriesSpotifysOwnExplanation(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()

	if err := st.SaveToken(ctx, "spotify", store.Token{
		AccessToken: "t", RefreshToken: "r", ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	// The plain-text shape Spotify actually returns for this case.
	const spotifySays = "Active premium subscription required for the owner of the app."
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, spotifySays, http.StatusForbidden)
	}))
	defer srv.Close()

	sp := newTestSpotify(t, st, srv.URL)
	_, err = sp.ImportLiked(ctx)
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("err = %v, want ErrForbidden", err)
	}
	if !strings.Contains(err.Error(), spotifySays) {
		t.Fatalf("err = %v, want it to carry Spotify's own words", err)
	}
}

// The JSON shape, which other endpoints use.
func TestForbiddenReadsTheJSONErrorMessage(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	if err := st.SaveToken(ctx, "spotify", store.Token{
		AccessToken: "t", RefreshToken: "r", ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"error":{"status":403,"message":"User not registered in the Developer Dashboard"}}`))
	}))
	defer srv.Close()

	_, err = newTestSpotify(t, st, srv.URL).ImportLiked(ctx)
	if !strings.Contains(err.Error(), "User not registered in the Developer Dashboard") {
		t.Fatalf("err = %v, want the JSON message", err)
	}
}
