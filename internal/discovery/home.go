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
			HasCover: a.HasCover,
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
		add("Still warm",
			"The albums you finished most recently.", local(al))
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
				add("Out of your own history",
					"Ranked from what you have played, narrowed to albums on your disk.", local(al))
			}
		}
	}

	if al, err := s.st.MostPlayedAlbums(ctx, perRow); err == nil {
		add("Worn thin", "The albums with the most finished plays.", local(al))
	}

	if al, err := s.st.NewestAlbums(ctx, perRow); err == nil {
		add("New to the shelf", "The last albums the scanner found on disk, newest first.", local(al))
	}

	// Capped like every other row. The whole library has its own paged view;
	// pouring ten thousand albums into a side-scroller helps nobody.
	if al, err := s.st.Albums(ctx); err == nil && len(al) > 0 {
		total := len(al)
		if len(al) > perRow {
			al = al[:perRow]
		}
		sec := AlbumSection{
			Title:  "The whole shelf",
			Reason: "Every album on disk, by artist.",
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
			add("Out in the world",
				"Released in the last two weeks and ranked by ListenBrainz listens — not in "+
					"your library, so playing one takes a moment to find a stream.", al)
		}
		if al, err := s.charts.Trending(ctx, perRow); err != nil {
			log.Printf("home: trending: %v", err)
		} else {
			add("Elsewhere this week",
				"What ListenBrainz listeners played most these past seven days — none of "+
					"it is on your disk.", al)
		}
	}

	return out, nil
}
