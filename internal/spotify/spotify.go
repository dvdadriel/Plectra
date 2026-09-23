// Package spotify links a Spotify account and reads what the user owns there:
// their playlists and their Liked Songs.
//
// Authorization Code with PKCE, not the client-secret flow. Plectra runs on the
// listener's own machine, where a secret in a config file is not a secret; PKCE
// needs only a client id and proves the exchange came from the same process
// that started the login.
//
// Nothing here plays audio. Spotify's API has no audio access at all — the
// account is a source of playlists and names, and the songs are matched against
// files already in the library.
package spotify

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/plectra/plectra/internal/store"
)

// Scopes are all read-only. Plectra cannot change anything in the account, and
// asking for less than this would mean missing private or collaborative lists.
var Scopes = []string{
	"playlist-read-private",
	"playlist-read-collaborative",
	"user-library-read",
}

type Playlist struct {
	ID     string  `json:"id"`
	Name   string  `json:"name"`
	Owner  string  `json:"owner,omitempty"`
	Tracks []Track `json:"tracks"`
}

type Track struct {
	Name    string   `json:"name"`
	Artists []string `json:"artists"`
	Album   string   `json:"album,omitempty"`
}

type Service struct {
	ClientID    string
	RedirectURI string
	HTTP        *http.Client
	Now         func() time.Time

	// Injectable so tests can point at a local server instead of Spotify.
	AuthBase string
	APIBase  string

	st *store.Store

	mu      sync.Mutex
	pending map[string]string // login state -> PKCE verifier
	access  string
	expires time.Time
}

func New(st *store.Store, clientID, redirectURI string) *Service {
	return &Service{
		ClientID:    clientID,
		RedirectURI: redirectURI,
		HTTP:        &http.Client{Timeout: 30 * time.Second},
		Now:         time.Now,
		AuthBase:    "https://accounts.spotify.com",
		APIBase:     "https://api.spotify.com/v1",
		st:          st,
		pending:     map[string]string{},
	}
}

// Configured reports whether a client id was supplied. Without one the account
// panel explains what to do instead of offering a button that cannot work.
func (s *Service) Configured() bool { return s.ClientID != "" }

func randomString() string {
	b := make([]byte, 48)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// AuthURL starts a login and returns where to send the browser. The verifier is
// held in memory only: a login that does not finish before a restart is simply
// started again.
func (s *Service) AuthURL() string {
	verifier := randomString()
	state := randomString()
	sum := sha256.Sum256([]byte(verifier))

	s.mu.Lock()
	// A login is one at a time in practice; an abandoned one must not pin
	// memory forever.
	if len(s.pending) > 8 {
		s.pending = map[string]string{}
	}
	s.pending[state] = verifier
	s.mu.Unlock()

	q := url.Values{
		"client_id":             {s.ClientID},
		"response_type":         {"code"},
		"redirect_uri":          {s.RedirectURI},
		"state":                 {state},
		"scope":                 {strings.Join(Scopes, " ")},
		"code_challenge_method": {"S256"},
		"code_challenge":        {base64.RawURLEncoding.EncodeToString(sum[:])},
	}
	return s.AuthBase + "/authorize?" + q.Encode()
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
	Error        string `json:"error"`
	ErrorDesc    string `json:"error_description"`
}

func (s *Service) postToken(ctx context.Context, form url.Values) (tokenResponse, error) {
	var tr tokenResponse
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		s.AuthBase+"/api/token", strings.NewReader(form.Encode()))
	if err != nil {
		return tr, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := s.HTTP.Do(req)
	if err != nil {
		return tr, err
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil {
		return tr, fmt.Errorf("spotify answered %d with something that is not JSON", resp.StatusCode)
	}
	if tr.Error != "" {
		// Spotify's own wording is more useful than anything invented here.
		if tr.ErrorDesc != "" {
			return tr, fmt.Errorf("%s: %s", tr.Error, tr.ErrorDesc)
		}
		return tr, fmt.Errorf("%s", tr.Error)
	}
	if tr.AccessToken == "" {
		return tr, fmt.Errorf("spotify answered %d without a token", resp.StatusCode)
	}
	return tr, nil
}

// Exchange finishes the login. The state must be one this process issued:
// without that check, any page could hand Plectra a code of its own.
func (s *Service) Exchange(ctx context.Context, state, code string) error {
	s.mu.Lock()
	verifier, ok := s.pending[state]
	delete(s.pending, state)
	s.mu.Unlock()
	if !ok {
		return fmt.Errorf("this login did not start here")
	}

	tr, err := s.postToken(ctx, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {s.RedirectURI},
		"client_id":     {s.ClientID},
		"code_verifier": {verifier},
	})
	if err != nil {
		return err
	}

	s.mu.Lock()
	s.access = tr.AccessToken
	s.expires = s.Now().Add(time.Duration(tr.ExpiresIn) * time.Second)
	s.mu.Unlock()

	if tr.RefreshToken != "" {
		if err := s.st.SetSetting(ctx, store.KeySpotifyRefreshToken, tr.RefreshToken); err != nil {
			return err
		}
	}
	// Remember who it is, so the panel can say more than "connected".
	if who, err := s.me(ctx); err == nil && who != "" {
		s.st.SetSetting(ctx, store.KeySpotifyAccount, who)
	}
	return nil
}

// token returns a usable access token, refreshing it when it has expired. The
// refresh token is the only thing that survives a restart.
func (s *Service) token(ctx context.Context) (string, error) {
	s.mu.Lock()
	// A minute of slack: a token that expires mid-import is worse than one
	// refreshed slightly early.
	if s.access != "" && s.Now().Add(time.Minute).Before(s.expires) {
		tok := s.access
		s.mu.Unlock()
		return tok, nil
	}
	s.mu.Unlock()

	refresh, err := s.st.Setting(ctx, store.KeySpotifyRefreshToken)
	if err != nil {
		return "", err
	}
	if refresh == "" {
		return "", fmt.Errorf("connect your Spotify account first")
	}

	tr, err := s.postToken(ctx, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refresh},
		"client_id":     {s.ClientID},
	})
	if err != nil {
		return "", err
	}

	s.mu.Lock()
	s.access = tr.AccessToken
	s.expires = s.Now().Add(time.Duration(tr.ExpiresIn) * time.Second)
	s.mu.Unlock()

	// Spotify sometimes rotates the refresh token; keeping the old one would
	// silently break the next session.
	if tr.RefreshToken != "" && tr.RefreshToken != refresh {
		s.st.SetSetting(ctx, store.KeySpotifyRefreshToken, tr.RefreshToken)
	}
	return tr.AccessToken, nil
}

// Connected reports whether an account is linked, and whose it is.
func (s *Service) Connected(ctx context.Context) (bool, string) {
	refresh, err := s.st.Setting(ctx, store.KeySpotifyRefreshToken)
	if err != nil || refresh == "" {
		return false, ""
	}
	who, _ := s.st.Setting(ctx, store.KeySpotifyAccount)
	return true, who
}

// Disconnect forgets the account. Spotify keeps its own record of the grant,
// which the user can revoke from their account page.
func (s *Service) Disconnect(ctx context.Context) error {
	s.mu.Lock()
	s.access, s.expires = "", time.Time{}
	s.mu.Unlock()
	if err := s.st.SetSetting(ctx, store.KeySpotifyRefreshToken, ""); err != nil {
		return err
	}
	return s.st.SetSetting(ctx, store.KeySpotifyAccount, "")
}

func (s *Service) get(ctx context.Context, url string, out any) error {
	tok, err := s.token(ctx)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)

	resp, err := s.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// Spotify explains its own 403s better than a generic message can:
		// development-mode user lists and the Premium requirement both land here.
		var e struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		json.NewDecoder(resp.Body).Decode(&e)
		if e.Error.Message != "" {
			return fmt.Errorf("spotify: %d %s", resp.StatusCode, e.Error.Message)
		}
		return fmt.Errorf("spotify answered %d", resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (s *Service) me(ctx context.Context) (string, error) {
	var who struct {
		DisplayName string `json:"display_name"`
		ID          string `json:"id"`
	}
	if err := s.get(ctx, s.APIBase+"/me", &who); err != nil {
		return "", err
	}
	if who.DisplayName != "" {
		return who.DisplayName, nil
	}
	return who.ID, nil
}

// page is the envelope Spotify wraps every list in.
type page[T any] struct {
	Items []T    `json:"items"`
	Next  string `json:"next"`
}

// all follows `next` to the end. Guessing a limit would quietly truncate a long
// playlist, which is the one thing an import must not do.
func all[T any](ctx context.Context, s *Service, first string) ([]T, error) {
	var out []T
	for url := first; url != ""; {
		var p page[T]
		if err := s.get(ctx, url, &p); err != nil {
			return nil, err
		}
		out = append(out, p.Items...)
		url = p.Next
	}
	return out, nil
}

type apiTrack struct {
	Name    string                  `json:"name"`
	Album   struct{ Name string }   `json:"album"`
	Artists []struct{ Name string } `json:"artists"`
}

func convert(t apiTrack) (Track, bool) {
	if t.Name == "" {
		return Track{}, false // a removed or unavailable track comes back empty
	}
	names := make([]string, 0, len(t.Artists))
	for _, a := range t.Artists {
		names = append(names, a.Name)
	}
	return Track{Name: t.Name, Artists: names, Album: t.Album.Name}, true
}

// Playlists reads every playlist the account can see, each with its full track
// listing. The second return is the names of playlists Spotify refused to serve:
// dropping them silently would let an import look complete when it was not.
func (s *Service) Playlists(ctx context.Context) ([]Playlist, []string, error) {
	type plItem struct {
		ID    string `json:"id"`
		Name  string `json:"name"`
		Owner struct {
			DisplayName string `json:"display_name"`
		} `json:"owner"`
	}
	lists, err := all[plItem](ctx, s, s.APIBase+"/me/playlists?limit=50")
	if err != nil {
		return nil, nil, err
	}

	type trackItem struct {
		Track apiTrack `json:"track"`
	}
	var refused []string
	out := make([]Playlist, 0, len(lists))
	for _, pl := range lists {
		items, err := all[trackItem](ctx, s,
			fmt.Sprintf("%s/playlists/%s/tracks?limit=100", s.APIBase, pl.ID))
		if err != nil {
			// One unreadable playlist must not lose the other forty. Spotify's
			// own editorial lists answer 404 for apps registered after
			// November 2024, and that is not a reason to fail the import — but
			// the name is carried out so the user is told which ones.
			refused = append(refused, pl.Name)
			continue
		}
		tracks := make([]Track, 0, len(items))
		for _, it := range items {
			if t, ok := convert(it.Track); ok {
				tracks = append(tracks, t)
			}
		}
		out = append(out, Playlist{
			ID: pl.ID, Name: pl.Name, Owner: pl.Owner.DisplayName, Tracks: tracks,
		})
	}
	return out, refused, nil
}

// Liked reads the account's saved songs.
func (s *Service) Liked(ctx context.Context) ([]Track, error) {
	type item struct {
		Track apiTrack `json:"track"`
	}
	items, err := all[item](ctx, s, s.APIBase+"/me/tracks?limit=50")
	if err != nil {
		return nil, err
	}
	out := make([]Track, 0, len(items))
	for _, it := range items {
		if t, ok := convert(it.Track); ok {
			out = append(out, t)
		}
	}
	return out, nil
}
