package metadata

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/plectra/plectra/internal/store"
)

// Spotify supplements MusicBrainz with cover art, consistent naming and release
// dates, and is the migration path for playlists and history. It is never the
// source of audio, and never the canonical source of metadata.
//
// Note: since late 2024 Spotify has closed /recommendations, /audio-features,
// /audio-analysis, related-artists and featured playlists to new applications,
// so nothing here may be built on those.
type Spotify struct {
	// Credentials are guarded by mu: they can be entered in the UI while the
	// server is running.
	clientID     string
	clientSecret string
	RedirectURL  string
	BaseURL      string
	AccountsURL  string
	Client       *http.Client

	st  *store.Store
	lim *limiter

	mu           sync.Mutex
	token        store.Token
	pendingState string
}

func NewSpotify(st *store.Store, clientID, clientSecret, redirectURL string, clock Clock) *Spotify {
	return &Spotify{
		clientID: clientID, clientSecret: clientSecret, RedirectURL: redirectURL,
		BaseURL:     "https://api.spotify.com/v1",
		AccountsURL: "https://accounts.spotify.com",
		Client:      &http.Client{Timeout: 20 * time.Second},
		st:          st,
		// Spotify publishes no fixed rate; a modest spacing keeps us well clear.
		lim: newLimiter(clock, 200*time.Millisecond),
	}
}

func (s *Spotify) Name() string { return "spotify" }

// SetCredentials replaces the client id and secret. Any half-finished login is
// dropped: it was started with the old application.
func (s *Spotify) SetCredentials(id, secret string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clientID, s.clientSecret = id, secret
	s.pendingState = ""
}

func (s *Spotify) credentials() (string, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.clientID, s.clientSecret
}

// Configured reports whether credentials exist at all. Without them the provider
// is simply absent, which is a supported state.
func (s *Spotify) Configured() bool {
	id, secret := s.credentials()
	return id != "" && secret != ""
}

// CredentialsLookValid reports whether the client id and secret have the shape
// Spotify issues: 32 hex characters each. A placeholder left in a script is the
// most common reason a first login fails, and Spotify's own error page does not
// say which value is at fault.
func (s *Spotify) CredentialsLookValid() bool {
	id, secret := s.credentials()
	return looksLikeCredential(id) && looksLikeCredential(secret)
}

func looksLikeCredential(v string) bool {
	return len(v) == 32 && strings.Trim(v, "0123456789abcdef") == ""
}

// ErrNotAuthorized means the user has not linked their Spotify account yet.
var ErrNotAuthorized = errors.New("spotify: not authorized")

// ErrForbidden is Spotify refusing a token it recognises. For a self-hosted
// application this nearly always means one thing: the app is in development
// mode, which serves only the accounts listed in its dashboard, and this
// account is not one of them.
var ErrForbidden = errors.New("this Spotify account is not on your application's user list. " +
	"An app in development mode only works for accounts added under " +
	"Settings → User Management in the Spotify dashboard (up to five). " +
	"Add the account you just linked, then import again")

// StartAuth begins the authorization-code flow and returns the URL to send the
// browser to. The returned state is remembered and must come back unchanged;
// without that check anyone who can reach this machine could hand the callback
// a code of their choosing.
func (s *Spotify) StartAuth() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// Without randomness the check is worthless; refuse rather than pretend.
		return ""
	}
	state := hex.EncodeToString(b)

	s.mu.Lock()
	s.pendingState = state
	s.mu.Unlock()

	return s.authURL(state)
}

// CheckState reports whether a callback carries the state this server issued,
// and consumes it so a replay of the same callback is refused.
func (s *Spotify) CheckState(state string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	want := s.pendingState
	if want == "" || subtle.ConstantTimeCompare([]byte(state), []byte(want)) != 1 {
		// A wrong state must not consume the pending one: otherwise any stray
		// request to the callback cancels the login the user is in the middle of.
		return false
	}
	s.pendingState = "" // consumed, so the same callback cannot be replayed
	return true
}

// Linked reports whether an account is connected — a token exists and has a
// refresh token to keep it alive.
func (s *Spotify) Linked(ctx context.Context) bool {
	t, err := s.st.LoadToken(ctx, s.Name())
	return err == nil && t.RefreshToken != ""
}

func (s *Spotify) authURL(state string) string {
	id, _ := s.credentials()
	q := url.Values{
		"client_id":     {id},
		"response_type": {"code"},
		"redirect_uri":  {s.RedirectURL},
		"scope":         {"playlist-read-private playlist-read-collaborative user-library-read"},
		"state":         {state},
	}
	return s.AccountsURL + "/authorize?" + q.Encode()
}

// Exchange turns the callback code into stored tokens.
func (s *Spotify) Exchange(ctx context.Context, code string) error {
	form := url.Values{
		"grant_type":   {"authorization_code"},
		"code":         {code},
		"redirect_uri": {s.RedirectURL},
	}
	return s.tokenRequest(ctx, form)
}

func (s *Spotify) tokenRequest(ctx context.Context, form url.Values) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		s.AccountsURL+"/api/token", strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	id, secret := s.credentials()
	req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString(
		[]byte(id+":"+secret)))

	resp, err := s.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("spotify token: status %d", resp.StatusCode)
	}

	var body struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return err
	}
	t := store.Token{
		AccessToken:  body.AccessToken,
		RefreshToken: body.RefreshToken,
		ExpiresAt:    time.Now().Add(time.Duration(body.ExpiresIn) * time.Second),
	}
	s.mu.Lock()
	s.token = t
	s.mu.Unlock()
	return s.st.SaveToken(ctx, s.Name(), t)
}

// accessToken returns a live token, refreshing it when needed. A refresh that
// fails disables the provider rather than retrying in a loop.
func (s *Spotify) accessToken(ctx context.Context) (string, error) {
	s.mu.Lock()
	t := s.token
	s.mu.Unlock()

	if t.AccessToken == "" {
		var err error
		t, err = s.st.LoadToken(ctx, s.Name())
		if err != nil {
			return "", ErrNotAuthorized
		}
	}
	if time.Until(t.ExpiresAt) > time.Minute {
		return t.AccessToken, nil
	}
	if t.RefreshToken == "" {
		return "", ErrNotAuthorized
	}
	err := s.tokenRequest(ctx, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {t.RefreshToken},
	})
	if err != nil {
		return "", fmt.Errorf("%w: refresh failed: %v", ErrNotAuthorized, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.token.AccessToken, nil
}

func (s *Spotify) get(ctx context.Context, path string, out any) error {
	if err := s.lim.wait(ctx); err != nil {
		return err
	}
	token, err := s.accessToken(ctx)
	if err != nil {
		return err
	}
	u := path
	if !strings.HasPrefix(u, "http") {
		u = s.BaseURL + path
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := s.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
		return &RetryableError{After: retryAfter(resp), Status: resp.StatusCode}
	case resp.StatusCode == http.StatusUnauthorized:
		return ErrNotAuthorized
	case resp.StatusCode == http.StatusForbidden:
		return ErrForbidden
	case resp.StatusCode != http.StatusOK:
		return fmt.Errorf("spotify: status %d", resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (s *Spotify) Lookup(ctx context.Context, q Query) ([]Match, error) {
	var typ, query string
	switch q.Kind {
	case KindArtist:
		typ, query = "artist", "artist:"+q.Artist
	case KindAlbum:
		typ, query = "album", "album:"+q.Album+" artist:"+q.Artist
	default:
		typ, query = "track", "track:"+q.Title+" artist:"+q.Artist
	}

	var body struct {
		Artists struct{ Items []spotifyArtist } `json:"artists"`
		Albums  struct{ Items []spotifyAlbum }  `json:"albums"`
		Tracks  struct{ Items []spotifyTrack }  `json:"tracks"`
	}
	path := "/search?type=" + typ + "&limit=5&q=" + url.QueryEscape(query)
	if err := s.get(ctx, path, &body); err != nil {
		return nil, err
	}

	var out []Match
	// Spotify returns no score, so results are ranked by their own ordering:
	// the first hit is treated as confident, the rest as fallbacks.
	score := func(i int) int { return 95 - i*10 }
	for i, a := range body.Artists.Items {
		out = append(out, Match{ID: a.ID, Artist: a.Name, Score: score(i)})
	}
	for i, al := range body.Albums.Items {
		m := Match{ID: al.ID, Album: al.Name, Year: year(al.ReleaseDate), Score: score(i)}
		if len(al.Artists) > 0 {
			m.Artist = al.Artists[0].Name
		}
		if len(al.Images) > 0 {
			m.CoverURL = al.Images[0].URL // widest image first
		}
		out = append(out, m)
	}
	for i, t := range body.Tracks.Items {
		m := Match{ID: t.ID, Title: t.Name, Album: t.Album.Name, Score: score(i)}
		if len(t.Artists) > 0 {
			m.Artist = t.Artists[0].Name
		}
		out = append(out, m)
	}
	return out, nil
}

type spotifyArtist struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type spotifyImage struct {
	URL string `json:"url"`
}

type spotifyAlbum struct {
	ID          string          `json:"id"`
	Name        string          `json:"name"`
	ReleaseDate string          `json:"release_date"`
	Artists     []spotifyArtist `json:"artists"`
	Images      []spotifyImage  `json:"images"`
}

type spotifyTrack struct {
	ID      string          `json:"id"`
	Name    string          `json:"name"`
	Album   spotifyAlbum    `json:"album"`
	Artists []spotifyArtist `json:"artists"`
}
