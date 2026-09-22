package source

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

type fakeProvider struct {
	name      string
	available bool
	found     []Candidate
	resolved  string
	err       error
}

func (f *fakeProvider) Name() string    { return f.name }
func (f *fakeProvider) Available() bool { return f.available }
func (f *fakeProvider) Find(context.Context, Query) ([]Candidate, error) {
	return f.found, f.err
}
func (f *fakeProvider) Resolve(context.Context, Candidate) (string, error) {
	return f.resolved, f.err
}

// A provider whose tool is not installed must never be offered: a candidate
// that cannot play is worse than no candidate.
func TestUnavailableProvidersAreDropped(t *testing.T) {
	reg := NewRegistry(
		&fakeProvider{name: "present", available: true},
		&fakeProvider{name: "absent", available: false},
		nil,
	)
	if names := reg.Names(); len(names) != 1 || names[0] != "present" {
		t.Fatalf("names = %v, want only the available one", names)
	}
}

func TestRankingPrefersTheRightLengthAndTitle(t *testing.T) {
	q := Query{Artist: "Sigur Rós", Title: "Hoppípolla", DurationMS: 268000}
	reg := NewRegistry(&fakeProvider{name: "p", available: true, found: []Candidate{
		// Identical title and artist to the real track once the parenthetical
		// is stripped, so length is the only thing that can tell them apart.
		{ID: "mix", Provider: "p", Title: "Hoppipolla (Full Album Mix)", Artist: "Sigur Ros", DurationMS: 3600000},
		{ID: "right", Provider: "p", Title: "Hoppipolla", Artist: "Sigur Ros", DurationMS: 268000},
		{ID: "live", Provider: "p", Title: "Hoppipolla (Live in Reykjavik)", Artist: "Sigur Ros", DurationMS: 310000},
	}})

	got, err := reg.Find(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	if got[0].ID != "right" {
		t.Fatalf("best candidate = %q (score %d), want the studio track", got[0].ID, got[0].Score)
	}
	// Title and artist match on all three, so only length separates the
	// hour-long mix from the live version. Asserting the scores directly is
	// what guards the length penalty; asserting the order alone does not,
	// because a tie keeps the original order.
	byID := map[string]int{}
	for _, c := range got {
		byID[c.ID] = c.Score
	}
	// Without the length penalty the mix would score the same as the real
	// track, because everything else about it matches.
	if byID["mix"] >= byID["right"]-25 {
		t.Fatalf("mix scored %d against the real track's %d — an hour-long upload is not "+
			"being penalised for its length", byID["mix"], byID["right"])
	}
}

func TestResolveGoesToTheProviderThatFoundIt(t *testing.T) {
	a := &fakeProvider{name: "a", available: true, resolved: "from-a"}
	b := &fakeProvider{name: "b", available: true, resolved: "from-b"}
	reg := NewRegistry(a, b)

	got, err := reg.Resolve(context.Background(), Candidate{ID: "x", Provider: "b"})
	if err != nil || got != "from-b" {
		t.Fatalf("resolved %q, err %v", got, err)
	}
	if _, err := reg.Resolve(context.Background(), Candidate{ID: "x", Provider: "gone"}); err == nil {
		t.Fatal("resolving through a missing provider succeeded")
	}
}

// yt-dlp is driven as a subprocess, so the parsing is tested against a stub
// binary rather than the real network.
func TestYTDLPParsesSearchOutput(t *testing.T) {
	dir := t.TempDir()
	stub := filepath.Join(dir, "yt-dlp")
	script := `#!/bin/sh
if [ "$1" = "--version" ]; then echo stub; exit 0; fi
for arg in "$@"; do
  case "$arg" in
    -g) echo "https://example.invalid/audio.webm"; exit 0;;
  esac
done
echo '{"id":"abc123","title":"Some Song (Official Video)","uploader":"A Channel","duration":211.0}'
echo '{"id":"def456","title":"Some Song (Live)","channel":"Fan Uploads","duration":260.0}'
`
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	y := NewYTDLP()
	y.Binary = stub

	found, err := y.Find(context.Background(), Query{Title: "Some Song"})
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 2 {
		t.Fatalf("parsed %d candidates, want 2", len(found))
	}
	if found[0].ID != "abc123" || found[0].Artist != "A Channel" || found[0].DurationMS != 211000 {
		t.Fatalf("first candidate = %+v", found[0])
	}
	// The second entry uses "channel" instead of "uploader"; both are names.
	if found[1].Artist != "Fan Uploads" {
		t.Fatalf("second artist = %q", found[1].Artist)
	}

	loc, err := y.Resolve(context.Background(), found[0])
	if err != nil {
		t.Fatal(err)
	}
	// The formats yt-dlp returns need ffmpeg, so the location must say so.
	if want := "ffmpeg:https://example.invalid/audio.webm"; loc != want {
		t.Fatalf("resolved %q, want %q", loc, want)
	}
}

func TestYTDLPIsUnavailableWithoutTheBinary(t *testing.T) {
	y := NewYTDLP()
	y.Binary = filepath.Join(t.TempDir(), "definitely-not-installed")
	if y.Available() {
		t.Fatal("reported available with no binary present")
	}
}
