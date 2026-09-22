package subsonic

import (
	"context"
	"net/http"
	"time"

	"github.com/plectra/plectra/internal/store"
)

func toPlaylistEntry(p store.Playlist) playlistEntry {
	return playlistEntry{
		ID: idString(p.ID), Name: p.Name, Comment: p.Description,
		Owner: serverName, Public: false, SongCount: p.TrackCount,
		Created: time.Unix(p.CreatedAt, 0).UTC().Format(time.RFC3339),
		Changed: time.Unix(p.UpdatedAt, 0).UTC().Format(time.RFC3339),
	}
}

func idString(id int64) string { return albumID(id)[3:] } // plain number, no prefix

func (a *API) getPlaylists(ctx context.Context) (response, error) {
	lists, err := a.lists.List(ctx)
	if err != nil {
		return response{}, err
	}
	out := &playlistList{Playlist: []playlistEntry{}}
	for _, p := range lists {
		out.Playlist = append(out.Playlist, toPlaylistEntry(p))
	}
	res := okResponse()
	res.Playlists = out
	return res, nil
}

func (a *API) getPlaylist(ctx context.Context, r *http.Request) (response, error) {
	id, ok := parseID(r.Form.Get("id"), "")
	if !ok {
		return errorResponse(errParameter, "id is required"), nil
	}
	lists, err := a.lists.List(ctx)
	if err != nil {
		return response{}, err
	}
	var found *store.Playlist
	for i := range lists {
		if lists[i].ID == id {
			found = &lists[i]
		}
	}
	if found == nil {
		return response{}, store.ErrNotFound
	}
	tracks, err := a.lists.Tracks(ctx, id)
	if err != nil {
		return response{}, err
	}
	starred := a.starredSet(ctx)

	out := &playlistWithSongs{playlistEntry: toPlaylistEntry(*found)}
	var duration int64
	for _, t := range tracks {
		out.Entry = append(out.Entry, a.toChild(t, starred))
		duration += t.DurationMS
	}
	out.Duration = int(duration / 1000)

	res := okResponse()
	res.Playlist = out
	return res, nil
}

func (a *API) createPlaylist(ctx context.Context, r *http.Request) (response, error) {
	name := r.Form.Get("name")
	songIDs := trackIDs(r, "songId")

	// With a playlistId the call is a rewrite of an existing list, which is how
	// clients "save" a reordered queue.
	if idRaw := r.Form.Get("playlistId"); idRaw != "" {
		id, ok := parseID(idRaw, "")
		if !ok {
			return errorResponse(errParameter, "bad playlistId"), nil
		}
		if err := a.lists.Reorder(ctx, id, songIDs); err != nil {
			return response{}, err
		}
		if name != "" {
			if err := a.lists.Update(ctx, id, name, ""); err != nil {
				return response{}, err
			}
		}
		return a.getPlaylistByID(ctx, id)
	}

	if name == "" {
		return errorResponse(errParameter, "name is required"), nil
	}
	created, err := a.lists.Create(ctx, name, "")
	if err != nil {
		return response{}, err
	}
	if len(songIDs) > 0 {
		if err := a.lists.Add(ctx, created.ID, songIDs); err != nil {
			return response{}, err
		}
	}
	return a.getPlaylistByID(ctx, created.ID)
}

func (a *API) updatePlaylist(ctx context.Context, r *http.Request) (response, error) {
	id, ok := parseID(r.Form.Get("playlistId"), "")
	if !ok {
		return errorResponse(errParameter, "playlistId is required"), nil
	}
	if name := r.Form.Get("name"); name != "" {
		if err := a.lists.Update(ctx, id, name, r.Form.Get("comment")); err != nil {
			return response{}, err
		}
	}
	if add := trackIDs(r, "songIdToAdd"); len(add) > 0 {
		if err := a.lists.Add(ctx, id, add); err != nil {
			return response{}, err
		}
	}
	// Removals are given as positions in the current list, not track ids.
	if idx := r.Form["songIndexToRemove"]; len(idx) > 0 {
		tracks, err := a.lists.Tracks(ctx, id)
		if err != nil {
			return response{}, err
		}
		for _, raw := range idx {
			pos, ok := parseID(raw, "")
			if !ok && raw != "0" {
				continue
			}
			if raw == "0" {
				pos = 0
			}
			if int(pos) >= 0 && int(pos) < len(tracks) {
				if err := a.lists.Remove(ctx, id, tracks[pos].ID); err != nil {
					return response{}, err
				}
			}
		}
	}
	return okResponse(), nil
}

func (a *API) deletePlaylist(ctx context.Context, r *http.Request) (response, error) {
	id, ok := parseID(r.Form.Get("id"), "")
	if !ok {
		return errorResponse(errParameter, "id is required"), nil
	}
	if err := a.lists.Delete(ctx, id); err != nil {
		return response{}, err
	}
	return okResponse(), nil
}

func (a *API) getPlaylistByID(ctx context.Context, id int64) (response, error) {
	r := &http.Request{Form: map[string][]string{"id": {idString(id)}}}
	return a.getPlaylist(ctx, r)
}

// star and unstar map onto likes, which is the only starring Plectra has.
func (a *API) star(ctx context.Context, r *http.Request, on bool) (response, error) {
	ids := trackIDs(r, "id")
	ids = append(ids, trackIDs(r, "songId")...)
	if len(ids) == 0 {
		return errorResponse(errParameter, "only songs can be starred"), nil
	}
	// Starring a track that does not exist is a client error, not a database
	// constraint failure leaking out as error code 0.
	found, err := a.cat.Tracks(ctx, ids)
	if err != nil {
		return response{}, err
	}
	if len(found) != len(ids) {
		return response{}, store.ErrNotFound
	}
	for _, id := range ids {
		if on {
			err = a.lists.Like(ctx, id)
		} else {
			err = a.lists.Unlike(ctx, id)
		}
		if err != nil {
			return response{}, err
		}
	}
	return okResponse(), nil
}

func (a *API) getStarred(ctx context.Context, method string) (response, error) {
	tracks, err := a.lists.Liked(ctx)
	if err != nil {
		return response{}, err
	}
	starred := a.starredSet(ctx)
	out := &starredResult{}
	for _, t := range tracks {
		out.Song = append(out.Song, a.toChild(t, starred))
	}
	res := okResponse()
	if method == "getStarred" {
		res.Starred = out
	} else {
		res.Starred2 = out
	}
	return res, nil
}

// trackIDs reads a repeated parameter of song ids, ignoring anything that is
// not a track id — a client must not be able to star an album by accident.
func trackIDs(r *http.Request, name string) []int64 {
	var out []int64
	for _, raw := range r.Form[name] {
		if id, ok := parseID(raw, "tr-"); ok {
			out = append(out, id)
		}
	}
	return out
}
