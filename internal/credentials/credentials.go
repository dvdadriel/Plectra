// Package credentials keeps the optional third-party keys in one place: stored
// in the database, entered from the UI, and applied to the running providers
// without a restart.
//
// Nothing here is required. Plectra plays music with none of it set.
package credentials

import (
	"context"
	"log"

	"github.com/plectra/plectra/internal/store"
)

// Spotify is the part of the Spotify provider this package drives.
type Spotify interface {
	SetCredentials(id, secret string)
	Configured() bool
	CredentialsLookValid() bool
	Linked(ctx context.Context) bool
}

// LastFM is the part of the Last.fm provider this package drives.
type LastFM interface {
	SetKey(key string)
	Key() string
}

// Scrobbler is the part of the ListenBrainz client this package drives.
type Scrobbler interface {
	SetToken(token string)
	Token() string
	// ValidateToken checks a token without submitting a listen.
	ValidateToken(ctx context.Context, token string) (string, error)
}

type Manager struct {
	st        *store.Store
	spotify   Spotify
	lastfm    LastFM
	scrobbler Scrobbler

	// lbUser is the account name ListenBrainz reported for the stored token, so
	// the UI can show who it will scrobble as.
	lbUser string
}

func New(st *store.Store, sp Spotify, lf LastFM, sc Scrobbler) *Manager {
	return &Manager{st: st, spotify: sp, lastfm: lf, scrobbler: sc}
}

// Load applies whatever is already stored. Values passed on the command line
// stay in force for anything the database does not override.
func (m *Manager) Load(ctx context.Context) error {
	id, err := m.st.Setting(ctx, store.KeySpotifyClientID)
	if err != nil {
		return err
	}
	secret, err := m.st.Setting(ctx, store.KeySpotifyClientSecret)
	if err != nil {
		return err
	}
	if m.spotify != nil && id != "" && secret != "" {
		m.spotify.SetCredentials(id, secret)
	}

	if key, err := m.st.Setting(ctx, store.KeyLastFMAPIKey); err != nil {
		return err
	} else if m.lastfm != nil && key != "" {
		m.lastfm.SetKey(key)
	}

	token, err := m.st.Setting(ctx, store.KeyListenBrainzToken)
	if err != nil {
		return err
	}
	if m.scrobbler != nil {
		if token != "" {
			m.scrobbler.SetToken(token)
		}
		// A token supplied on the command line is checked too: the panel should
		// say who it will scrobble as, wherever the token came from.
		if live := m.scrobbler.Token(); live != "" {
			if user, err := m.scrobbler.ValidateToken(ctx, live); err == nil {
				m.lbUser = user
			} else {
				log.Printf("listenbrainz: token check failed: %v", err)
			}
		}
	}
	return nil
}

// Save stores and applies credentials. Each field present in the request is
// applied as given — including an empty one, which clears that credential now
// rather than at the next restart.
func (m *Manager) Save(ctx context.Context, values map[string]string) error {
	id, haveID := values["spotifyClientId"]
	secret, haveSecret := values["spotifyClientSecret"]
	if haveID || haveSecret {
		if !haveID {
			id, _ = m.st.Setting(ctx, store.KeySpotifyClientID)
		}
		if !haveSecret {
			secret, _ = m.st.Setting(ctx, store.KeySpotifyClientSecret)
		}
		if err := m.st.SetSetting(ctx, store.KeySpotifyClientID, id); err != nil {
			return err
		}
		if err := m.st.SetSetting(ctx, store.KeySpotifyClientSecret, secret); err != nil {
			return err
		}
		if m.spotify != nil {
			m.spotify.SetCredentials(id, secret)
		}
	}

	if key, ok := values["lastfmApiKey"]; ok {
		if err := m.st.SetSetting(ctx, store.KeyLastFMAPIKey, key); err != nil {
			return err
		}
		if m.lastfm != nil {
			m.lastfm.SetKey(key)
		}
	}

	if token, ok := values["listenbrainzToken"]; ok {
		// Check the token before storing it, so a typo is reported now rather
		// than discovered the first time a scrobble silently fails.
		user := ""
		if token != "" && m.scrobbler != nil {
			var err error
			if user, err = m.scrobbler.ValidateToken(ctx, token); err != nil {
				return err
			}
		}
		if err := m.st.SetSetting(ctx, store.KeyListenBrainzToken, token); err != nil {
			return err
		}
		if m.scrobbler != nil {
			m.scrobbler.SetToken(token)
		}
		m.lbUser = user
	}
	return nil
}

// Status says what is set, never what it is set to.
func (m *Manager) Status(ctx context.Context) map[string]any {
	out := map[string]any{}
	if m.spotify != nil {
		out["spotifyConfigured"] = m.spotify.Configured()
		out["spotifyCredentialsValid"] = m.spotify.CredentialsLookValid()
		out["spotifyLinked"] = m.spotify.Linked(ctx)
	}
	if m.lastfm != nil {
		out["lastfmConfigured"] = m.lastfm.Key() != ""
	}
	if m.scrobbler != nil {
		out["listenbrainzConfigured"] = m.scrobbler.Token() != ""
		out["listenbrainzUser"] = m.lbUser
	}
	return out
}
