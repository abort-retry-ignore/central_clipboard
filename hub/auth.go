package main

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	minTokenLen      = 16
	minPassphraseLen = 10
)

var (
	nameRE       = regexp.MustCompile(`^[A-Za-z0-9_-]{1,32}$`)
	errBadName   = errors.New("name must be 1-32 characters: letters, digits, - or _")
	errNameTaken = errors.New("that name belongs to a fixed token")
)

func hashToken(t string) string {
	sum := sha256.Sum256([]byte(t))
	return hex.EncodeToString(sum[:])
}

// parseTokens reads "phone:abc...,laptop:def..." into token -> name.
// A bare token gets a numbered name.
func parseTokens(spec string) (map[string]string, error) {
	out := map[string]string{}
	for i, item := range strings.Split(spec, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		name, tok := "", item
		if cut := strings.LastIndex(item, ":"); cut >= 0 {
			name, tok = item[:cut], item[cut+1:]
		}
		if len(tok) < minTokenLen {
			return nil, fmt.Errorf("CLIP_TOKENS: token #%d is shorter than %d characters", i+1, minTokenLen)
		}
		if name == "" {
			name = fmt.Sprintf("device%d", i+1)
		}
		out[tok] = name
	}
	return out, nil
}

type tokenEntry struct {
	SHA256  string `json:"sha256"`
	Created string `json:"created"`
}

type fileStamp struct {
	mod, size int64
	ok        bool
}

// Registry answers "whose token is this?". Fixed tokens come from the
// environment; enrolled ones live (hashed) in a small file that is re-read
// whenever it changes, so `hub revoke` takes effect without a restart.
type Registry struct {
	mu      sync.Mutex
	static  map[string]string // sha256 -> name
	fixed   map[string]bool   // names of fixed tokens
	path    string            // tokens.json; "" means enrolled tokens are not kept
	stamp   fileStamp
	dynamic map[string]tokenEntry // name -> entry
}

func NewRegistry(static map[string]string, path string) *Registry {
	r := &Registry{static: map[string]string{}, fixed: map[string]bool{}, path: path,
		dynamic: map[string]tokenEntry{}}
	for tok, name := range static {
		r.static[hashToken(tok)] = name
		r.fixed[name] = true
	}
	return r
}

func (r *Registry) refresh() {
	if r.path == "" {
		return
	}
	var st fileStamp
	if fi, err := os.Stat(r.path); err == nil {
		st = fileStamp{fi.ModTime().UnixNano(), fi.Size(), true}
	}
	if st == r.stamp {
		return
	}
	r.stamp = st
	r.dynamic = map[string]tokenEntry{}
	if !st.ok {
		return
	}
	if b, err := os.ReadFile(r.path); err == nil {
		_ = json.Unmarshal(b, &r.dynamic)
	}
}

func (r *Registry) save() error {
	if r.path == "" {
		return errors.New("no data directory for enrolled tokens")
	}
	b, _ := json.MarshalIndent(r.dynamic, "", "  ")
	tmp := r.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, r.path); err != nil {
		return err
	}
	r.stamp = fileStamp{}
	r.refresh()
	return nil
}

// lookup returns the device name for a token.
func (r *Registry) lookup(token string) (string, bool) {
	h := []byte(hashToken(token))
	r.mu.Lock()
	defer r.mu.Unlock()
	r.refresh()
	for known, name := range r.static {
		if subtle.ConstantTimeCompare(h, []byte(known)) == 1 {
			return name, true
		}
	}
	for name, e := range r.dynamic {
		if subtle.ConstantTimeCompare(h, []byte(e.SHA256)) == 1 {
			return name, true
		}
	}
	return "", false
}

// issue creates a token for name. Enrolling a name again replaces its token.
func (r *Registry) issue(name string) (string, error) {
	if !nameRE.MatchString(name) {
		return "", errBadName
	}
	if r.fixed[name] {
		return "", errNameTaken
	}
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	token := hex.EncodeToString(raw)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.refresh()
	r.dynamic[name] = tokenEntry{SHA256: hashToken(token), Created: time.Now().UTC().Format(time.RFC3339)}
	if err := r.save(); err != nil {
		return "", err
	}
	return token, nil
}

func (r *Registry) revoke(name string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.refresh()
	if _, ok := r.dynamic[name]; !ok {
		return false, nil
	}
	delete(r.dynamic, name)
	return true, r.save()
}

type deviceInfo struct{ Name, Kind, Created string }

func (r *Registry) list() []deviceInfo {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.refresh()
	var out []deviceInfo
	for _, name := range r.static {
		out = append(out, deviceInfo{name, "fixed", "-"})
	}
	for name, e := range r.dynamic {
		out = append(out, deviceInfo{name, "enrolled", e.Created})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (r *Registry) count() int { return len(r.list()) }

// throttle limits failed passphrase attempts per client address.
type throttle struct {
	mu     sync.Mutex
	fails  map[string][]time.Time
	limit  int
	window time.Duration
}

func newThrottle(limit int, window time.Duration) *throttle {
	return &throttle{fails: map[string][]time.Time{}, limit: limit, window: window}
}

func (t *throttle) recent(ip string) []time.Time {
	cutoff := time.Now().Add(-t.window)
	var keep []time.Time
	for _, ts := range t.fails[ip] {
		if ts.After(cutoff) {
			keep = append(keep, ts)
		}
	}
	t.fails[ip] = keep
	return keep
}

func (t *throttle) blocked(ip string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.recent(ip)) >= t.limit
}

func (t *throttle) fail(ip string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.fails[ip] = append(t.recent(ip), time.Now())
}

func (t *throttle) clear(ip string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.fails, ip)
}
