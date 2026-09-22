package history

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/plectra/plectra/internal/store"
)

// lbServer records what reached the endpoint, and answers with status.
type lbServer struct {
	*httptest.Server
	requests int
	auth     string
	path     string
	body     []byte
}

func newLBServer(t *testing.T, status int) *lbServer {
	t.Helper()
	s := &lbServer{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.requests++
		s.auth = r.Header.Get("Authorization")
		s.path = r.URL.Path
		s.body, _ = io.ReadAll(r.Body)
		w.WriteHeader(status)
		io.WriteString(w, `{"status":"ok"}`)
	}))
	t.Cleanup(s.Close)
	return s
}

func completedPlay() store.Play {
	return store.Play{
		PlayedAt:  1700000000,
		MSPlayed:  201000,
		Completed: true,
		Source:    SourcePlectra,
		RawArtist: "Massive Attack",
		RawAlbum:  "Mezzanine",
		RawTitle:  "Teardrop",
	}
}

func TestScrobbleSubmitsListenWithTokenAndMetadata(t *testing.T) {
	srv := newLBServer(t, http.StatusOK)
	lb := &ListenBrainz{Token: "secret-token", BaseURL: srv.URL, Client: srv.Client()}

	if err := lb.Scrobble(context.Background(), completedPlay()); err != nil {
		t.Fatal(err)
	}
	if srv.requests != 1 {
		t.Fatalf("requests = %d, want 1", srv.requests)
	}
	if srv.auth != "Token secret-token" {
		t.Fatalf("Authorization = %q, want %q", srv.auth, "Token secret-token")
	}
	if srv.path != "/1/submit-listens" {
		t.Fatalf("path = %q, want /1/submit-listens", srv.path)
	}

	var got struct {
		ListenType string `json:"listen_type"`
		Payload    []struct {
			ListenedAt    int64 `json:"listened_at"`
			TrackMetadata struct {
				ArtistName  string `json:"artist_name"`
				TrackName   string `json:"track_name"`
				ReleaseName string `json:"release_name"`
			} `json:"track_metadata"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(srv.body, &got); err != nil {
		t.Fatalf("payload is not the documented shape: %v (%s)", err, srv.body)
	}
	if len(got.Payload) != 1 {
		t.Fatalf("payload has %d listens, want 1: %s", len(got.Payload), srv.body)
	}
	l := got.Payload[0]
	// listened_at is the play's own timestamp, not the moment of submission.
	if l.ListenedAt != 1700000000 {
		t.Errorf("listened_at = %d, want 1700000000", l.ListenedAt)
	}
	if l.TrackMetadata.ArtistName != "Massive Attack" {
		t.Errorf("artist_name = %q, want Massive Attack", l.TrackMetadata.ArtistName)
	}
	if l.TrackMetadata.TrackName != "Teardrop" {
		t.Errorf("track_name = %q, want Teardrop", l.TrackMetadata.TrackName)
	}
	if l.TrackMetadata.ReleaseName != "Mezzanine" {
		t.Errorf("release_name = %q, want Mezzanine", l.TrackMetadata.ReleaseName)
	}
}

// ListenBrainz expects listens, not attempts — and without a token there is
// nobody to submit as. Neither case may touch the network at all.
func TestScrobbleSendsNothingWhenSkippedOrUnconfigured(t *testing.T) {
	skipped := completedPlay()
	skipped.Completed = false

	cases := []struct {
		name  string
		token string
		play  store.Play
	}{
		{"skipped play", "secret-token", skipped},
		{"empty token", "", completedPlay()},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := newLBServer(t, http.StatusOK)
			lb := &ListenBrainz{Token: c.token, BaseURL: srv.URL, Client: srv.Client()}
			if err := lb.Scrobble(context.Background(), c.play); err != nil {
				t.Fatalf("Scrobble returned %v, want nil", err)
			}
			if srv.requests != 0 {
				t.Fatalf("server saw %d requests, want none", srv.requests)
			}
		})
	}
}

// A rejected submission is an error the caller can log, never a panic.
func TestScrobbleReturnsErrorOnNon200(t *testing.T) {
	srv := newLBServer(t, http.StatusUnauthorized)
	lb := &ListenBrainz{Token: "bad-token", BaseURL: srv.URL, Client: srv.Client()}

	err := lb.Scrobble(context.Background(), completedPlay())
	if err == nil {
		t.Fatal("Scrobble returned nil for a 401 response, want an error")
	}
	if srv.requests != 1 {
		t.Fatalf("requests = %d, want 1", srv.requests)
	}
}
