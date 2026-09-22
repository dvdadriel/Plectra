package credentials

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/plectra/plectra/internal/store"
)

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

func newManager(t *testing.T) (*Manager, *store.Store, *fakeLastFM, *fakeScrobbler, context.Context) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	lf, sc := &fakeLastFM{}, &fakeScrobbler{validFor: "good-token"}
	return New(st, lf, sc), st, lf, sc, context.Background()
}

func TestSavedCredentialsAreAppliedAndSurviveReload(t *testing.T) {
	m, st, lf, sc, ctx := newManager(t)

	if err := m.Save(ctx, map[string]string{
		"lastfmApiKey":      "lastfm-key",
		"listenbrainzToken": "good-token",
	}); err != nil {
		t.Fatal(err)
	}
	if lf.Key() != "lastfm-key" || sc.Token() != "good-token" {
		t.Fatalf("not applied: lastfm=%q lb=%q", lf.Key(), sc.Token())
	}

	// A fresh manager over the same database reapplies them: this is what makes
	// entering them in the UI survive a restart.
	lf2, sc2 := &fakeLastFM{}, &fakeScrobbler{validFor: "good-token"}
	if err := New(st, lf2, sc2).Load(ctx); err != nil {
		t.Fatal(err)
	}
	if lf2.Key() != "lastfm-key" || sc2.Token() != "good-token" {
		t.Fatal("stored credentials were not reapplied on load")
	}
}

// A token is checked before it is stored, so a typo is reported now rather than
// discovered when a scrobble silently fails months later.
func TestBadListenBrainzTokenIsRejectedAndNotStored(t *testing.T) {
	m, st, _, sc, ctx := newManager(t)

	if err := m.Save(ctx, map[string]string{"listenbrainzToken": "wrong"}); err == nil {
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
	m, _, _, _, ctx := newManager(t)
	if err := m.Save(ctx, map[string]string{
		"lastfmApiKey":      "lastfm-key",
		"listenbrainzToken": "good-token",
	}); err != nil {
		t.Fatal(err)
	}

	st := m.Status(ctx)
	for k, v := range st {
		if s, ok := v.(string); ok && (s == "lastfm-key" || s == "good-token") {
			t.Fatalf("status leaked a credential in %q", k)
		}
	}
	if st["lastfmConfigured"] != true || st["listenbrainzConfigured"] != true {
		t.Fatalf("status = %v, want both reported as set", st)
	}
	if st["listenbrainzUser"] != "listener" {
		t.Fatalf("status = %v, want the account name the service reported", st)
	}
}

// Clearing one credential must not disturb the other, and must take effect now
// rather than at the next restart.
func TestClearingOneCredentialLeavesTheRest(t *testing.T) {
	m, _, lf, sc, ctx := newManager(t)
	if err := m.Save(ctx, map[string]string{
		"lastfmApiKey":      "lastfm-key",
		"listenbrainzToken": "good-token",
	}); err != nil {
		t.Fatal(err)
	}
	if err := m.Save(ctx, map[string]string{"lastfmApiKey": ""}); err != nil {
		t.Fatal(err)
	}
	if lf.Key() != "" {
		t.Fatalf("last.fm key = %q, want cleared", lf.Key())
	}
	if sc.Token() != "good-token" {
		t.Fatal("clearing the last.fm key also dropped the ListenBrainz token")
	}
}
