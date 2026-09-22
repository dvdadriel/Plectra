package credentials

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/plectra/plectra/internal/store"
)

type fakeSpotify struct{ id, secret string }

func (f *fakeSpotify) SetCredentials(id, secret string) { f.id, f.secret = id, secret }
func (f *fakeSpotify) Configured() bool                 { return f.id != "" && f.secret != "" }
func (f *fakeSpotify) CredentialsLookValid() bool       { return len(f.id) == 32 }
func (f *fakeSpotify) Linked(context.Context) bool      { return false }

type fakeLastFM struct{ key string }

func (f *fakeLastFM) SetKey(k string) { f.key = k }
func (f *fakeLastFM) Key() string     { return f.key }

type fakeScrobbler struct {
	token     string
	validFor  string
	validated []string
}

func (f *fakeScrobbler) SetToken(t string) { f.token = t }
func (f *fakeScrobbler) Token() string     { return f.token }
func (f *fakeScrobbler) ValidateToken(_ context.Context, token string) (string, error) {
	f.validated = append(f.validated, token)
	if token == f.validFor {
		return "listener", nil
	}
	return "", errors.New("Token invalid.")
}

func newManager(t *testing.T) (*Manager, *store.Store, *fakeSpotify, *fakeLastFM, *fakeScrobbler, context.Context) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	sp, lf, sc := &fakeSpotify{}, &fakeLastFM{}, &fakeScrobbler{validFor: "good-token"}
	return New(st, sp, lf, sc), st, sp, lf, sc, context.Background()
}

func TestSavedCredentialsAreAppliedAndSurviveReload(t *testing.T) {
	m, st, sp, lf, sc, ctx := newManager(t)

	id := "0123456789abcdef0123456789abcdef"
	err := m.Save(ctx, map[string]string{
		"spotifyClientId":     id,
		"spotifyClientSecret": "fedcba9876543210fedcba9876543210",
		"lastfmApiKey":        "lastfm-key",
		"listenbrainzToken":   "good-token",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !sp.Configured() || lf.Key() != "lastfm-key" || sc.Token() != "good-token" {
		t.Fatalf("credentials were not applied: spotify=%v lastfm=%q lb=%q",
			sp.Configured(), lf.Key(), sc.Token())
	}

	// A fresh manager over the same database reapplies them: this is what makes
	// entering them in the UI survive a restart.
	sp2, lf2, sc2 := &fakeSpotify{}, &fakeLastFM{}, &fakeScrobbler{validFor: "good-token"}
	if err := New(st, sp2, lf2, sc2).Load(ctx); err != nil {
		t.Fatal(err)
	}
	if !sp2.Configured() || lf2.Key() != "lastfm-key" || sc2.Token() != "good-token" {
		t.Fatal("stored credentials were not reapplied on load")
	}
}

// A token is checked before it is stored, so a typo is reported now rather than
// discovered when a scrobble silently fails months later.
func TestBadListenBrainzTokenIsRejectedAndNotStored(t *testing.T) {
	m, st, _, _, sc, ctx := newManager(t)

	err := m.Save(ctx, map[string]string{"listenbrainzToken": "wrong"})
	if err == nil {
		t.Fatal("a rejected token was accepted")
	}
	if stored, _ := st.Setting(ctx, store.KeyListenBrainzToken); stored != "" {
		t.Fatalf("the rejected token was stored anyway: %q", stored)
	}
	if len(sc.validated) != 1 {
		t.Fatalf("validated %d times, want exactly one check", len(sc.validated))
	}
}

func TestStatusNeverReturnsTheValues(t *testing.T) {
	m, _, _, _, _, ctx := newManager(t)
	secret := "fedcba9876543210fedcba9876543210"
	if err := m.Save(ctx, map[string]string{
		"spotifyClientId":     "0123456789abcdef0123456789abcdef",
		"spotifyClientSecret": secret,
		"lastfmApiKey":        "lastfm-key",
	}); err != nil {
		t.Fatal(err)
	}

	for k, v := range m.Status(ctx) {
		if s, ok := v.(string); ok && (s == secret || s == "lastfm-key" ||
			s == "0123456789abcdef0123456789abcdef") {
			t.Fatalf("status leaked a credential in %q", k)
		}
	}
	st := m.Status(ctx)
	if st["spotifyConfigured"] != true || st["lastfmConfigured"] != true {
		t.Fatalf("status = %v, want both reported as set", st)
	}
}

// Clearing one credential must not disturb the others.
func TestClearingOneCredentialLeavesTheRest(t *testing.T) {
	m, _, sp, lf, _, ctx := newManager(t)
	if err := m.Save(ctx, map[string]string{
		"spotifyClientId":     "0123456789abcdef0123456789abcdef",
		"spotifyClientSecret": "fedcba9876543210fedcba9876543210",
		"lastfmApiKey":        "lastfm-key",
	}); err != nil {
		t.Fatal(err)
	}
	if err := m.Save(ctx, map[string]string{"lastfmApiKey": ""}); err != nil {
		t.Fatal(err)
	}
	if lf.Key() != "" {
		t.Fatalf("last.fm key = %q, want cleared", lf.Key())
	}
	if !sp.Configured() {
		t.Fatal("clearing the last.fm key also dropped the Spotify credentials")
	}
}
