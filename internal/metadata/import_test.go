package metadata

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/plectra/plectra/internal/store"
)

func TestMatchKeyIgnoresSpellingNoise(t *testing.T) {
	same := [][2]string{
		{"Massive Attack|Teardrop", "massive attack|Teardrop (Remastered 2011)"},
		{"Queen|Under Pressure", "QUEEN|Under Pressure - Live"},
		{"Sigur Rós|Hoppípolla", "sigur ros|Hoppipolla"}, // diacritics must fold away
		{"AC/DC|Back In Black", "ACDC|Back in Black [Bonus Track]"},
	}
	for _, pair := range same {
		a := matchKey(split(pair[0]))
		b := matchKey(split(pair[1]))
		if a != b {
			t.Errorf("%q and %q did not match: %q vs %q", pair[0], pair[1], a, b)
		}
	}
	// Different songs must not collide.
	if matchKey("Queen", "Bohemian Rhapsody") == matchKey("Queen", "Under Pressure") {
		t.Error("different titles produced the same key")
	}
}

func split(s string) (string, string) {
	for i := 0; i < len(s); i++ {
		if s[i] == '|' {
			return s[:i], s[i+1:]
		}
	}
	return s, ""
}

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
