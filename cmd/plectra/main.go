// Command plectra is a self-hosted music player: scan a folder, play it locally,
// control it from a browser on the same machine.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/plectra/plectra/internal/api/native"
	"github.com/plectra/plectra/internal/api/subsonic"
	"github.com/plectra/plectra/internal/catalog"
	"github.com/plectra/plectra/internal/discovery"
	"github.com/plectra/plectra/internal/history"
	"github.com/plectra/plectra/internal/library"
	"github.com/plectra/plectra/internal/metadata"
	"github.com/plectra/plectra/internal/player"
	"github.com/plectra/plectra/internal/playlist"
	"github.com/plectra/plectra/internal/store"
	"github.com/plectra/plectra/web"
)

func main() {
	// A .env beside the binary is read first, so keys need not be typed on the
	// command line where the process list would show them.
	loadDotEnv(".env")

	home, _ := os.UserHomeDir()
	dataDir := defaultDataDir()
	music := flag.String("music", filepath.Join(home, "Music"), "music library directory")
	dbPath := flag.String("db", filepath.Join(dataDir, "plectra.db"), "database file")
	coverDir := flag.String("covers", filepath.Join(dataDir, "covers"), "cover art cache directory")
	addr := flag.String("addr", "127.0.0.1:4533", "listen address")
	enrich := flag.Bool("enrich", true, "look up metadata from MusicBrainz and Spotify")
	spotifyID := flag.String("spotify-id", os.Getenv("SPOTIFY_CLIENT_ID"), "Spotify client id (optional)")
	spotifySecret := flag.String("spotify-secret", os.Getenv("SPOTIFY_CLIENT_SECRET"), "Spotify client secret (optional)")
	lbToken := flag.String("listenbrainz-token", firstEnv("LISTENBRAINZ_TOKEN", "metabrainz_secret_key"), "ListenBrainz token, to scrobble plays (optional)")
	spotifyRedirect := flag.String("spotify-redirect", "",
		"OAuth redirect URI registered with Spotify (default http://127.0.0.1:<port>/api/spotify/callback)")
	lastfmKey := flag.String("lastfm-key", firstEnv("LASTFM_API_KEY", "last-fm-api-key"), "Last.fm API key, improves recommendations (optional)")
	subUser := flag.String("subsonic-user", "plectra", "username for OpenSubsonic clients")
	subPass := flag.String("subsonic-password", os.Getenv("PLECTRA_PASSWORD"), "password for OpenSubsonic clients; empty disables the API")
	scanOnly := flag.Bool("scan", false, "scan the library and exit")
	scanOnStart := flag.Bool("scan-on-start", false, "scan the library at startup")
	watch := flag.Bool("watch", false, "watch the library directory and import changes as they happen")
	flag.Parse()

	if err := os.MkdirAll(filepath.Dir(*dbPath), 0o755); err != nil {
		log.Fatal(err)
	}
	st, err := store.Open(*dbPath)
	if err != nil {
		log.Fatalf("open database: %v", err)
	}
	defer st.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	scanner := library.New(st, *music, *coverDir)
	if *scanOnly {
		report(scanner.Scan(ctx))
		return
	}
	// Nothing touches the library unless asked: the user scans from Settings,
	// or starts with -scan-on-start, or turns the watcher on.
	if *scanOnStart {
		scanner.StartScan(ctx)
	}
	if *watch {
		go func() {
			if err := scanner.Watch(ctx, nil); err != nil {
				log.Printf("watcher off: %v", err) // a missing watcher is not fatal
			}
		}()
	}

	sink, err := player.NewDeviceSink()
	if err != nil {
		log.Fatalf("open audio device: %v", err)
	}
	defer sink.Close()

	pl := player.New(sink)
	restoreState(ctx, st, pl)

	// History listens to the player; the player does not know it exists.
	events, unsub := pl.Subscribe()
	defer unsub()
	recorder := history.New(st)
	if *lbToken != "" {
		recorder = recorder.WithScrobbler(history.NewListenBrainz(*lbToken))
	}
	historyDone := make(chan struct{})
	go func() {
		recorder.Watch(ctx, events)
		close(historyDone)
	}()

	assets, err := fs.Sub(web.FS, ".")
	if err != nil {
		log.Fatal(err)
	}
	api := native.New(catalog.New(st), playlist.New(st), pl, assets).
		WithHistory(recorder).
		WithLibrary(scanner)

	// Metadata is optional at every level: no network, no credentials, or a dead
	// provider all degrade a feature and never touch playback.
	//
	// Linking a Spotify account is deliberately independent of -enrich: someone
	// who does not want automatic metadata lookups may still want to import
	// their playlists and listening history.
	var (
		providers []metadata.Provider
		link      native.SpotifyLink
		worker    *metadata.Worker
	)
	if *spotifyID != "" && *spotifySecret != "" {
		// Spotify issues 32 hex characters for both. Checking the shape here
		// turns "INVALID_CLIENT" on Spotify's own error page — which says
		// nothing about which value is wrong — into a line in our log.
		warnCredential("-spotify-id", *spotifyID)
		warnCredential("-spotify-secret", *spotifySecret)

		redirect := *spotifyRedirect
		if redirect == "" {
			redirect = defaultRedirect(*addr)
		}
		log.Printf("spotify: register this exact redirect uri in your app: %s", redirect)
		sp := metadata.NewSpotify(st, *spotifyID, *spotifySecret, redirect, metadata.SystemClock)
		providers = append(providers, sp)
		link = sp
	}
	if *enrich {
		providers = append([]metadata.Provider{metadata.NewMusicBrainz(metadata.SystemClock)}, providers...)
		worker = metadata.NewWorker(st, *coverDir, metadata.SystemClock, providers...)
		go func() {
			if _, err := worker.Seed(ctx); err != nil {
				log.Printf("enrich seed: %v", err)
			}
			worker.Run(ctx)
		}()
	}
	// A nil *Worker in an interface is not a nil interface, so pass one only
	// when there is a worker to pass.
	if worker != nil {
		api = api.WithMetadata(worker, link)
	} else if link != nil {
		api = api.WithMetadata(nil, link)
	}

	// Recommendations rank tracks already in the library, using plays recorded
	// here. The similarity providers are optional: ListenBrainz needs no key,
	// Last.fm is used only when one is supplied.
	api = api.WithDiscovery(discovery.New(st,
		discovery.NewListenBrainz(),
		lastfmProvider(*lastfmKey),
	))

	handler := api.Handler()

	// OpenSubsonic is the one surface reachable from other devices, so it stays
	// off until a password is set.
	sub := subsonic.New(catalog.New(st), playlist.New(st), pl, recorder, *subUser, *subPass)
	if sub.Enabled() {
		mux := http.NewServeMux()
		mux.Handle("/rest/", sub.Handler())
		mux.Handle("/", handler)
		handler = mux
		log.Printf("opensubsonic enabled at http://%s/rest as user %q", *addr, *subUser)
	} else {
		log.Printf("opensubsonic disabled: set -subsonic-password to enable it")
	}

	srv := &http.Server{Addr: *addr, Handler: handler}
	go func() {
		<-ctx.Done()
		saveState(st, pl)
		shutdown, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		srv.Shutdown(shutdown)
	}()

	log.Printf("plectra listening on http://%s (library: %s)", *addr, *music)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
	saveState(st, pl)

	// Let the recorder bank the track that was playing before the process exits.
	select {
	case <-historyDone:
	case <-time.After(2 * time.Second):
	}
}

// restoreState brings the queue back after a restart, paused at where it stopped.
func restoreState(ctx context.Context, st *store.Store, pl *player.Player) {
	saved, err := st.LoadPlayerState(ctx)
	if err != nil {
		return // nothing saved yet, or unreadable: start clean
	}
	tracks, err := st.TracksByIDs(ctx, saved.TrackIDs)
	if err != nil || len(tracks) == 0 {
		return
	}
	pl.Restore(tracks, saved.Index, saved.PositionMS, saved.Volume, saved.Shuffle, saved.Repeat)
}

func saveState(st *store.Store, pl *player.Player) {
	s := pl.State()
	ids := make([]int64, len(s.Queue))
	for i, t := range s.Queue {
		ids[i] = t.ID
	}
	err := st.SavePlayerState(context.Background(), store.PlayerState{
		TrackIDs: ids, Index: s.Index, PositionMS: s.PositionMS,
		Volume: s.Volume, Shuffle: s.Shuffle, Repeat: s.Repeat,
	})
	if err != nil {
		log.Printf("save state: %v", err)
	}
}

// warnCredential reports a value that cannot be a Spotify credential. It warns
// rather than refuses: the exact format is Spotify's to change, not ours.
func warnCredential(flag, value string) {
	if len(value) != 32 || strings.Trim(value, "0123456789abcdef") != "" {
		log.Printf("warning: %s does not look like a Spotify credential "+
			"(expected 32 hex characters, got %d chars) — Spotify will answer INVALID_CLIENT",
			flag, len(value))
	}
}

// defaultRedirect derives the OAuth callback from the listen port, always on the
// loopback literal address. Spotify refuses plain http for anything else — and
// refuses the hostname "localhost" too — so deriving it from the bind address
// would break the moment someone listens on 0.0.0.0 to reach the UI from a phone.
func defaultRedirect(addr string) string {
	port := "4533"
	if _, p, err := net.SplitHostPort(addr); err == nil && p != "" {
		port = p
	}
	return "http://127.0.0.1:" + port + "/api/spotify/callback"
}

// lastfmProvider returns a typed nil-free value: a nil *LastFM inside a
// non-nil interface would look configured and fail on every call.
func lastfmProvider(key string) discovery.SimilarProvider {
	if l := discovery.NewLastFM(key); l != nil {
		return l
	}
	return nil
}

// firstEnv returns the first of these environment variables that is set, so a
// .env written with one spelling still works.
func firstEnv(names ...string) string {
	for _, n := range names {
		if v := os.Getenv(n); v != "" {
			return v
		}
	}
	return ""
}

// loadDotEnv reads KEY=value lines into the environment without overwriting
// anything already set. Quietly does nothing when the file is absent.
func loadDotEnv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.Trim(strings.TrimSpace(value), `"'`)
		if key == "" || os.Getenv(key) != "" {
			continue
		}
		os.Setenv(key, value)
	}
}

func report(r library.Result, err error) {
	if err != nil {
		log.Printf("scan: %v", err)
	}
	log.Printf("scan done: %d added/updated, %d unchanged, %d failed", r.Scanned, r.Skipped, r.Failed)
}

func defaultDataDir() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "."
	}
	return filepath.Join(dir, "plectra")
}
