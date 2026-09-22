package history

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
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
	Token   string
	BaseURL string
	Client  *http.Client
}

func NewListenBrainz(token string) *ListenBrainz {
	return &ListenBrainz{
		Token:   token,
		BaseURL: "https://api.listenbrainz.org",
		Client:  &http.Client{Timeout: 15 * time.Second},
	}
}

// Scrobble submits one finished listen. Skips that were not completed are not
// sent: ListenBrainz expects listens, not attempts.
func (l *ListenBrainz) Scrobble(ctx context.Context, p store.Play) error {
	if l.Token == "" || !p.Completed || p.RawTitle == "" {
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
	req.Header.Set("Authorization", "Token "+l.Token)
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
