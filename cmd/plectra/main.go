// Command plectra is a self-hosted music player: scan a folder, play it locally,
// control it from a browser on the same machine.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/plectra/plectra/internal/api/native"
	"github.com/plectra/plectra/internal/api/subsonic"
	"github.com/plectra/plectra/internal/browse"
	"github.com/plectra/plectra/internal/catalog"
	"github.com/plectra/plectra/internal/chart"
	"github.com/plectra/plectra/internal/discovery"
	"github.com/plectra/plectra/internal/history"
	"github.com/plectra/plectra/internal/library"
	"github.com/plectra/plectra/internal/metadata"
	"github.com/plectra/plectra/internal/player"
	"github.com/plectra/plectra/internal/playlist"
	"github.com/plectra/plectra/internal/radio"
	"github.com/plectra/plectra/internal/source"
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
	enrich := flag.Bool("enrich", true, "look up metadata from MusicBrainz")
	subUser := flag.String("subsonic-user", "plectra", "username for OpenSubsonic clients")
	subPass := flag.String("subsonic-password", firstEnv("PLECTRA_PASSWORD", "subsonic-password"), "password for OpenSubsonic clients; empty disables the API")
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
	historyDone := make(chan struct{})
	go func() {
		recorder.Watch(ctx, events)
		close(historyDone)
	}()

	assets, err := fs.Sub(web.FS, ".")
	if err != nil {
		log.Fatal(err)
	}
	cat := catalog.New(st)
	api := native.New(cat, playlist.New(st), pl, assets).
		WithHistory(recorder).
		WithLibrary(scanner).
		WithCoverCache(*coverDir)

	// Metadata is optional at every level: no network or a dead provider
	// degrades a feature and never touches playback.
	if *enrich {
		worker := metadata.NewWorker(st, *coverDir, metadata.SystemClock,
			metadata.NewMusicBrainz(metadata.SystemClock))
		api = api.WithMetadata(worker)
		go func() {
			if _, err := worker.Seed(ctx); err != nil {
				log.Printf("enrich seed: %v", err)
			}
			worker.Run(ctx)
		}()
	}

	// Recommendations rank tracks already in the library, using plays recorded
	// here. No external similarity provider ships today.
	// Charts come from ListenBrainz, which needs no key; Last.fm is the
	// fallback and uses one if it is there.
	recs := discovery.New(st).WithCharts(chart.New(firstEnv("LASTFM_API_KEY", "last-fm-api-key")))
	api = api.WithDiscovery(recs)

	// Keep playing when the queue runs down. This lives in the player rather
	// than in the page, so it works with no browser open.
	pl.Suggest = func(ctx context.Context, seed store.Track) []store.Track {
		return recs.NextUp(ctx, pl.State().Queue, 10)
	}

	// External sources are optional and self-declaring: a provider that needs a
	// tool the user has not installed simply does not appear.
	sources := source.NewRegistry(source.NewYTDLP())
	// A queue entry with no file is a track from a catalogue album. Looking it
	// up is what lets next and previous walk an album Plectra does not own.
	pl.Resolve = func(ctx context.Context, t store.Track) (string, error) {
		found, err := sources.Find(ctx, source.Query{Artist: t.Artist, Title: t.Title})
		if err != nil {
			return "", err
		}
		if len(found) == 0 {
			return "", fmt.Errorf("no candidate for %s — %s", t.Artist, t.Title)
		}
		// A track with no file has no length until something measures it. The
		// search result carries one, so the row learns it the first time it
		// plays — which is what makes the seek bar work on it afterwards.
		if t.ID != 0 && t.DurationMS == 0 && found[0].DurationMS > 0 {
			if err := cat.SetDuration(ctx, t.ID, found[0].DurationMS); err != nil {
				log.Printf("record duration for %s: %v", t.Title, err)
			}
		}
		return sources.Resolve(ctx, found[0])
	}
	if names := sources.Names(); len(names) > 0 {
		log.Printf("external audio sources: %v", names)
	}
	api = api.WithRadio(radio.New()).
		WithSources(sources).
		WithBrowser(browse.New()).
		WithFastBrowser(browse.NewDeezer())

	handler := api.Handler()

	// OpenSubsonic is the one surface reachable from other devices, so it stays
	// off until a password is set.
	sub := subsonic.New(cat, playlist.New(st), pl, recorder, *subUser, *subPass)
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
