# Plectra — Desain Sistem

**Tanggal:** 2026-09-22
**Status:** Disetujui untuk perencanaan implementasi

---

## 1. Tujuan & Batasan

Plectra adalah pemutar musik desktop self-hosted, open source, tanpa iklan. Pengguna
menjalankan instansinya sendiri di mesin sendiri; tidak ada layanan terpusat dan tidak
ada yang perlu dimonitor oleh pemelihara proyek.

### Masalah yang diselesaikan

Iklan Spotify free-tier disisipkan server-side ke dalam stream. Tidak ada aplikasi client
yang bisa menghilangkannya, dan Web API Spotify tidak menyediakan akses audio sama
sekali. Karena itu Plectra harus memiliki sumber audionya sendiri. Spotify tetap dipakai,
tapi sebagai sumber metadata dan jalur migrasi — bukan sumber audio.

### Batasan yang mengarahkan seluruh desain

1. **Distribusi satu binary.** Faktor adopsi nomor satu untuk software self-hosted.
   Setiap keputusan yang menambah langkah instalasi harus membayar harganya.
2. **Berjalan offline.** Tidak ada layanan eksternal yang boleh menjadi syarat agar
   musik bisa diputar.
3. **Legal sepenuhnya.** Tidak ada bypass DRM, tidak ada scraping stream berbayar.
4. **Barrier kontributor rendah.** Ini proyek open source; kode harus mudah dimasuki
   orang baru.

### Non-tujuan

Multi-tenant hosting, layanan cloud, katalog streaming, aplikasi mobile native buatan
sendiri, dan microservices.

---

## 2. Keputusan Teknis Utama

| Keputusan | Pilihan | Alasan |
|---|---|---|
| Bahasa | **Go** | Satu-satunya yang menang di dua kriteria sekaligus: binary statis lintas platform, dan barrier kontributor rendah. Rust lebih cepat tapi memangkas velocity dan kontributor; bottleneck di sini I/O disk dan decoding, bukan bahasa host. Preseden: Navidrome, Gonic. |
| Arsitektur | **Modular monolith** | Untuk deployment self-hosted single-user, microservices adalah pajak operasional tanpa imbalan. Batas domain dijaga lewat interface, sehingga pemecahan di kemudian hari adalah pekerjaan hari, bukan rewrite. |
| Database | **SQLite (WAL) + FTS5** | Satu user, satu mesin. Postgres hanya menambah langkah instalasi. |
| Output audio | **miniaudio via malgo** | Satu API untuk WASAPI / CoreAudio / ALSA-PulseAudio. |
| Dekoder | **Pure-Go** (FLAC, MP3, Ogg, Opus, WAV) | Mempertahankan janji zero-dependency. ffmpeg dipakai opsional bila ada di PATH untuk AAC/ALAC/WMA. |
| UI | Web UI di-embed lewat `embed.FS`, diakses di localhost | Tidak ada toolchain frontend yang perlu dipasang pengguna. |
| Client pihak ketiga | **OpenSubsonic + jukebox mode** | Memberi client mobile matang tanpa menulis kode mobile. |

### Konsekuensi yang diterima

Output audio native memerlukan **cgo**, sehingga cross-compile bukan lagi satu perintah
dan rilis harus lewat CI per-OS. Janji "unduh dan jalankan" tetap utuh bagi pengguna;
yang bertambah rumit hanya proses rilis.

---

## 3. Arsitektur

```
cmd/plectra                  entrypoint, wiring, konfigurasi

internal/store              SQLite + migrasi. Satu-satunya yang menulis SQL.
internal/library            scan direktori, watcher, baca tag, hash file
internal/catalog            query: album/artis/track/search (FTS5)
internal/metadata           enrichment: provider MusicBrainz + Spotify
internal/player             mesin audio: decode -> ring buffer -> output device
internal/queue              antrian, shuffle, repeat
internal/playlist           playlist, likes
internal/history            riwayat play, import, scrobble
internal/api/native         REST + WebSocket untuk web UI sendiri
internal/api/subsonic       OpenSubsonic, termasuk jukebox mode
web/                        aset UI (di-embed)
```

**Dependensi mengalir satu arah:** `api` -> `player` / `catalog` / `playlist` /
`history` -> `store`. Tidak ada panah balik. `player` tidak mengetahui HTTP; `api`
tidak mengetahui ALSA. Tidak ada package yang membaca tabel milik package lain;
akses data selalu lewat interface pemilik domainnya.

### Tiga interface yang menjadi sendi sistem

```go
// Dari mana byte audio datang. v1: file lokal.
type Source interface {
    Open(ctx context.Context, trackID string) (io.ReadSeekCloser, Format, error)
}

// Dari mana metadata datang. MusicBrainz, Spotify, tag lokal.
type MetadataProvider interface {
    Lookup(ctx context.Context, q Query) ([]Match, error)
}

// Ke mana audio keluar. v1: sound card lokal.
type Sink interface {
    Write(pcm []float32) (int, error)
    Format() Format
    Close() error
}
```

Ketiganya adalah alasan monolith ini tidak akan menjadi bola lumpur, dan alasan sumber
atau output baru bisa ditambahkan tanpa menyentuh inti.

---

## 4. Mesin Audio

### Pipeline

```
          goroutine decoder                      thread audio (milik OS)
         ────────────────────                  ────────────────────────
Source -> Decoder -> Resampler -> Gain -> [ RING BUFFER ] -> Sink callback -> speaker
             |                              lock-free, ~500ms        |
        prefetch track                                          framesPlayed++
         berikutnya                                                  |
                                                                     v
                                                             posisi & event
```

### Aturan thread audio

Callback audio tidak boleh mengalokasi memori, mengambil mutex, menyentuh disk, atau
memanggil apa pun yang dapat blocking. Ia hanya menyalin frame dari ring buffer dan
menaikkan counter. Semua pekerjaan berat berada di goroutine decoder.

Konsekuensinya, kontrol player memakai channel command, bukan pemanggilan method
langsung:

```go
type Player struct {
    cmds   chan command   // Play, Pause, Seek, Next, SetVolume, ...
    events chan Event     // dikonsumsi lapisan API, di-fan-out ke WebSocket
}
```

Satu goroutine `run()` memiliki seluruh state player dan memproses command secara
serial. Tidak ada mutex di jalur playback dan tidak ada race, karena hanya satu pihak
yang berhak mengubah state.

### Gapless

Ketika sisa track kurang dari 5 detik, decoder membuka track berikutnya dan mengisi
buffer kedua. Pada sampel terakhir track berjalan, decoder menyambung langsung dari
buffer kedua tanpa menutup dan membuka ulang device audio. Perbedaan sample rate
diselaraskan oleh resampler ke format sink yang sedang terbuka.

Bila prefetch gagal, track berikutnya di-skip, event error dikirim, dan playback
berlanjut. Satu file rusak tidak boleh menghentikan musik.

### Posisi playback

Posisi dihitung dari jumlah frame yang benar-benar dikonsumsi sink, bukan dari
`time.Now()`. Wall clock akan melenceng terhadap audio clock, dan itulah penyebab
progress bar tidak sinkron. Sink counter adalah sumber kebenaran.

### Underrun

Ring buffer kosong menyebabkan sink menulis **silence**, bukan mengulang buffer lama.
Event `buffering` dikirim dan decoder mengejar. Underrun beruntun dicatat sebagai
metrik agar penyebabnya (disk atau resampler) dapat dilacak.

---

## 5. Library & Metadata

### Alur scanning

```
fsnotify / scan manual
        |
        v
  work queue (N worker, N = NumCPU/2)
        |
        +-- baca tag (FLAC / ID3 / Vorbis)
        +-- hash cepat (ukuran + mtime + 64KB pertama)
        +-- upsert ke SQLite
                |
                v
        enqueue enrich job (antrian terpisah)
                |
                +-- MusicBrainz  (rate limit 1 req/detik, wajib dipatuhi)
                +-- Spotify      (metadata + cover art)
                +-- cache cover art ke disk; path disimpan di DB
```

Scanning dan enrichment sengaja dipisah. Scan harus selesai dalam hitungan detik dan
bisa berjalan offline; enrichment boleh berjam-jam, boleh gagal, dan boleh diulang.
Menggabungkannya berarti menyandera library pada rate limit penyedia eksternal.

Hash file dipakai sebagai identitas agar file yang dipindah atau di-rename tidak
kehilangan riwayat play dan keanggotaan playlist-nya.

### Peran Spotify

Spotify berperan sebagai penyedia metadata dan jalur migrasi:

- Enrichment: cover art resolusi tinggi, penamaan album/artis yang konsisten,
  tanggal rilis.
- Import playlist dan liked songs, dicocokkan ke library lokal.
- Import riwayat mendengarkan (lihat bagian 6).

**Yang tidak tersedia dan tidak direncanakan:** audio stream. Selain itu, sejak akhir
2024 Spotify menutup `/recommendations`, `/audio-features`, `/audio-analysis`,
`related-artists`, dan featured playlists bagi aplikasi baru — sehingga discovery
tidak boleh dibangun di atas Spotify. MusicBrainz adalah sumber metadata kanonik;
Spotify melengkapi, bukan menjadi fondasi.

---

## 6. Riwayat Mendengarkan

Diperlakukan sebagai satu modul utuh, mencakup tiga arah aliran data:

| Arah | Sumber / tujuan | Catatan |
|---|---|---|
| Masuk, backfill | **Spotify GDPR export** | Riwayat penuh seumur akun dengan timestamp dan `ms_played`. Diminta lewat Privacy Settings; file JSON tiba dalam 2–4 minggu. Ini sumber data historis yang sebenarnya. |
| Masuk, berkelanjutan | `GET /me/player/recently-played` | Hanya 50 track terakhir, perlu di-poll. Jembatan masa transisi; mengering dengan sendirinya setelah Plectra menjadi pemutar utama. |
| Keluar | **Scrobble ListenBrainz** | Terbuka, gratis, dan riwayat dapat diekspor kapan saja. |

Import yang tidak cocok dengan library lokal tetap disimpan beserta metadata mentahnya.
Membuang baris tersebut akan membuat statistik berbohong. Ketika file yang bersesuaian
kelak masuk ke library, proses rematch mengklaim riwayat lamanya.

---

## 7. Model Data

```sql
artists        id, name, sort_name, mbid, spotify_id, image_path

albums         id, title, artist_id, year, mbid, spotify_id, cover_path, disc_count

tracks         id, album_id, artist_id, title, track_no, disc_no,
               duration_ms, format, bitrate, sample_rate, channels,
               path, file_hash, mtime, replaygain_track, replaygain_album,
               mbid, spotify_id, added_at

playlists      id, name, description, created_at, updated_at, is_smart, rules_json

playlist_items playlist_id, track_id, position            -- PK gabungan

likes          track_id, liked_at

plays          id, track_id (NULL bila tak cocok), played_at, ms_played, completed,
               source,                     -- 'plectra' | 'spotify_export'
                                           -- | 'spotify_api' | 'listenbrainz'
               raw_artist, raw_album, raw_title,
               spotify_track_id

enrich_jobs    id, entity_type, entity_id, provider, state, attempts, next_run_at
```

### Keputusan skema yang disengaja

**`plays` menyimpan event, bukan counter.** Play count adalah hasil `COUNT(*)`.
Menyimpan counter akan menghilangkan kemampuan membuat statistik berbasis waktu, dan
data yang diperlukan untuk mengembalikannya sudah hilang permanen.

**`file_hash` adalah identitas, bukan `path`.** File yang dipindah atau di-rename tetap
membawa riwayat dan keanggotaan playlist-nya.

**`enrich_jobs` adalah tabel, bukan antrian di memori.** Plectra yang mati di tengah
enrichment melanjutkan dari posisi terakhir.

**Playlist pintar** hanya menyimpan aturan sebagai JSON dan dievaluasi saat query;
tidak ada tabel tambahan.

---

## 8. API

### Native (untuk web UI sendiri)

```
GET    /api/albums?sort=&limit=&cursor=
GET    /api/albums/{id}
GET    /api/artists
GET    /api/artists/{id}
GET    /api/tracks/{id}
GET    /api/search?q=                 -- FTS5, satu endpoint untuk semua entitas
GET    /api/playlists
POST   /api/playlists
PATCH  /api/playlists/{id}
DELETE /api/playlists/{id}
POST   /api/likes/{trackID}
DELETE /api/likes/{trackID}
GET    /api/cover/{albumID}

POST   /api/player/play               {trackIDs | albumID | playlistID, startIndex}
POST   /api/player/pause
POST   /api/player/resume
POST   /api/player/next
POST   /api/player/prev
POST   /api/player/seek               {positionMs}
POST   /api/player/volume             {level}
POST   /api/player/mode               {shuffle, repeat}
GET    /api/player/queue
POST   /api/player/queue              -- susun ulang

WS     /api/events                    -- state player, progress scan, hasil enrichment
```

WebSocket bersifat satu arah (server ke client) untuk penyiaran state. Perintah tetap
melalui POST agar mudah di-debug dengan curl dan tidak memerlukan protokol RPC buatan
sendiri di atas WebSocket.

### OpenSubsonic (untuk client pihak ketiga)

Subset yang dipakai client nyata: `ping`, `getLicense`, `getMusicFolders`, `getIndexes`,
`getArtists`, `getArtist`, `getAlbum`, `getAlbumList2`, `getSong`, `search3`,
`getPlaylists`, `getPlaylist`, `createPlaylist`, `updatePlaylist`, `star`, `unstar`,
`getCoverArt`, `stream`, `scrobble`, `jukeboxControl`.

`stream` dan `jukeboxControl` adalah dua jalur berbeda: `stream` mengirim audio ke
device client, `jukeboxControl` mengendalikan speaker mesin Plectra. Keduanya didukung;
client yang memilih.

---

## 9. Penanganan Error

| Kegagalan | Perilaku |
|---|---|
| File tidak terbaca saat play | Skip, kirim event error, lanjutkan antrian |
| Sound card hilang (unplug) | Pause, coba buka ulang device, kirim event ke UI |
| Provider metadata down | Enrichment ditunda dengan backoff; library tetap utuh |
| Token Spotify kedaluwarsa | Refresh otomatis; bila gagal, nonaktifkan provider tanpa retry beruntun |
| Rate limit provider tercapai | Hormati `Retry-After`; job dijadwalkan ulang |
| Database terkunci | Retry dengan backoff; WAL mode aktif sejak awal |
| Import riwayat sebagian gagal | Baris valid tetap masuk; baris gagal dilaporkan, bukan membatalkan seluruh import |

Prinsip umum: kegagalan pada jalur metadata atau sumber eksternal tidak boleh
menghentikan musik.

---

## 10. Strategi Pengujian

- **`player`** — sink palsu yang mengonsumsi PCM pada kecepatan terkendali. Gapless,
  seek, underrun, dan transisi antrian diuji secara deterministik tanpa sound card,
  sehingga mesin audio dapat diuji di CI.
- **`library`** — scan direktori fixture berisi file audio pendek dengan tag yang
  sengaja berantakan (hilang, salah encoding, konflik).
- **`metadata`** — provider di-mock lewat interface. Rate limit dan backoff diuji
  dengan clock palsu.
- **`history`** — import file GDPR export contoh; verifikasi pencocokan, penanganan
  baris tak cocok, dan rematch setelah track masuk library.
- **`api/subsonic`** — uji kontrak terhadap spesifikasi OpenSubsonic untuk menjaga
  kompatibilitas client pihak ketiga tidak rusak diam-diam.

---

## 11. Rencana Rilis

**v0.1 — musiknya bunyi**
Scan library, baca tag, SQLite, mesin player dengan gapless, queue, web UI minimal
(browse album dan artis, play, kontrol transport). Tanpa akses jaringan keluar sama
sekali. Milestone ini membuktikan bagian tersulit sudah beres.

**v0.2 — enak dipakai**
Search (FTS5), playlist, likes, pencatatan riwayat play lokal, cover art dari tag
tersemat, file watcher, persistensi state saat restart.

**v0.3 — metadata rapi**
Provider MusicBrainz, antrian enrichment dengan backoff, cache cover art, OAuth
Spotify, enrichment Spotify, import playlist dan liked songs dengan pencocokan ke
library lokal.

**v0.4 — riwayat mendengarkan**
Import Spotify GDPR export, jembatan `recently-played`, scrobble ListenBrainz,
rematch otomatis untuk baris yang sebelumnya tidak cocok.

**v0.5 — remote dari HP**
OpenSubsonic dan jukebox mode.

### Ditunda sampai ada permintaan nyata

Podcast, lirik, crossfade, equalizer, multi-user dan autentikasi, Chromecast/UPnP,
transcoding on-the-fly, playlist kolaboratif, discovery/rekomendasi, mode offline
mobile, output ke browser.

Discovery secara khusus ditunda: rekomendasi yang baik memerlukan data riwayat.
Setelah import GDPR di v0.4, data itu baru tersedia — dan keputusan desainnya dapat
diambil berdasarkan pengukuran, bukan tebakan.

---

## 12. Yang Belum Diputuskan

Diserahkan ke fase perencanaan implementasi:

- Framework web UI (kandidat: HTMX + Go template, atau SPA dengan build yang di-embed)
- Pustaka spesifik untuk dekoder dan resampler
- Skema konfigurasi dan lokasi file config per-OS
- Format dan lokasi cache cover art
- Tema, tipografi, dan bahasa antarmuka — sengaja dikeluarkan dari spec ini
