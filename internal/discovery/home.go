package discovery

import (
	"context"
	"log"

	"github.com/plectra/plectra/internal/chart"
	"github.com/plectra/plectra/internal/store"
)

// AlbumSection is one band of the home view. Albums rather than tracks: a wall
// of sleeves is how people recognise music they know.
type AlbumSection struct {
	Title  string        `json:"title"`
	Reason string        `json:"reason"`
	Albums []chart.Album `json:"albums"`
	// Total and More are set when the row is a window onto something larger,
	// so the heading can offer the full, paged list instead of pretending the
	// row is all there is.
	Total int    `json:"total,omitempty"`
	More  string `json:"more,omitempty"`
}

// Charts is the slice of the outside world the home view uses. Optional: with
// no chart client the local sections still stand on their own.
type Charts interface {
	Trending(ctx context.Context, limit int) ([]chart.Album, error)
	New(ctx context.Context, limit int) ([]chart.Album, error)
}

// WithCharts attaches the world's charts to the home view.
func (s *Service) WithCharts(c Charts) *Service {
	s.charts = c
	return s
}

const perRow = 18

// local turns library albums into the shape the view renders, which carries
// both kinds: a local album has an id, a chart album has an MBID.
func local(albums []store.Album) []chart.Album {
	out := make([]chart.Album, 0, len(albums))
	for _, a := range albums {
		out = append(out, chart.Album{
			LocalID: a.ID, Title: a.Title, Artist: a.Artist, Year: a.Year,
		})
	}
	return out
}

// Home builds the album view. Every section states why it is there, and a
// section with nothing in it is left out rather than shown empty — an empty
// "recommended for you" is worse than no recommendation.
func (s *Service) Home(ctx context.Context) ([]AlbumSection, error) {
	var out []AlbumSection

	add := func(title, reason string, albums []chart.Album) {
		if len(albums) > 0 {
			out = append(out, AlbumSection{Title: title, Reason: reason, Albums: albums})
		}
	}

	if al, err := s.st.RecentAlbums(ctx, perRow); err == nil {
		add("Pick up where you left off",
			"The albums you listened to most recently.", local(al))
	}

	// Recommendations are built from tracks, then collapsed to their albums.
	//
	// Gated on there being any listening at all: Sections() also returns a
	// "never played" band, which exists for a silent library too. Without this
	// check a brand-new library gets a row headed "Because of what you play"
	// when nothing has been played — a label that is simply untrue.
	heard, err := s.st.TopArtists(ctx, 1)
	if err == nil && len(heard) > 0 {
		if secs, err := s.Sections(ctx); err == nil {
			var tracks []store.Track
			for _, sec := range secs {
				tracks = append(tracks, sec.Tracks...)
			}
			if al, err := s.st.AlbumsOfTracks(ctx, tracks, perRow); err == nil {
				add("Because of what you play",
					"Ranked from your own listening, filtered to albums you own.", local(al))
			}
		}
	}

	if al, err := s.st.MostPlayedAlbums(ctx, perRow); err == nil {
		add("Played the most", "Your own top albums, by completed plays.", local(al))
	}

	if al, err := s.st.NewestAlbums(ctx, perRow); err == nil {
		add("Recently added", "The last albums the scanner found on disk.", local(al))
	}

	// Capped like every other row. The whole library has its own paged view;
	// pouring ten thousand albums into a side-scroller helps nobody.
	if al, err := s.st.Albums(ctx); err == nil && len(al) > 0 {
		total := len(al)
		if len(al) > perRow {
			al = al[:perRow]
		}
		sec := AlbumSection{
			Title:  "Your library",
			Reason: "Everything on disk.",
			Albums: local(al),
			Total:  total,
			More:   "albums",
		}
		out = append(out, sec)
	}

	// The world's charts are someone else's server: a failure here costs a
	// section, never the page.
	if s.charts != nil {
		if al, err := s.charts.New(ctx, perRow); err != nil {
			log.Printf("home: new releases: %v", err)
		} else {
			add("New releases", "Out in the last two weeks, by how much they are being played.", al)
		}
		if al, err := s.charts.Trending(ctx, perRow); err != nil {
			log.Printf("home: trending: %v", err)
		} else {
			add("Trending this week", "What ListenBrainz listeners played most this week.", al)
		}
	}

	return out, nil
}
