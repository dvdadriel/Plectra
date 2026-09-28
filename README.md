# Plectra

A self-hosted music player in Go. It scans a folder of music, plays it on the
machine it runs on, and gives you a browser to drive it from — your own files,
no account, no ads, no catalogue trying to sell you something.

Audio comes out of the **server's** sound card. The web page at
`127.0.0.1:4533` is a remote control, not a stream player. Phones and other
clients connect over OpenSubsonic.

## Install

Needs Go 1.27 and a C toolchain (the audio device binding uses cgo).

```sh
go build -o plectra ./cmd/plectra
./plectra -music ~/Music -scan-on-start
```

Open http://127.0.0.1:4533.

The database and cover cache live in your user config directory
(`~/Library/Application Support/plectra` on macOS, `~/.config/plectra` on
Linux). Everything is embedded in the binary — no asset directory to ship.

### Optional extras

| Tool | What it unlocks |
|---|---|
| `ffmpeg` | Internet radio and any format the Go decoders cannot read |
| `yt-dlp` | Playing tracks found in catalogue search that are not in your library |

Both are looked up on `PATH` at startup. A missing tool removes a feature; it
never breaks playback of your own files.

## Flags

```
-music    <dir>   library directory            (default ~/Music)
-addr     <host>  listen address               (default 127.0.0.1:4533)
-db       <file>  database file
-covers   <dir>   cover art cache
-scan             scan and exit
-scan-on-start    scan at startup
-watch            import filesystem changes as they happen
-enrich           look up metadata from MusicBrainz (default true)
-subsonic-user     <name>
-subsonic-password <pass>   empty disables the OpenSubsonic API
```

Nothing touches the library unless asked: scan from Settings, or pass
`-scan-on-start`, or turn the watcher on.

### Environment

A `.env` beside the binary is read at startup, so keys stay out of the process
list.

```
PLECTRA_PASSWORD=...   # same as -subsonic-password
LASTFM_API_KEY=...     # optional; charts fall back to ListenBrainz without it
```

## What it does

- **Library** — scans MP3, FLAC, OGG and WAV, reads tags and embedded art, and
  keeps a SQLite index. Optional filesystem watcher.
- **Playback** — gapless queue, shuffle, repeat, seek, volume. The queue and
  position survive a restart.
- **Metadata** — a background worker fills gaps from MusicBrainz and caches
  cover art. Entirely optional; the network is never on the playback path.
- **Search the wider catalogue** — Deezer for search-as-you-type, MusicBrainz
  for full track listings. Neither provides audio; playing a result goes
  through `yt-dlp`.
- **Radio** — stations from radio-browser.info, decoded by ffmpeg.
- **Discovery** — suggestions ranked from *your* recorded plays, plus charts
  from ListenBrainz. Rows say which is which; nothing is dressed up as
  "made for you".
- **History** — every play recorded locally, feeding the recommendations.
- **OpenSubsonic** — off until you set a password. Then any Subsonic client can
  browse, play and scrobble against it.

## Layout

```
cmd/plectra        entry point and wiring
internal/audio     decoders and the ffmpeg bridge
internal/player    engine, queue, device sink
internal/library   scanner, cover extraction, watcher
internal/store     SQLite
internal/api       native JSON API + OpenSubsonic
internal/browse    catalogue search (MusicBrainz, Deezer)
internal/discovery recommendations
internal/source    external audio providers (yt-dlp)
web                embedded interface
docs               DESIGN.md (design system), PRODUCT.md (scope)
```

## Troubleshooting

**`403 Forbidden` when a catalogue track starts.** YouTube changed something
and your `yt-dlp` predates the fix. Update it — this is the first thing to try,
before assuming Plectra is at fault.

```sh
pip install -U yt-dlp    # or: brew upgrade yt-dlp
```

**No audio sources listed at startup.** Both `yt-dlp` and `ffmpeg` must be on
`PATH`; a provider that cannot decode what it finds does not offer itself.

**OpenSubsonic disabled.** Set `-subsonic-password` or `PLECTRA_PASSWORD`.
