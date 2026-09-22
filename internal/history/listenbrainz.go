package history

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/plectra/plectra/internal/store"
)

// Scrobbler sends finished plays somewhere else. It is optional at every level:
// a scrobble that fails is logged and forgotten, never retried into the music.
type Scrobbler interface {
	Scrobble(ctx context.Context, p store.Play) error
}

// ListenBrainz is the open scrobbling target named in the spec: free, and the
// history can be exported again at any time.
type ListenBrainz struct {
	BaseURL string
	Client  *http.Client

	mu    sync.RWMutex
	token string
}

func NewListenBrainz(token string) *ListenBrainz {
	return &ListenBrainz{
		token:   token,
		BaseURL: "https://api.listenbrainz.org",
		Client:  &http.Client{Timeout: 15 * time.Second},
	}
}

// Scrobble submits one finished listen. Skips that were not completed are not
// sent: ListenBrainz expects listens, not attempts.
// SetToken replaces the token; an empty one turns scrobbling off.
func (l *ListenBrainz) SetToken(token string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.token = token
}

func (l *ListenBrainz) Token() string {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.token
}

// ValidateToken asks ListenBrainz whether the token works, without submitting
// anything. Checking a credential must never write to someone's listen history.
func (l *ListenBrainz) ValidateToken(ctx context.Context, token string) (string, error) {
	if token == "" {
		return "", nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, l.BaseURL+"/1/validate-token", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Token "+token)

	resp, err := l.Client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("listenbrainz: status %d", resp.StatusCode)
	}
	var body struct {
		Valid    bool   `json:"valid"`
		UserName string `json:"user_name"`
		Message  string `json:"message"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", err
	}
	if !body.Valid {
		return "", fmt.Errorf("listenbrainz: %s", body.Message)
	}
	return body.UserName, nil
}

func (l *ListenBrainz) Scrobble(ctx context.Context, p store.Play) error {
	if l.Token() == "" || !p.Completed || p.RawTitle == "" {
		return nil
	}
	payload := map[string]any{
		"listen_type": "single",
		"payload": []map[string]any{{
			"listened_at": p.PlayedAt,
			"track_metadata": map[string]any{
				"artist_name":  p.RawArtist,
				"track_name":   p.RawTitle,
				"release_name": p.RawAlbum,
				"additional_info": map[string]any{
					"media_player":      "Plectra",
					"submission_client": "Plectra",
				},
			},
		}},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		l.BaseURL+"/1/submit-listens", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Token "+l.Token())
	req.Header.Set("Content-Type", "application/json")

	resp, err := l.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("listenbrainz: status %d", resp.StatusCode)
	}
	return nil
}
