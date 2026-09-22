// Command plectra is a self-hosted music player: scan a folder, play it locally,
// control it from a browser on the same machine.
package main

import (
	"context"
	"errors"
	"flag"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/plectra/plectra/internal/api/native"
	"github.com/plectra/plectra/internal/catalog"
	"github.com/plectra/plectra/internal/history"
	"github.com/plectra/plectra/internal/library"
	"github.com/plectra/plectra/internal/player"
	"github.com/plectra/plectra/internal/playlist"
	"github.com/plectra/plectra/internal/store"
	"github.com/plectra/plectra/web"
)

func main() {
	home, _ := os.UserHomeDir()
	dataDir := defaultDataDir()
	music := flag.String("music", filepath.Join(home, "Music"), "music library directory")
	dbPath := flag.String("db", filepath.Join(dataDir, "plectra.db"), "database file")
	coverDir := flag.String("covers", filepath.Join(dataDir, "covers"), "cover art cache directory")
	addr := flag.String("addr", "127.0.0.1:4533", "listen address")
	scanOnly := flag.Bool("scan", false, "scan the library and exit")
	watch := flag.Bool("watch", true, "watch the library directory for changes")
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
	go func() { report(scanner.Scan(ctx)) }() // scanning must not delay startup
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
	historyDone := make(chan struct{})
	go func() {
		history.New(st).Watch(ctx, events)
		close(historyDone)
	}()

	assets, err := fs.Sub(web.FS, ".")
	if err != nil {
		log.Fatal(err)
	}
	api := native.New(catalog.New(st), playlist.New(st), pl, assets)

	srv := &http.Server{Addr: *addr, Handler: api.Handler()}
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
