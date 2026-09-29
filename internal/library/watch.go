package library

import (
	"context"
	"io/fs"
	"log"
	"path/filepath"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
)

// Watch keeps the library in step with the filesystem until ctx is done.
// Editors and copy tools fire many events per file, so changes are debounced
// and then handled in one batch.
func (s *Scanner) Watch(ctx context.Context, onChange func()) error {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer w.Close()

	// fsnotify does not recurse; every existing directory is added explicitly,
	// and new ones are added as they appear.
	addTree(w, s.Root())

	const debounce = 500 * time.Millisecond
	var timer *time.Timer
	var fire <-chan time.Time
	pending := map[string]bool{}

	for {
		select {
		case <-ctx.Done():
			return nil

		case ev, ok := <-w.Events:
			if !ok {
				return nil
			}
			if ev.Has(fsnotify.Create) {
				addTree(w, ev.Name) // no-op for files, needed for new folders
			}
			if !supported[strings.ToLower(filepath.Ext(ev.Name))] {
				continue
			}
			pending[ev.Name] = true
			if timer == nil {
				timer = time.NewTimer(debounce)
				fire = timer.C
			} else {
				timer.Reset(debounce)
			}

		case <-fire:
			timer, fire = nil, nil
			for p := range pending {
				delete(pending, p)
				switch err := s.scanFile(ctx, p); {
				case err == nil || err == errUnchanged:
				default:
					// A file may still be mid-copy; the next event will catch it.
					log.Printf("watch %s: %v", p, err)
				}
			}
			if onChange != nil {
				onChange()
			}

		case err, ok := <-w.Errors:
			if !ok {
				return nil
			}
			log.Printf("watch: %v", err) // watching must never kill the process
		}
	}
}

func addTree(w *fsnotify.Watcher, root string) {
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			w.Add(p)
		}
		return nil
	})
}
