package native

import (
	"crypto/subtle"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// Settings exists so everything Plectra needs can be entered once, in the
// interface, instead of by hand in a file a new listener has never opened. It
// writes the same .env the binary reads at startup — there is no second source
// of truth, and a value typed here looks exactly like a value typed there.
//
// Secrets are never read back out. The panel is told whether a key is set,
// which is all it needs to draw itself, and a key that leaves this process
// does not come back to a browser.
type settings struct {
	envPath string
	// password is the OpenSubsonic password as it stood at startup. It is what
	// guards these routes: the rest of the native API is open, so the one page
	// that writes credentials cannot be.
	password string
	scanner  RootSetter // nil when no scanner is wired
	addr     string     // the address the server was told to listen on
	user     string     // the OpenSubsonic username
}

// Setup is what the settings routes need to know about the running server.
type Setup struct {
	EnvPath  string
	Password string
	Addr     string
	User     string
	Scanner  RootSetter
}

// RootSetter is the slice of the scanner settings may use: where the library
// is, and pointing it somewhere else.
type RootSetter interface {
	Root() string
	SetRoot(string) error
}

// WithSettings turns on the setup routes, writing to s.EnvPath.
func (a *API) WithSettings(s Setup) *API {
	a.settings = &settings{
		envPath: s.EnvPath, password: s.Password,
		scanner: s.Scanner, addr: s.Addr, user: s.User,
	}
	return a
}

func (a *API) settingsRoutes(mux *http.ServeMux) {
	if a.settings == nil {
		return
	}
	mux.HandleFunc("GET /api/settings", a.getSettings)
	mux.HandleFunc("PUT /api/settings", a.putSettings)
}

// field is one thing that can be configured: what .env calls it, what the panel
// calls it, and where to go to get one. The guidance lives here so that adding
// a key later is one entry rather than an edit in two languages.
type field struct {
	Name  string `json:"name"`  // what the JSON body calls it
	Label string `json:"label"` // what the panel calls it
	Help  string `json:"help"`
	Link  string `json:"link,omitempty"`
	// Secret fields are write-only: their value never leaves the process.
	Secret bool `json:"secret"`
	// Live fields take effect on save. The rest are read at startup, and the
	// panel says so rather than letting someone wonder why nothing happened.
	Live  bool   `json:"live"`
	Set   bool   `json:"set"`
	Value string `json:"value,omitempty"` // only ever filled for a non-secret

	env string // canonical .env name, appended when the file has neither
	alt string // the other spelling the loader accepts
}

// fields is every key Plectra reads. Nothing else belongs in .env: a setting
// the code never looks at is a promise the interface cannot keep.
func fields() []field {
	return []field{{
		Name: "musicDir", Label: "Music folder",
		Help: "Where your music lives. Point Plectra at it, save, then press Look for music below.",
		Live: true,
		env:  "PLECTRA_MUSIC", alt: "music-dir",
	}, {
		Name: "subsonicPassword", Label: "Listening password",
		Help:   "Lets your phone and other apps play from this collection, signing in as \"plectra\". Any password works, so choose a strong one: anyone on your network can try it. Leave it empty and nothing outside this machine can connect.",
		Secret: true,
		env:    "PLECTRA_PASSWORD", alt: "subsonic-password",
	}, {
		Name: "lastfmApiKey", Label: "Last.fm key",
		Help:   "Optional. A spare source for the rows about what other people are playing, which already work without it. Make a Last.fm account, then any name and a blank callback will do.",
		Link:   "https://www.last.fm/api/account/create",
		Secret: true,
		env:    "LASTFM_API_KEY", alt: "last-fm-api-key",
	}}
}

// authorised guards the setup routes. With a password configured it is the
// password, from anywhere. Without one there is nothing yet to check against,
// so a first run is allowed from the machine itself and nowhere else.
//
// A reverse proxy or a tunnel also connects over the loopback, so that
// exception is genuinely local only while nothing forwards to this port —
// which is the state a first run is in.
func (s *settings) authorised(r *http.Request) bool {
	if s.password == "" {
		return isLoopback(r)
	}
	return subtle.ConstantTimeCompare(
		[]byte(r.Header.Get("X-Plectra-Password")), []byte(s.password)) == 1
}

func isLoopback(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (a *API) getSettings(w http.ResponseWriter, r *http.Request) {
	// Which keys are set is not itself a secret, but it is still only for
	// whoever may change them: it says what a stolen request would be worth.
	if !a.settings.authorised(r) {
		http.Error(w, "not allowed", http.StatusForbidden)
		return
	}
	// What is stored, not what this process was started with: a key saved a
	// moment ago is set, even though it does not take effect until a restart.
	// The panel says which of those two it is with its own badge.
	stored := storedEnv(a.settings.envPath)
	out := fields()
	for i := range out {
		f := &out[i]
		f.Set = stored[f.env] != "" || stored[f.alt] != "" ||
			os.Getenv(f.env) != "" || os.Getenv(f.alt) != ""
		if f.Name == "musicDir" && a.settings.scanner != nil {
			// The live value, not the stored one: a folder given on the command
			// line never reaches .env, and showing the file would be a lie.
			f.Value = a.settings.scanner.Root()
			f.Set = f.Value != ""
		}
	}
	writeJSON(w, map[string]any{"fields": out, "mobile": a.settings.mobile()})
}

// mobile describes how to reach this server from a phone. Typing an address
// from memory is where a setup usually goes wrong, so the panel shows the one
// that actually works rather than leaving it to be guessed.
func (s *settings) mobile() map[string]any {
	if s.password == "" {
		return map[string]any{"ready": false,
			"note": "Set the listening password above, then start Plectra again. The address to type into your phone appears here once it is on."}
	}
	host := lanAddress(s.addr)
	if host == "" {
		_, port, _ := net.SplitHostPort(s.addr)
		if port == "" {
			port = "4533"
		}
		return map[string]any{"ready": false,
			"note": "Plectra is keeping to this machine, so no phone can reach it yet. Start it again with the address set to 0.0.0.0:" + port + " and they can."}
	}
	return map[string]any{"ready": true, "url": "http://" + host, "user": s.user}
}

// lanAddress turns the address the server listens on into one another device
// can type in. A server bound to every interface answers on the machine's own
// LAN address, and that is the one a phone needs: 0.0.0.0 is not a destination,
// and 127.0.0.1 is a different machine from over there.
func lanAddress(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return ""
	}
	if ip := net.ParseIP(host); ip != nil && !ip.IsUnspecified() {
		if ip.IsLoopback() {
			return "" // reachable from here and nowhere else
		}
		return net.JoinHostPort(host, port)
	}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ""
	}
	for _, a := range addrs {
		n, ok := a.(*net.IPNet)
		if !ok || n.IP.IsLoopback() {
			continue
		}
		if v4 := n.IP.To4(); v4 != nil {
			return net.JoinHostPort(v4.String(), port)
		}
	}
	return ""
}

func (a *API) putSettings(w http.ResponseWriter, r *http.Request) {
	if !a.settings.authorised(r) {
		http.Error(w, "not allowed", http.StatusForbidden)
		return
	}
	// A settings body is a handful of short strings. Anything larger is not one.
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	var req map[string]string
	if err := decode(r, &req); err != nil {
		http.Error(w, "bad request", 400)
		return
	}

	changes := map[string]string{}
	for _, f := range fields() {
		v, ok := req[f.Name]
		if !ok {
			continue // absent means "leave it alone", which is not "clear it"
		}
		v = strings.TrimSpace(v)
		if strings.ContainsAny(v, "\n\r") {
			http.Error(w, "a value cannot span lines", 400)
			return
		}
		// A folder that does not exist is refused before anything is written:
		// saving it would leave the library pointing at nothing.
		if f.Name == "musicDir" && v != "" && a.settings.scanner != nil {
			if err := a.settings.scanner.SetRoot(v); err != nil {
				http.Error(w, err.Error(), 400)
				return
			}
		}
		changes[f.env] = v
	}
	if len(changes) == 0 {
		writeJSON(w, map[string]any{"saved": false})
		return
	}
	if err := writeEnv(a.settings.envPath, changes); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	writeJSON(w, map[string]any{"saved": true})
}

// storedEnv reads the values .env holds, which is what the panel reports as
// set. A missing or unreadable file is simply an empty one: this only decides
// how a badge reads.
func storedEnv(path string) map[string]string {
	out := map[string]string{}
	b, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if k, v, ok := strings.Cut(line, "="); ok {
			out[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"'`)
		}
	}
	return out
}

// writeEnv updates keys in place and appends the ones the file does not have,
// leaving every other line — comments included — exactly as it found them. The
// file holds credentials, so it is written 0600 and replaced atomically: a
// half-written .env is a server that will not start.
func writeEnv(path string, changes map[string]string) error {
	existing, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read %s: %w", path, err)
	}

	// Either spelling of a key counts as that key's line.
	canonical := map[string]string{}
	for _, f := range fields() {
		canonical[f.env], canonical[f.alt] = f.env, f.env
	}

	var out []string
	seen := map[string]bool{}
	for _, line := range strings.Split(string(existing), "\n") {
		key, _, isPair := strings.Cut(line, "=")
		name := canonical[strings.TrimSpace(key)]
		if !isPair || strings.HasPrefix(strings.TrimSpace(line), "#") || name == "" {
			out = append(out, line)
			continue
		}
		v, ok := changes[name]
		if !ok {
			out = append(out, line)
			continue
		}
		if seen[name] {
			continue // a duplicate of a key already written: drop it
		}
		seen[name] = true
		out = append(out, strings.TrimSpace(key)+"="+v)
	}
	for _, f := range fields() {
		if v, ok := changes[f.env]; ok && !seen[f.env] {
			out = append(out, f.env+"="+v)
		}
	}

	body := strings.TrimRight(strings.Join(out, "\n"), "\n") + "\n"
	tmp := filepath.Join(filepath.Dir(path), ".env.tmp")
	if err := os.WriteFile(tmp, []byte(body), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("replace %s: %w", path, err)
	}
	return os.Chmod(path, 0o600)
}
