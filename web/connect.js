/*
 * Plectra — import every Spotify playlist in one run.
 *
 * Paste this into the DevTools console on https://open.spotify.com while you
 * are logged in. It borrows the access token the web player already holds for
 * your session, reads your playlists and Liked Songs through the same endpoints
 * the player itself uses, and posts the result to Plectra.
 *
 * Nothing is configured and nothing is stored: no developer app, no client id,
 * no redirect URI, and your password is never seen. The token belongs to the
 * page, lives about an hour, and is never written down.
 *
 * This reads the web player's private API, not the documented one. It is the
 * price of needing no configuration: Spotify can change it without notice, and
 * if it does this script stops working — it cannot break Plectra or your
 * library, which only ever sees the finished JSON.
 */

(async () => {
  const PLECTRA = '__PLECTRA_ORIGIN__';

  if (!location.hostname.endsWith('open.spotify.com')) {
    console.error('Plectra: run this on https://open.spotify.com, logged in.');
    return;
  }

  const log = (...a) => console.log('%cPlectra', 'color:#0a7', ...a);

  // The player keeps a session token for its own API calls. Two places have
  // held it across redesigns; try the endpoint first, then the inlined blob.
  async function token() {
    try {
      const r = await fetch('/api/token?reason=transport&productType=web_player', {credentials: 'include'});
      if (r.ok) {
        const j = await r.json();
        if (j.accessToken) return j.accessToken;
      }
    } catch (e) {}
    const el = document.getElementById('session');
    if (el) {
      try {
        const j = JSON.parse(el.textContent);
        if (j.accessToken) return j.accessToken;
      } catch (e) {}
    }
    return null;
  }

  const tok = await token();
  if (!tok) {
    console.error('Plectra: could not read the session token. Reload the page while logged in and try again.');
    return;
  }

  // Spotify pages every list. Follow `next` until it runs out rather than
  // guessing a limit — a 2000-track playlist is normal.
  async function pages(url) {
    const out = [];
    while (url) {
      const r = await fetch(url, {headers: {Authorization: 'Bearer ' + tok}});
      if (!r.ok) throw new Error(url + ' answered ' + r.status);
      const j = await r.json();
      out.push(...(j.items || []));
      url = j.next;
      if (url) await new Promise(r => setTimeout(r, 120)); // be a polite client
    }
    return out;
  }

  const track = (t) => t && t.name && {
    name: t.name,
    artists: (t.artists || []).map(a => a.name),
    album: t.album ? t.album.name : '',
  };

  log('reading your playlists…');
  const lists = await pages('https://api.spotify.com/v1/me/playlists?limit=50');

  const playlists = [];
  for (const pl of lists) {
    const items = await pages(`https://api.spotify.com/v1/playlists/${pl.id}/tracks?limit=100`);
    const tracks = items.map(i => track(i.track)).filter(Boolean);
    log(`  ${pl.name} — ${tracks.length} tracks`);
    playlists.push({name: pl.name, spotifyId: pl.id, tracks});
  }

  log('reading Liked Songs…');
  const liked = (await pages('https://api.spotify.com/v1/me/tracks?limit=50'))
    .map(i => track(i.track)).filter(Boolean);

  const body = {source: 'spotify', playlists, liked};
  log(`sending ${playlists.length} playlists and ${liked.length} liked songs to Plectra…`);

  const resp = await fetch(PLECTRA + '/api/playlists/import', {
    method: 'POST',
    headers: {'Content-Type': 'application/json'},
    body: JSON.stringify(body),
  }).catch(e => {
    console.error('Plectra: could not reach ' + PLECTRA + '. Is it running?', e);
  });
  if (!resp) return;
  if (!resp.ok) {
    console.error('Plectra refused the import:', await resp.text());
    return;
  }

  const res = await resp.json();
  log(`done — ${res.playlists} playlists created, ${res.matched} tracks matched, ${res.liked} liked.`);
  if (res.empty && res.empty.length) log('nothing local in:', res.empty.join(', '));
  if (res.unmatched && res.unmatched.length) {
    log(`${res.unmatched.length} tracks are not in your library yet:`, res.unmatched.slice(0, 20));
  }
  log('Open Plectra → Playlists.');
})();
