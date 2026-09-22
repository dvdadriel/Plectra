# Plectra — Rencana Implementasi

Spec: `docs/superpowers/specs/2026-09-22-plectra-design.md` (sumber kebenaran).
Rencana ini hanya memecah spec jadi langkah kerja + cara membuktikannya selesai.
Status diperbarui di tempat setiap ada milestone yang tuntas.

Aturan kerja: satu milestone = satu potong yang bisa dijalankan. Tidak ada kode
"untuk nanti". Setiap potongan logika non-trivial meninggalkan satu test yang gagal
bila logikanya rusak.

---

## Status ringkas

| Milestone | Isi | Status |
|---|---|---|
| v0.1 | musiknya bunyi | **Selesai** (2026-09-22) |
| v0.2 | enak dipakai | **Selesai** (2026-09-22) |
| v0.3 | metadata rapi | Belum |
| v0.4 | riwayat mendengarkan | Belum |
| v0.5 | remote dari HP | Belum |
| v0.6 | tampilan vintage | Belum — dijalankan **setelah** v0.3–v0.5 tuntas |

---

## v0.1 — musiknya bunyi — SELESAI

| Bagian | Hasil |
|---|---|
| `store` | SQLite WAL, skema artists/albums/tracks, upsert by `file_hash` |
| `library` | walk direktori, baca tag (dhowden/tag), quick hash, rescan skip by mtime |
| `catalog` | query baca album/artis/track |
| `player` | ring buffer lock-free, decoder FLAC/MP3/Ogg/WAV, resampler, sink malgo, gapless, queue+shuffle+repeat, posisi dari frame sink |
| `api/native` | REST album/artis + kontrol player, SSE `/api/events` |
| `web` | UI minimal: browse, play, transport, volume |

Bukti: `go vet ./...` bersih; test `player` (transisi antrian, seek/pause, file rusak
di-skip, ring wrap) dan `library` (rescan idempoten) hijau; uji nyata dua file WAV
berurutan di sound card, posisi mengikuti frame sink.

Penyederhanaan yang diambil (tercatat sebagai komentar `ponytail:` di kode):
SSE menggantikan WebSocket; `queue` menyatu di `player`; belum ada tabel migrasi;
resampler linear; seek dengan decode-and-discard; durasi diisi saat play pertama.

---

## v0.2 — enak dipakai — SELESAI

Semua butir 2.1–2.8 tuntas dan terbukti. Dua bug ditemukan lewat uji nyata dan
sudah diperbaiki: posisi playback hilang setelah seek/restore (offset seek tidak
ikut dihitung), dan riwayat gagal ditulis saat shutdown karena context sudah
dibatalkan. Keduanya sekarang punya test.

Perubahan struktur: dekoder dipindah dari `player` ke paket baru `internal/audio`
supaya scanner bisa memakai `audio.Probe` untuk mengisi durasi tanpa `library`
bergantung pada `player`.

Urutan dikerjakan dari yang paling dipakai UI ke yang paling jarang.

### 2.1 Skema tambahan
`playlists`, `playlist_items`, `likes`, `plays`, dan tabel FTS5 `tracks_fts`
(judul, artis, album) yang dijaga trigger. Tambah `plays` sekarang walau import
riwayat baru di v0.4 — kolomnya sudah ditetapkan spec, dan pencatatan play lokal
adalah bagian v0.2.
**Selesai bila:** skema dibuat ulang dari nol tanpa error, dan insert track mengisi
`tracks_fts` lewat trigger. ✅

### 2.2 Search (FTS5)
Satu endpoint `GET /api/search?q=` mengembalikan artis, album, dan track.
**Selesai bila:** test mencari potongan judul dan salah ketik spasi tetap ketemu;
query kosong tidak error. ✅

### 2.3 Playlist & likes
`internal/playlist`: CRUD playlist, tambah/hapus/susun ulang item, like/unlike.
API: `GET/POST/PATCH/DELETE /api/playlists`, `POST|DELETE /api/likes/{trackID}`.
**Selesai bila:** test membuat playlist, menambah 3 track, menyusun ulang, menghapus
satu, dan urutan akhir benar setelah dibaca ulang dari DB. ✅

### 2.4 Riwayat play lokal
`internal/history`: player mengirim event selesai/berpindah track, history menulis
baris `plays` (source `plectra`, `ms_played`, `completed`).
Ambang `completed`: >= 50% durasi atau >= 4 menit (konvensi scrobble).
**Selesai bila:** test dengan sink palsu memutar 2 track dan menghasilkan 2 baris
`plays` dengan `completed = true`; track yang di-skip di detik awal tercatat
`completed = false`. ✅

### 2.5 Cover art dari tag tersemat
Ekstrak gambar saat scan, tulis ke cache disk, simpan path di `albums.cover_path`.
`GET /api/cover/{albumID}` menyajikannya; UI menampilkan grid.
**Selesai bila:** scan file bertag gambar menghasilkan file cache dan endpoint
mengembalikan 200 + content-type yang benar; album tanpa gambar mengembalikan 404. ✅

### 2.6 File watcher
fsnotify pada root library; debounce, lalu scan ulang path yang berubah.
**Selesai bila:** test menyalin file baru ke direktori yang sedang diawasi dan
track muncul di katalog tanpa restart. ✅

### 2.7 Persistensi state saat restart
Simpan antrian, index, posisi, volume, shuffle/repeat ke tabel `player_state`
(satu baris). Saat start, pulihkan dalam keadaan pause.
**Selesai bila:** test menulis state, membuka ulang store, dan mendapat state sama. ✅

### 2.8 UI v0.2
Kotak search, halaman playlist, tombol like, grid cover, tampilan antrian.
**Selesai bila:** dijalankan nyata: cari → putar → like → masuk playlist → restart →
antrian kembali. ✅

---

## v0.3 — metadata rapi

MusicBrainz provider (rate limit 1 req/detik, dihormati lewat clock yang bisa
di-fake), tabel `enrich_jobs` + worker backoff, cache cover art dari provider,
OAuth Spotify, enrichment Spotify, import playlist & liked songs dengan pencocokan
ke library lokal. Scanning tetap tidak boleh menunggu enrichment.

## v0.4 — riwayat mendengarkan

Import Spotify GDPR export (baris tak cocok tetap disimpan mentah), jembatan
`recently-played`, scrobble ListenBrainz, rematch otomatis saat file yang cocok
masuk library.

## v0.5 — remote dari HP

`internal/api/subsonic`: subset endpoint di spec, `stream` dan `jukeboxControl`
sebagai dua jalur terpisah, uji kontrak terhadap spesifikasi OpenSubsonic.

---

## v0.6 — tampilan vintage

**Prasyarat: v0.3, v0.4, dan v0.5 sudah selesai.** Jangan dimulai sebelum itu —
mengubah tampilan di atas fitur yang belum ada berarti mengerjakannya dua kali.

### Keputusan framework

**Tidak memakai framework CSS/JS.** Yang dipakai: CSS custom properties untuk
token warna, `@media (prefers-color-scheme)` + atribut `data-theme` untuk pergantian
tema, dan JS vanilla yang sudah ada.

Alasannya bukan selera, tapi tiga batasan spec yang saling mengunci:

| Batasan spec | Akibatnya bagi frontend |
|---|---|
| Distribusi satu binary, tanpa toolchain frontend (§2) | Tailwind, Bootstrap, dan SPA apa pun butuh langkah build. Gugur. |
| Berjalan offline (§1) | CDN gugur, termasuk play-CDN Tailwind. Semua aset harus ikut di-embed. |
| Barrier kontributor rendah (§1) | Satu file CSS yang bisa dibaca siapa saja mengalahkan konvensi framework. |

Kandidat yang sempat masuk hitungan dan kenapa tidak dipakai:
- **Tailwind (build lokal)** — hasilnya bagus, tapi menambah Node ke syarat kontribusi.
- **Pico.css / classless CSS** — satu file, bisa di-embed, tapi temanya generik;
  kita justru sedang mengejar tampilan spesifik, jadi nilainya tinggal sedikit.
- **Alpine.js (15KB, tanpa build)** — dipertimbangkan bila interaksi UI tumbuh
  jauh lebih ramai dari sekarang. Hari ini `EventSource` + beberapa event listener
  sudah cukup; ditunda sampai ada bukti kodenya jadi susah dirawat.

Yang **ditambahkan**: satu font bertema vintage yang di-embed di binary (bukan
Google Fonts — itu melanggar syarat offline), dipilih dari font berlisensi bebas.

**Keputusan (disetujui 2026-09-22): tetap vanilla.** Next.js ditolak karena SSR dan
API routes-nya butuh Node saat runtime — dua server untuk satu pemutar lokal, dan
static export-nya hanya React dengan langkah ekstra. React dan Vue keduanya sah
menurut §12 (SPA dengan build yang di-embed, preseden Navidrome), tapi harganya
`node_modules` dan langkah build di CI untuk UI yang hari ini 217 baris dengan 11
event handler dan satu objek state dari SSE.

Pindah ke framework **hanya** bila salah satu pemicu ini kena — patokan, bukan selera:

1. UI tembus ~600 baris, atau
2. butuh routing sungguhan (deep link ke album/playlist, tombol back), atau
3. muncul state kompleks yang tidak datang dari SSE (drag-drop reorder queue,
   multi-select, edit inline).

Bila kena: **Vue 3 + Vite**, hasil `dist/` di-embed, Go tetap satu-satunya server.

### Palet

Sumber: https://colorhunt.co/palette/0a2947f3e4c9d3d4c08b5e3c

| Token | Hex | Nama kerja |
|---|---|---|
| `--navy` | `#0A2947` | biru tinta |
| `--cream` | `#F3E4C9` | kertas tua |
| `--sage` | `#D3D4C0` | hijau kertas pudar |
| `--leather` | `#8B5E3C` | cokelat kulit |

Keempatnya dipakai di **kedua** tema; yang berpindah adalah perannya.

| Peran | Light | Dark |
|---|---|---|
| Latar halaman | `--cream` | `--navy` |
| Permukaan (kartu, footer, header) | `--sage` | navy yang dinaikkan sedikit terangnya |
| Teks utama | `--navy` | `--cream` |
| Teks sekunder | navy 65% | `--sage` |
| Aksen (playing, progress, like aktif) | `--leather` | `--leather` |
| Garis / pemisah | leather 25% | sage 20% |
| Teks di atas aksen | `--cream` | `--cream` |

**Aturan kontras yang wajib dipatuhi:** `--leather` di atas `--navy` hanya sekitar
2.2:1, jadi di tema gelap cokelat **tidak boleh jadi warna teks** — hanya untuk isian,
garis, dan indikator, dengan teks `--cream` di atasnya. Di tema terang, navy di atas
cream sekitar 12:1 dan aman untuk teks kecil.

### Butir kerja

1. **Token & pergantian tema** — satu blok `:root` untuk light, override di
   `@media (prefers-color-scheme: dark)` dan `[data-theme="dark"]`, plus tombol
   tema yang menyimpan pilihan di `localStorage`.
   Selesai bila: light, dark, dan "ikut sistem" semuanya benar tanpa flash saat load.
2. **Tipografi vintage** — font terembed, skala ukuran, angka tabular untuk durasi.
   Selesai bila: tidak ada permintaan jaringan keluar saat halaman dibuka (cek di
   tab Network dengan mesin offline).
3. **Terapkan ke seluruh layar** — album grid, daftar track, playlist, likes, queue,
   search, footer transport, state kosong, state error.
   Selesai bila: setiap layar dicek di kedua tema, tidak ada warna hardcoded tersisa.
4. **Pass impeccable** — jalankan skill `impeccable` untuk audit hierarki visual,
   aksesibilitas (kontras, fokus keyboard, target sentuh), perilaku responsif di
   lebar ~400px, dan gerak (`prefers-reduced-motion`).
   Selesai bila: temuan audit sudah dikerjakan atau ditolak dengan alasan tertulis.
5. **Bukti akhir** — screenshot kedua tema dari instance yang benar-benar jalan.

---

## Yang tetap ditunda

Sesuai spec §11: podcast, lirik, crossfade, equalizer, multi-user, Chromecast/UPnP,
transcoding, playlist kolaboratif, discovery, offline mobile, output ke browser.
