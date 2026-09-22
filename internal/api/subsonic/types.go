package subsonic

import "encoding/xml"

// response is the single envelope every Subsonic reply travels in. The struct
// carries both xml and json tags because clients ask for either.
type response struct {
	XMLName       xml.Name `xml:"subsonic-response" json:"-"`
	Xmlns         string   `xml:"xmlns,attr" json:"-"`
	Status        string   `xml:"status,attr" json:"status"`
	Version       string   `xml:"version,attr" json:"version"`
	Type          string   `xml:"type,attr" json:"type"`
	ServerVersion string   `xml:"serverVersion,attr" json:"serverVersion"`
	OpenSubsonic  bool     `xml:"openSubsonic,attr" json:"openSubsonic"`

	Error         *apiError          `xml:"error,omitempty" json:"error,omitempty"`
	License       *license           `xml:"license,omitempty" json:"license,omitempty"`
	MusicFolders  *musicFolders      `xml:"musicFolders,omitempty" json:"musicFolders,omitempty"`
	Indexes       *indexList         `xml:"indexes,omitempty" json:"indexes,omitempty"`
	Artists       *indexList         `xml:"artists,omitempty" json:"artists,omitempty"`
	Artist        *artistWithAlbums  `xml:"artist,omitempty" json:"artist,omitempty"`
	Album         *albumWithSongs    `xml:"album,omitempty" json:"album,omitempty"`
	AlbumList     *albumList         `xml:"albumList,omitempty" json:"albumList,omitempty"`
	AlbumList2    *albumList         `xml:"albumList2,omitempty" json:"albumList2,omitempty"`
	Song          *child             `xml:"song,omitempty" json:"song,omitempty"`
	SearchResult2 *searchResult      `xml:"searchResult2,omitempty" json:"searchResult2,omitempty"`
	SearchResult3 *searchResult      `xml:"searchResult3,omitempty" json:"searchResult3,omitempty"`
	Playlists     *playlistList      `xml:"playlists,omitempty" json:"playlists,omitempty"`
	Playlist      *playlistWithSongs `xml:"playlist,omitempty" json:"playlist,omitempty"`
	Starred       *starredResult     `xml:"starred,omitempty" json:"starred,omitempty"`
	Starred2      *starredResult     `xml:"starred2,omitempty" json:"starred2,omitempty"`
	JukeboxStatus *jukeboxStatus     `xml:"jukeboxStatus,omitempty" json:"jukeboxStatus,omitempty"`
	JukeboxPlay   *jukeboxPlaylist   `xml:"jukeboxPlaylist,omitempty" json:"jukeboxPlaylist,omitempty"`
}

type apiError struct {
	Code    int    `xml:"code,attr" json:"code"`
	Message string `xml:"message,attr" json:"message"`
}

type license struct {
	Valid bool `xml:"valid,attr" json:"valid"`
}

type musicFolders struct {
	Folder []musicFolder `xml:"musicFolder" json:"musicFolder"`
}

type musicFolder struct {
	ID   int    `xml:"id,attr" json:"id"`
	Name string `xml:"name,attr" json:"name"`
}

// indexList serves both getIndexes and getArtists: same shape, different tag.
type indexList struct {
	LastModified    int64   `xml:"lastModified,attr" json:"lastModified"`
	IgnoredArticles string  `xml:"ignoredArticles,attr" json:"ignoredArticles"`
	Index           []index `xml:"index" json:"index"`
}

type index struct {
	Name   string   `xml:"name,attr" json:"name"`
	Artist []artist `xml:"artist" json:"artist"`
}

type artist struct {
	ID         string `xml:"id,attr" json:"id"`
	Name       string `xml:"name,attr" json:"name"`
	AlbumCount int    `xml:"albumCount,attr" json:"albumCount"`
	CoverArt   string `xml:"coverArt,attr,omitempty" json:"coverArt,omitempty"`
}

type artistWithAlbums struct {
	artist
	Album []albumID3 `xml:"album" json:"album"`
}

type albumID3 struct {
	ID        string `xml:"id,attr" json:"id"`
	Name      string `xml:"name,attr" json:"name"`
	Artist    string `xml:"artist,attr" json:"artist"`
	ArtistID  string `xml:"artistId,attr,omitempty" json:"artistId,omitempty"`
	CoverArt  string `xml:"coverArt,attr,omitempty" json:"coverArt,omitempty"`
	SongCount int    `xml:"songCount,attr" json:"songCount"`
	Duration  int    `xml:"duration,attr" json:"duration"`
	Year      int    `xml:"year,attr,omitempty" json:"year,omitempty"`
}

type albumWithSongs struct {
	albumID3
	Song []child `xml:"song" json:"song"`
}

type albumList struct {
	Album []albumID3 `xml:"album" json:"album"`
}

// child is Subsonic's song record.
type child struct {
	ID          string `xml:"id,attr" json:"id"`
	Parent      string `xml:"parent,attr,omitempty" json:"parent,omitempty"`
	IsDir       bool   `xml:"isDir,attr" json:"isDir"`
	Title       string `xml:"title,attr" json:"title"`
	Album       string `xml:"album,attr,omitempty" json:"album,omitempty"`
	Artist      string `xml:"artist,attr,omitempty" json:"artist,omitempty"`
	Track       int    `xml:"track,attr,omitempty" json:"track,omitempty"`
	DiscNumber  int    `xml:"discNumber,attr,omitempty" json:"discNumber,omitempty"`
	Year        int    `xml:"year,attr,omitempty" json:"year,omitempty"`
	CoverArt    string `xml:"coverArt,attr,omitempty" json:"coverArt,omitempty"`
	Size        int64  `xml:"size,attr,omitempty" json:"size,omitempty"`
	ContentType string `xml:"contentType,attr,omitempty" json:"contentType,omitempty"`
	Suffix      string `xml:"suffix,attr,omitempty" json:"suffix,omitempty"`
	Duration    int    `xml:"duration,attr,omitempty" json:"duration,omitempty"`
	BitRate     int    `xml:"bitRate,attr,omitempty" json:"bitRate,omitempty"`
	Path        string `xml:"path,attr,omitempty" json:"path,omitempty"`
	AlbumID     string `xml:"albumId,attr,omitempty" json:"albumId,omitempty"`
	ArtistID    string `xml:"artistId,attr,omitempty" json:"artistId,omitempty"`
	Type        string `xml:"type,attr,omitempty" json:"type,omitempty"`
	Starred     string `xml:"starred,attr,omitempty" json:"starred,omitempty"`
}

type searchResult struct {
	Artist []artist   `xml:"artist" json:"artist"`
	Album  []albumID3 `xml:"album" json:"album"`
	Song   []child    `xml:"song" json:"song"`
}

type playlistList struct {
	Playlist []playlistEntry `xml:"playlist" json:"playlist"`
}

type playlistEntry struct {
	ID        string `xml:"id,attr" json:"id"`
	Name      string `xml:"name,attr" json:"name"`
	Comment   string `xml:"comment,attr,omitempty" json:"comment,omitempty"`
	Owner     string `xml:"owner,attr,omitempty" json:"owner,omitempty"`
	Public    bool   `xml:"public,attr" json:"public"`
	SongCount int    `xml:"songCount,attr" json:"songCount"`
	Duration  int    `xml:"duration,attr" json:"duration"`
	Created   string `xml:"created,attr,omitempty" json:"created,omitempty"`
	Changed   string `xml:"changed,attr,omitempty" json:"changed,omitempty"`
}

type playlistWithSongs struct {
	playlistEntry
	Entry []child `xml:"entry" json:"entry"`
}

type starredResult struct {
	Artist []artist   `xml:"artist" json:"artist"`
	Album  []albumID3 `xml:"album" json:"album"`
	Song   []child    `xml:"song" json:"song"`
}

// jukeboxStatus describes the speaker attached to the Plectra machine, which is
// a different thing from streaming audio to the client's own device.
type jukeboxStatus struct {
	CurrentIndex int     `xml:"currentIndex,attr" json:"currentIndex"`
	Playing      bool    `xml:"playing,attr" json:"playing"`
	Gain         float64 `xml:"gain,attr" json:"gain"`
	Position     int     `xml:"position,attr" json:"position"`
}

type jukeboxPlaylist struct {
	jukeboxStatus
	Entry []child `xml:"entry" json:"entry"`
}
