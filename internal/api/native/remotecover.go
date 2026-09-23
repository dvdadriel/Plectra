package native

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"
)

// Chart artwork lives on archive.org, which measured four to thirteen seconds a
// file and five seconds even warm. Thirty-six of those on one page is not a
// page. So each one is fetched once, written into the same cover cache the
// scanner uses, and served from disk ever after.
//
// The URL is rebuilt here from an id pair rather than accepted from the client:
// a handler that fetches whatever URL it is handed is an open proxy.
type remoteCovers struct {
	dir  string
	http *http.Client

	mu      sync.Mutex
	pending map[string]chan struct{} // one fetch per file, however many ask
}

func newRemoteCovers(dir string) *remoteCovers {
	return &remoteCovers{
		dir:     dir,
		http:    &http.Client{Timeout: 45 * time.Second},
		pending: map[string]chan struct{}{},
	}
}

// mbid is the only shape accepted for either path segment. Anything else could
// climb out of the cache directory.
var mbidRe = regexp.MustCompile(`^[0-9a-f-]{36}$`)

func (a *API) remoteCoverRoutes(mux *http.ServeMux) {
	if a.covers == nil {
		return
	}
	mux.HandleFunc("GET /api/cover/mb/{release}/{caa}", a.remoteCover)
	mux.HandleFunc("GET /api/cover/rg/{group}", a.releaseGroupCover)
}

func (a *API) remoteCover(w http.ResponseWriter, r *http.Request) {
	release, caa := r.PathValue("release"), r.PathValue("caa")
	if !mbidRe.MatchString(release) || !isDigits(caa) {
		http.Error(w, "bad cover id", 400)
		return
	}

	path, err := a.covers.fetch(r.Context(), release, caa)
	if err != nil {
		http.NotFound(w, r) // the sleeve falls back to the album's initial
		return
	}
	// Immutable: a Cover Art Archive id names one image forever.
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	http.ServeFile(w, r, path)
}

// releaseGroupCover serves artwork for a catalogue album, which is known only by
// its release-group id — a catalogue search carries no Cover Art Archive id.
// That path goes through coverartarchive.org and its redirect, which is slow;
// caching it once is what makes it usable at all.
func (a *API) releaseGroupCover(w http.ResponseWriter, r *http.Request) {
	group := r.PathValue("group")
	if !mbidRe.MatchString(group) {
		http.Error(w, "bad cover id", 400)
		return
	}
	path, err := a.covers.fetchURL(r.Context(),
		filepath.Join(a.covers.dir, "rg-"+group+".jpg"),
		"https://coverartarchive.org/release-group/"+group+"/front-250")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	http.ServeFile(w, r, path)
}

func isDigits(s string) bool {
	if s == "" || len(s) > 20 {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// fetch returns the cached path, downloading it the first time. Concurrent
// requests for the same image wait for the one fetch rather than starting
// thirty-six of their own.
func (c *remoteCovers) fetch(ctx context.Context, release, caa string) (string, error) {
	return c.fetchURL(ctx,
		filepath.Join(c.dir, "mb-"+release+"-"+caa+".jpg"),
		fmt.Sprintf("https://archive.org/download/mbid-%s/mbid-%s-%s_thumb250.jpg",
			release, release, caa))
}

// fetchURL caches one image at `name`. The URL is always built by the caller
// from validated ids — never taken from the request — so this cannot be turned
// into an open proxy.
func (c *remoteCovers) fetchURL(ctx context.Context, name, url string) (string, error) {
	if _, err := os.Stat(name); err == nil {
		return name, nil
	}

	c.mu.Lock()
	wait, running := c.pending[name]
	if running {
		c.mu.Unlock()
		select {
		case <-wait:
		case <-ctx.Done():
			return "", ctx.Err()
		}
		if _, err := os.Stat(name); err != nil {
			return "", err
		}
		return name, nil
	}
	wait = make(chan struct{})
	c.pending[name] = wait
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		delete(c.pending, name)
		c.mu.Unlock()
		close(wait)
	}()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "Plectra/0.9 (https://github.com/plectra/plectra)")

	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("cover art archive answered %d", resp.StatusCode)
	}

	if err := os.MkdirAll(c.dir, 0o755); err != nil {
		return "", err
	}
	// Written under a temporary name and renamed, so a cancelled download can
	// never be served as a truncated image.
	tmp, err := os.CreateTemp(c.dir, "cover-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())

	if _, err := io.Copy(tmp, resp.Body); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(tmp.Name(), name); err != nil {
		return "", err
	}
	return name, nil
}
