package main

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"runtime/debug"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/abort-retry-ignore/central_clipboard"
)

const (
	pingEvery = 15 * time.Second
	bigClip   = 1 << 20 // clips above this trigger returning memory to the OS
)

type config struct {
	maxBytes   int64
	passphrase string // empty disables enrollment
	publicURL  string // overrides the address derived from the Host header
}

type Server struct {
	cfg      config
	store    *Store
	reg      *Registry
	throttle *throttle
	ping     time.Duration // how often idle event streams get a keep-alive
	files    fs.FS         // what we hand out: agents, scripts, shortcuts
}

func NewServer(cfg config, store *Store, reg *Registry) *Server {
	return &Server{cfg: cfg, store: store, reg: reg, throttle: newThrottle(5, 5*time.Minute), ping: pingEvery, files: assets.FS}
}

// respWriter records the status and device name for the access log and keeps
// Flush and per-write deadlines working through http.ResponseController.
type respWriter struct {
	http.ResponseWriter
	status int
	who    string
}

func (w *respWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *respWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (s *Server) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	w := &respWriter{ResponseWriter: rw, status: http.StatusOK, who: "-"}
	if r.URL.Path != "/healthz" {
		defer func() { // the path only: never log ?t=token
			log.Printf("%s %s %s %s %d", clientIP(r), w.who, r.Method, r.URL.Path, w.status)
		}()
	}
	switch p := r.URL.Path; p {
	case "/healthz":
		io.WriteString(w, "ok")
	case "/join":
		s.joinPage(w, r)
	case "/install.sh":
		s.installScript(w, r)
	case "/agent.service":
		s.asset(w, "agent/central-clipboard-agent.service", "text/plain; charset=utf-8", "")
	case "/enroll":
		s.enroll(w, r)
	case "/", "/clip", "/events":
		s.authed(w, r, p)
	default:
		if name, ok := strings.CutPrefix(p, "/shortcuts/"); ok && strings.HasSuffix(name, ".shortcut") {
			s.asset(w, "shortcuts/"+name, "application/octet-stream",
				"attachment; filename*=UTF-8''"+url.PathEscape(name))
		} else if name, ok := strings.CutPrefix(p, "/agent/"); ok && agentFileRE.MatchString(name) {
			s.asset(w, "agentbin/"+name, "application/gzip", "")
		} else {
			http.NotFound(w, r)
		}
	}
}

var agentFileRE = regexp.MustCompile(`^linux-(amd64|arm64)\.gz$`)

func (s *Server) asset(w *respWriter, name, contentType, disposition string) {
	b, err := fs.ReadFile(s.files, name)
	if err != nil {
		http.Error(w, "not in this build", http.StatusNotFound)
		return
	}
	h := w.Header()
	h.Set("Content-Type", contentType)
	h.Set("Content-Length", strconv.Itoa(len(b)))
	h.Set("Cache-Control", "no-store")
	if disposition != "" {
		h.Set("Content-Disposition", disposition)
	}
	w.Write(b)
}

func tokenFrom(r *http.Request) string {
	if tok := r.Header.Get("X-Token"); tok != "" {
		return tok
	}
	if a := r.Header.Get("Authorization"); strings.HasPrefix(a, "Bearer ") {
		return strings.TrimSpace(a[len("Bearer "):])
	}
	return r.URL.Query().Get("t")
}

func (s *Server) authed(w *respWriter, r *http.Request, path string) {
	tok := tokenFrom(r)
	who, ok := "", false
	if tok != "" {
		who, ok = s.reg.lookup(tok)
	}
	if !ok {
		if path == "/" && r.Method == http.MethodGet && strings.Contains(r.Header.Get("Accept"), "text/html") {
			http.Redirect(w, r, "/join", http.StatusFound) // someone opened the hub in a browser
			return
		}
		w.WriteHeader(http.StatusForbidden)
		return
	}
	w.who = who
	switch {
	case path == "/events" && r.Method == http.MethodGet:
		s.events(w, r, who, tok)
	case path == "/events":
		w.WriteHeader(http.StatusMethodNotAllowed)
	case r.Method == http.MethodGet:
		s.getClip(w)
	case r.Method == http.MethodPost:
		s.postClip(w, r)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *Server) getClip(w *respWriter) {
	clip, _ := s.store.Snapshot()
	if clip == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	http.NewResponseController(w).SetWriteDeadline(time.Now().Add(transferTime(len(clip.data), time.Minute)))
	h := w.Header()
	ct := clip.Mime
	if strings.HasPrefix(ct, "text/") {
		ct += "; charset=utf-8"
	}
	h.Set("Content-Type", ct)
	h.Set("Content-Length", strconv.Itoa(len(clip.data)))
	h.Set("X-Clip-Seq", strconv.FormatUint(clip.Seq, 10))
	h.Set("X-Clip-Sha", clip.SHA)
	h.Set("X-Clip-Source", clip.Source)
	if clip.Name != "" {
		escaped := url.PathEscape(clip.Name)
		h.Set("X-Clip-Name", escaped)
		h.Set("Content-Disposition", `attachment; filename="`+asciiOnly(clip.Name)+`"; filename*=UTF-8''`+escaped)
	}
	if clip.Bundle {
		h.Set("X-Clip-Bundle", "1")
	}
	w.Write(clip.data)
}

// transferTime is how long to allow for moving n bytes: a base for the
// round trips plus time at a slow 128 KiB/s, so big files survive poor links.
func transferTime(n int, base time.Duration) time.Duration {
	return base + time.Duration(n>>17)*time.Second
}

// asciiOnly is the fallback filename for old clients that ignore filename*.
func asciiOnly(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r > 0x7e || r == '"' || r == '\\' {
			return '_'
		}
		return r
	}, s)
}

// cleanName reads a filename an agent sent percent-encoded in a header. Agents
// sanitize it again before touching a disk; this keeps control characters and
// absurd lengths out of what the hub serves.
func cleanName(raw string) string {
	name, err := url.PathUnescape(raw)
	if err != nil {
		return ""
	}
	name = strings.TrimSpace(strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, name))
	for len(name) > 255 {
		_, n := utf8.DecodeLastRuneInString(name)
		name = name[:len(name)-n]
	}
	return name
}

func (s *Server) postClip(w *respWriter, r *http.Request) {
	if r.ContentLength > s.cfg.maxBytes {
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		return
	}
	if r.ContentLength > 0 { // a big upload on a slow link needs longer than the server-wide limit
		http.NewResponseController(w).SetReadDeadline(time.Now().Add(transferTime(int(r.ContentLength), 2*time.Minute)))
	}
	body := http.MaxBytesReader(w, r.Body, s.cfg.maxBytes)
	var data []byte
	var err error
	if r.ContentLength > 0 {
		data = make([]byte, r.ContentLength)
		_, err = io.ReadFull(body, data)
	} else {
		data, err = io.ReadAll(body)
	}
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			w.WriteHeader(http.StatusRequestEntityTooLarge)
		} else {
			w.WriteHeader(http.StatusBadRequest)
		}
		return
	}
	if len(data) == 0 {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	// A name makes the clip a file. Opaque bytes with no name, as the iPhone sends them, get one.
	mime, ext := sniff(data)
	name := cleanName(r.Header.Get("X-Filename"))
	if name == "" && isFileType(mime) {
		name = "clipboard-" + time.Now().UTC().Format("20060102-150405") + ext
	}
	bundle := name != "" && mime == "application/zip" && r.Header.Get("X-Clip-Bundle") == "1"
	clip, changed := s.store.Set(data, mime, w.who, name, bundle)
	if changed && len(data) > bigClip {
		go debug.FreeOSMemory() // Go keeps freed memory otherwise; a small device wants it back
	}
	w.Header().Set("X-Clip-Seq", strconv.FormatUint(clip.Seq, 10))
	w.Header().Set("X-Clip-Sha", clip.SHA)
	w.WriteHeader(http.StatusNoContent)
}

// events streams changes until the client leaves or its token is revoked.
// The token is checked again on every ping and change, so revoking a device
// cuts its open stream within one ping interval.
func (s *Server) events(w *respWriter, r *http.Request, who, tok string) {
	stillValid := func() bool { _, ok := s.reg.lookup(tok); return ok }
	rc := http.NewResponseController(w)
	send := func(format string, args ...any) error {
		rc.SetWriteDeadline(time.Now().Add(30 * time.Second))
		if _, err := fmt.Fprintf(w, format, args...); err != nil {
			return err
		}
		return rc.Flush()
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	clip, changed := s.store.Snapshot()
	hello, _ := json.Marshal(struct {
		You  string `json:"you"`
		Clip *Clip  `json:"clip"`
		Max  int64  `json:"max"` // the largest clip we accept, so agents need not try bigger ones
	}{who, clip, s.cfg.maxBytes})
	if send("event: hello\ndata: %s\n\n", hello) != nil {
		return
	}
	var last uint64
	if clip != nil {
		last = clip.Seq
	}
	tick := time.NewTicker(s.ping)
	defer tick.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-tick.C:
			if !stillValid() || send(": ping\n\n") != nil {
				return
			}
		case <-changed:
			clip, changed = s.store.Snapshot()
			if !stillValid() {
				return
			}
			if clip != nil && clip.Seq > last {
				last = clip.Seq
				meta, _ := json.Marshal(clip)
				if send("event: clip\ndata: %s\n\n", meta) != nil {
					return
				}
			}
		}
	}
}

// --- joining ------------------------------------------------------------------

var hostRE = regexp.MustCompile(`^([A-Za-z0-9.-]+|\[[0-9A-Fa-f:.]+\])(:[0-9]{1,5})?$`)

// baseURL is how this client reached the hub, so scripts and pages we hand out
// point back at the same address.
func (s *Server) baseURL(r *http.Request) (string, bool) {
	if s.cfg.publicURL != "" {
		return s.cfg.publicURL, true
	}
	if !hostRE.MatchString(r.Host) {
		return "", false
	}
	return "http://" + r.Host, true
}

func (s *Server) installScript(w *respWriter, r *http.Request) {
	hub, ok := s.baseURL(r)
	if !ok {
		http.Error(w, "bad Host header", http.StatusBadRequest)
		return
	}
	script, err := fs.ReadFile(s.files, "agent/join.sh")
	if err != nil {
		http.Error(w, "not in this build", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	io.WriteString(w, strings.ReplaceAll(string(script), "@HUB@", hub))
}

func passphraseOK(given, want string) bool {
	a, b := sha256.Sum256([]byte(given)), sha256.Sum256([]byte(want))
	return subtle.ConstantTimeCompare(a[:], b[:]) == 1
}

// enroll trades the join passphrase for a device token. It takes JSON or form
// fields (name, passphrase); the shell installer uses the latter.
func (s *Server) enroll(w *respWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if s.cfg.passphrase == "" {
		http.Error(w, "joining is disabled on this hub", http.StatusNotFound)
		return
	}
	ip := clientIP(r)
	if s.throttle.blocked(ip) {
		w.Header().Set("Retry-After", "300")
		http.Error(w, "too many wrong passphrases; wait a few minutes", http.StatusTooManyRequests)
		return
	}
	var req struct {
		Name       string `json:"name"`
		Passphrase string `json:"passphrase"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		if json.NewDecoder(r.Body).Decode(&req) != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
	} else {
		if r.ParseForm() != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		req.Name, req.Passphrase = r.PostFormValue("name"), r.PostFormValue("passphrase")
	}
	if !passphraseOK(req.Passphrase, s.cfg.passphrase) {
		s.throttle.fail(ip)
		time.Sleep(400 * time.Millisecond)
		http.Error(w, "wrong passphrase", http.StatusForbidden)
		return
	}
	hub, ok := s.baseURL(r)
	if !ok {
		http.Error(w, "bad Host header", http.StatusBadRequest)
		return
	}
	token, err := s.reg.issue(req.Name)
	switch {
	case errors.Is(err, errBadName):
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	case errors.Is(err, errNameTaken):
		http.Error(w, err.Error(), http.StatusConflict)
		return
	case err != nil:
		log.Printf("enroll %q: %v", req.Name, err)
		http.Error(w, "could not store the token", http.StatusInternalServerError)
		return
	}
	s.throttle.clear(ip)
	w.who = req.Name
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"name": req.Name, "token": token, "url": hub + "/?t=" + token})
}
