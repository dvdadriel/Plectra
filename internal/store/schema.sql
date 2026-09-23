PRAGMA journal_mode = WAL;
PRAGMA foreign_keys = ON;
PRAGMA busy_timeout = 5000;

CREATE TABLE IF NOT EXISTS artists (
    id         INTEGER PRIMARY KEY,
    name       TEXT NOT NULL UNIQUE,
    sort_name  TEXT NOT NULL DEFAULT '',
    mbid       TEXT,
    spotify_id TEXT,
    image_path TEXT
);

CREATE TABLE IF NOT EXISTS albums (
    id         INTEGER PRIMARY KEY,
    title      TEXT NOT NULL,
    artist_id  INTEGER NOT NULL REFERENCES artists(id),
    year       INTEGER,
    mbid       TEXT,
    spotify_id TEXT,
    cover_path TEXT,
    disc_count INTEGER NOT NULL DEFAULT 1,
    UNIQUE (title, artist_id)
);

CREATE TABLE IF NOT EXISTS tracks (
    id               INTEGER PRIMARY KEY,
    album_id         INTEGER NOT NULL REFERENCES albums(id),
    artist_id        INTEGER NOT NULL REFERENCES artists(id),
    title            TEXT NOT NULL,
    track_no         INTEGER NOT NULL DEFAULT 0,
    disc_no          INTEGER NOT NULL DEFAULT 1,
    duration_ms      INTEGER NOT NULL DEFAULT 0,
    format           TEXT NOT NULL DEFAULT '',
    bitrate          INTEGER NOT NULL DEFAULT 0,
    sample_rate      INTEGER NOT NULL DEFAULT 0,
    channels         INTEGER NOT NULL DEFAULT 0,
    path             TEXT NOT NULL,
    file_hash        TEXT NOT NULL UNIQUE,
    mtime            INTEGER NOT NULL DEFAULT 0,
    replaygain_track REAL,
    replaygain_album REAL,
    mbid             TEXT,
    spotify_id       TEXT,
    added_at         INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS tracks_album ON tracks(album_id, disc_no, track_no);
CREATE INDEX IF NOT EXISTS tracks_path  ON tracks(path);

CREATE TABLE IF NOT EXISTS playlists (
    id          INTEGER PRIMARY KEY,
    name        TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    created_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL,
    is_smart    INTEGER NOT NULL DEFAULT 0,
    rules_json  TEXT
);

CREATE TABLE IF NOT EXISTS playlist_items (
    playlist_id INTEGER NOT NULL REFERENCES playlists(id) ON DELETE CASCADE,
    track_id    INTEGER NOT NULL REFERENCES tracks(id) ON DELETE CASCADE,
    position    INTEGER NOT NULL,
    PRIMARY KEY (playlist_id, track_id)
);
CREATE INDEX IF NOT EXISTS playlist_items_order ON playlist_items(playlist_id, position);

CREATE TABLE IF NOT EXISTS likes (
    track_id INTEGER PRIMARY KEY REFERENCES tracks(id) ON DELETE CASCADE,
    liked_at INTEGER NOT NULL
);

-- plays stores events, never a counter: play count is COUNT(*), and that is the
-- only shape that can still answer time-based questions later.
CREATE TABLE IF NOT EXISTS plays (
    id               INTEGER PRIMARY KEY,
    track_id         INTEGER REFERENCES tracks(id) ON DELETE SET NULL,
    played_at        INTEGER NOT NULL,
    ms_played        INTEGER NOT NULL DEFAULT 0,
    completed        INTEGER NOT NULL DEFAULT 0,
    source           TEXT NOT NULL,
    raw_artist       TEXT,
    raw_album        TEXT,
    raw_title        TEXT
);
CREATE INDEX IF NOT EXISTS plays_track ON plays(track_id, played_at);
CREATE INDEX IF NOT EXISTS plays_time  ON plays(played_at);

-- One row, holding what has to survive a restart.
CREATE TABLE IF NOT EXISTS player_state (
    id         INTEGER PRIMARY KEY CHECK (id = 1),
    queue_json TEXT NOT NULL,
    idx        INTEGER NOT NULL,
    position_ms INTEGER NOT NULL,
    volume     REAL NOT NULL,
    shuffle    INTEGER NOT NULL,
    repeat     TEXT NOT NULL
);

CREATE VIRTUAL TABLE IF NOT EXISTS tracks_fts USING fts5(title, artist, album);

-- Triggers keep the index in step with tracks. A plain (non-contentless) FTS5
-- table is used so a delete is an ordinary DELETE by rowid.
CREATE TRIGGER IF NOT EXISTS tracks_fts_ins AFTER INSERT ON tracks BEGIN
    INSERT INTO tracks_fts (rowid, title, artist, album)
    VALUES (new.id, new.title,
            (SELECT name FROM artists WHERE id = new.artist_id),
            (SELECT title FROM albums  WHERE id = new.album_id));
END;

CREATE TRIGGER IF NOT EXISTS tracks_fts_del AFTER DELETE ON tracks BEGIN
    DELETE FROM tracks_fts WHERE rowid = old.id;
END;

CREATE TRIGGER IF NOT EXISTS tracks_fts_upd AFTER UPDATE ON tracks BEGIN
    DELETE FROM tracks_fts WHERE rowid = old.id;
    INSERT INTO tracks_fts (rowid, title, artist, album)
    VALUES (new.id, new.title,
            (SELECT name FROM artists WHERE id = new.artist_id),
            (SELECT title FROM albums  WHERE id = new.album_id));
END;

-- enrich_jobs is a table, not an in-memory queue: a Plectra that dies mid-run
-- resumes where it stopped.
CREATE TABLE IF NOT EXISTS enrich_jobs (
    id          INTEGER PRIMARY KEY,
    entity_type TEXT NOT NULL,              -- 'album' | 'artist' | 'track'
    entity_id   INTEGER NOT NULL,
    provider    TEXT NOT NULL,              -- 'musicbrainz' | 'spotify'
    state       TEXT NOT NULL DEFAULT 'pending', -- pending | done | failed
    attempts    INTEGER NOT NULL DEFAULT 0,
    last_error  TEXT,
    next_run_at INTEGER NOT NULL DEFAULT 0,
    UNIQUE (entity_type, entity_id, provider)
);
CREATE INDEX IF NOT EXISTS enrich_due ON enrich_jobs(state, next_run_at);

-- Spotify tokens live in the database so a restart does not force a re-login.
CREATE TABLE IF NOT EXISTS oauth_tokens (
    provider      TEXT PRIMARY KEY,
    access_token  TEXT NOT NULL,
    refresh_token TEXT NOT NULL,
    expires_at    INTEGER NOT NULL
);

-- Imported history is deduplicated on re-import: the same export applied twice
-- must not double every statistic. Local plays are exempt — two genuine listens
-- can share a second.
CREATE UNIQUE INDEX IF NOT EXISTS plays_import_unique
    ON plays(source, played_at, COALESCE(raw_artist,''), COALESCE(raw_title,''))
    WHERE source <> 'plectra';

-- Credentials entered in the UI. Plain text on purpose: this is a single-user
-- database on the user's own machine, and encrypting it with a key stored
-- beside it would be theatre.
CREATE TABLE IF NOT EXISTS settings (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);
