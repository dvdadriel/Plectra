package subsonic

import (
	"context"
	"net/http"
	"strconv"
)

// jukebox drives the speaker attached to the machine Plectra runs on. It is a
// different path from stream: that sends audio to the phone, this makes the
// stereo in the room play.
func (a *API) jukebox(ctx context.Context, r *http.Request) (response, error) {
	action := r.Form.Get("action")
	switch action {
	case "get", "status", "":
	case "start":
		a.pl.Resume()
	case "stop":
		a.pl.Pause()
	case "skip":
		if idx := intParam(r, "index", -1); idx >= 0 {
			st := a.pl.State()
			if idx < len(st.Queue) {
				a.pl.Play(st.Queue, idx)
			}
		} else {
			a.pl.Next()
		}
		if offset := intParam(r, "offset", 0); offset > 0 {
			a.pl.SeekMS(int64(offset) * 1000)
		}
	case "set", "add":
		tracks, err := a.cat.Tracks(ctx, trackIDs(r, "id"))
		if err != nil {
			return response{}, err
		}
		if action == "set" {
			a.pl.Play(tracks, 0)
		} else {
			a.pl.Enqueue(tracks)
		}
	case "clear":
		a.pl.Clear()
	case "remove":
		a.pl.Remove(intParam(r, "index", -1))
	case "shuffle":
		st := a.pl.State()
		a.pl.SetMode(true, st.Repeat)
	case "setGain":
		a.pl.SetVolume(floatParam(r, "gain", 1))
	default:
		return errorResponse(errParameter, "unknown jukebox action "+action), nil
	}

	st := a.pl.State()
	status := jukeboxStatus{
		CurrentIndex: st.Index, Playing: st.Playing,
		Gain: st.Volume, Position: int(st.PositionMS / 1000),
	}

	res := okResponse()
	// Only "get" returns the whole queue; every other action answers with status.
	if action == "get" {
		pl := &jukeboxPlaylist{jukeboxStatus: status}
		starred := a.starredSet(ctx)
		for _, t := range st.Queue {
			pl.Entry = append(pl.Entry, a.toChild(t, starred))
		}
		res.JukeboxPlay = pl
		return res, nil
	}
	res.JukeboxStatus = &status
	return res, nil
}

func floatParam(r *http.Request, name string, def float64) float64 {
	v, err := strconv.ParseFloat(r.Form.Get(name), 64)
	if err != nil {
		return def
	}
	return v
}
