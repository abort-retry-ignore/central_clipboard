package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Clip is immutable once created. The JSON form (without the bytes) is the
// metadata sent to agents.
type Clip struct {
	Seq    uint64  `json:"seq"`
	Mime   string  `json:"mime"`
	Size   int     `json:"size"`
	SHA    string  `json:"sha"`
	Source string  `json:"source"`
	TS     float64 `json:"ts"`
	Name   string  `json:"name,omitempty"`   // set when the clip is a file
	Bundle bool    `json:"bundle,omitempty"` // a zip holding several files or a folder
	data   []byte
}

// Store holds the current clip. With dir set it also persists the clip so a
// restart keeps it; leave dir empty on flash-backed devices to keep it in RAM only.
type Store struct {
	mu      sync.Mutex
	clip    *Clip
	changed chan struct{} // closed, then replaced, on every change
	dir     string
}

func NewStore(dir string) *Store {
	s := &Store{changed: make(chan struct{}), dir: dir}
	s.clip = s.load()
	return s
}

func (s *Store) load() *Clip {
	if s.dir == "" {
		return nil
	}
	meta, err := os.ReadFile(filepath.Join(s.dir, "current.json"))
	if err != nil {
		return nil
	}
	data, err := os.ReadFile(filepath.Join(s.dir, "current.bin"))
	if err != nil {
		return nil
	}
	var c Clip
	if json.Unmarshal(meta, &c) != nil || shaHex(data) != c.SHA {
		return nil
	}
	c.data = data
	return &c
}

func (s *Store) persist(c *Clip) {
	if s.dir == "" {
		return
	}
	meta, _ := json.Marshal(c)
	for name, content := range map[string][]byte{"current.bin": c.data, "current.json": meta} {
		tmp := filepath.Join(s.dir, name+".tmp")
		if err := os.WriteFile(tmp, content, 0o600); err != nil {
			log.Printf("persisting clip: %v", err)
			return
		}
		if err := os.Rename(tmp, filepath.Join(s.dir, name)); err != nil {
			log.Printf("persisting clip: %v", err)
			return
		}
	}
}

// Snapshot returns the current clip (nil when empty) and a channel that is
// closed when the clip next changes.
func (s *Store) Snapshot() (*Clip, <-chan struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.clip, s.changed
}

// Set replaces the clip. The same bytes under the same name are a no-op and report false.
func (s *Store) Set(data []byte, mime, source, name string, bundle bool) (*Clip, bool) {
	sha := shaHex(data)
	s.mu.Lock()
	defer s.mu.Unlock()
	if c := s.clip; c != nil && c.SHA == sha && c.Name == name && c.Bundle == bundle {
		return c, false
	}
	var seq uint64 = 1
	if s.clip != nil {
		seq = s.clip.Seq + 1
	}
	c := &Clip{Seq: seq, Mime: mime, Size: len(data), SHA: sha, Source: source, Name: name, Bundle: bundle,
		TS: float64(time.Now().UnixNano()) / 1e9, data: data}
	s.persist(c)
	s.clip = c
	close(s.changed)
	s.changed = make(chan struct{})
	return c, true
}

func shaHex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
