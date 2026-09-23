package native

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// The cover path is rebuilt from an id pair inside the handler. These are the
// inputs that would turn it into an open proxy or let it climb out of the cache
// directory, and each must be refused before any fetch happens.
func TestRemoteCoverRejectsAnythingButAnIDPair(t *testing.T) {
	h := newHarness(t)
	h.h = h.api.WithCoverCache(t.TempDir()).Handler()

	for _, path := range []string{
		"/api/cover/mb/..%2f..%2fetc/1",
		"/api/cover/mb/not-an-mbid/1",
		"/api/cover/mb/bd01f665-425a-44d3-8926-e550417236f5/notanumber",
		"/api/cover/mb/bd01f665-425a-44d3-8926-e550417236f5/-1",
	} {
		w := httptest.NewRecorder()
		h.h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code == 200 {
			t.Errorf("%s was accepted, want a refusal", path)
		}
	}
}

// Without a cover directory the route must not exist rather than panic on a
// nil cache.
func TestRemoteCoverRouteIsAbsentWithoutACacheDirectory(t *testing.T) {
	h := newHarness(t)
	w := httptest.NewRecorder()
	h.h.ServeHTTP(w, httptest.NewRequest("GET",
		"/api/cover/mb/bd01f665-425a-44d3-8926-e550417236f5/1", nil))
	if w.Code == 200 {
		t.Errorf("answered 200 with no cache configured")
	}
	if strings.Contains(w.Body.String(), "panic") {
		t.Errorf("panicked: %s", w.Body)
	}
}
