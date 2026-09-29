package library

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"
)

// Status is what the UI needs to know about scanning: whether one is running,
// and what the last one did.
type Status struct {
	Running    bool   `json:"running"`
	StartedAt  int64  `json:"startedAt,omitempty"`
	FinishedAt int64  `json:"finishedAt,omitempty"`
	Scanned    int    `json:"scanned"`
	Skipped    int    `json:"skipped"`
	Failed     int    `json:"failed"`
	Error      string `json:"error,omitempty"`
	Root       string `json:"root"`
}

// StartScan runs a scan in the background and reports whether it started. A scan
// already in progress is left alone rather than queued: two walks of the same
// tree would only fight over the same rows.
func (s *Scanner) StartScan(ctx context.Context) bool {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return false
	}
	s.running = true
	s.status = Status{Running: true, StartedAt: time.Now().Unix(), Root: s.root}
	s.mu.Unlock()

	go func() {
		res, err := s.Scan(ctx)
		s.mu.Lock()
		defer s.mu.Unlock()
		s.running = false
		s.status = Status{
			StartedAt:  s.status.StartedAt,
			FinishedAt: time.Now().Unix(),
			Scanned:    res.Scanned,
			Skipped:    res.Skipped,
			Failed:     res.Failed,
			Root:       s.root,
		}
		if err != nil {
			s.status.Error = err.Error()
		}
	}()
	return true
}

// Root reports the directory being scanned.
func (s *Scanner) Root() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.root
}

// SetRoot points the scanner at another directory, so the library can be found
// from the interface rather than only from a command line nobody sees.
//
// It refuses while a scan is running: the walk in flight is over the old tree,
// and finishing it under a new name would file those tracks against the wrong
// root. It refuses a path that is not a directory for the same reason a typo
// should not quietly empty the library.
//
// ponytail: a running -watch still watches the old tree until restart. Moving
// the watcher means tearing one down mid-event; wire it if anyone changes
// their library directory often enough to notice.
func (s *Scanner) SetRoot(root string) error {
	fi, err := os.Stat(root)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("%s is not a directory", root)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running {
		return errors.New("a scan is running; try again when it finishes")
	}
	s.root, s.status.Root = root, root
	return nil
}

// Status reports the current or most recent scan.
func (s *Scanner) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status
}

// guard is embedded in Scanner; kept here so the scanning logic stays in scan.go.
type guard struct {
	mu      sync.Mutex
	running bool
	status  Status
}
