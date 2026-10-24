package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"mime"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const streamIdle = 45 * time.Second // the hub pings every 15 s

type clipMeta struct {
	Seq    uint64 `json:"seq"`
	Mime   string `json:"mime"`
	Size   int    `json:"size"`
	SHA    string `json:"sha"`
	Source string `json:"source"`
}

// Agent mirrors the local clipboard to the hub and back.
//
// On start it adopts the hub's clip (the hub is the source of truth); after
// that the last writer wins. A copy that could not be pushed is pushed again
// when the connection returns.
type Agent struct {
	url, token string
	cb         Clipboard
	stage      string // where received files wait to be pasted

	mu       sync.Mutex
	me       string // our device name, as the hub knows us
	max      int64  // the largest clip the hub accepts; 0 until it tells us
	localSHA string // sha of the content we last put on, or read from, the local clipboard
	hubSHA   string // sha of the hub clip we last pushed or applied
	dirty    bool   // a local copy has not reached the hub

	ready     chan struct{} // closed after the first hello
	readyOnce sync.Once
}

func NewAgent(url, token string, cb Clipboard) *Agent {
	return &Agent{url: url, token: token, cb: cb, stage: stageRoot(), ready: make(chan struct{})}
}

// do makes one request. size is how many bytes will move, so a big file on a
// slow link gets longer than a short text does; extra is header name, value pairs.
func (a *Agent) do(ctx context.Context, method, path string, body []byte, size int, extra ...string) (*http.Response, error) {
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, a.url+path, r)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Token", a.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/octet-stream")
	}
	for i := 0; i+1 < len(extra); i += 2 {
		req.Header.Set(extra[i], extra[i+1])
	}
	timeout := 30*time.Second + time.Duration(size>>17)*time.Second // plus time at 128 KiB/s
	return (&http.Client{Timeout: timeout}).Do(req)
}

// ownFiles reports whether paths are files we staged ourselves, which means a
// clipboard change we caused and must not send back.
func (a *Agent) ownFiles(paths []string) bool {
	for _, p := range paths {
		if !strings.HasPrefix(filepath.Clean(p), a.stage+string(filepath.Separator)) {
			return false
		}
	}
	return len(paths) > 0
}

// pushLocal sends the local clipboard to the hub unless we already know it.
func (a *Agent) pushLocal(ctx context.Context) {
	a.mu.Lock()
	defer a.mu.Unlock()
	c, ok := a.cb.Read()
	if !ok {
		return
	}
	data, what := c.Data, c.Mime
	var extra []string
	if len(c.Files) > 0 {
		if a.ownFiles(c.Files) {
			return
		}
		p, err := packFiles(c.Files, a.max)
		if err != nil {
			log.Printf("not sending the copied files: %v", err)
			return
		}
		data, what = p.data, "file "+p.name
		extra = []string{"X-Filename", url.PathEscape(p.name)}
		if p.bundle {
			extra = append(extra, "X-Clip-Bundle", "1")
		}
	}
	sha := shaHex(data)
	if sha == a.localSHA { // our own write echoing back, or a repeat copy
		return
	}
	resp, err := a.do(ctx, http.MethodPost, "/clip", data, len(data), extra...)
	if err != nil {
		log.Printf("push failed, will retry on reconnect: %v", err)
		a.dirty = true
		return
	}
	resp.Body.Close()
	switch {
	case resp.StatusCode >= 500:
		log.Printf("push failed (HTTP %d), will retry on reconnect", resp.StatusCode)
		a.dirty = true
	case resp.StatusCode >= 400:
		log.Printf("hub rejected %s (%d bytes): HTTP %d", what, len(data), resp.StatusCode)
	default:
		a.localSHA, a.hubSHA, a.dirty = sha, sha, false
		log.Printf("pushed %s, %d bytes", what, len(data))
	}
}

// apply fetches the hub's clip and puts it on the local clipboard.
func (a *Agent) apply(ctx context.Context, m clipMeta) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if m.SHA == a.localSHA || m.SHA == a.hubSHA {
		return
	}
	resp, err := a.do(ctx, http.MethodGet, "/clip", nil, m.Size)
	if err != nil {
		log.Printf("fetching clip failed: %v", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return
	}
	if resp.ContentLength > agentClipCap {
		a.hubSHA = m.SHA
		log.Printf("ignoring a %d byte clip from %s: more than this machine takes", resp.ContentLength, m.Source)
		return
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, agentClipCap+1))
	if err != nil {
		log.Printf("fetching clip failed: %v", err)
		return
	}
	sum := shaHex(data)
	if want := resp.Header.Get("X-Clip-Sha"); want != "" && want != sum {
		log.Printf("clip from %s arrived damaged, ignoring it", m.Source)
		return
	}
	a.hubSHA = sum
	hubMime, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if name, _ := url.PathUnescape(resp.Header.Get("X-Clip-Name")); name != "" || isOpaque(hubMime) {
		a.applyFiles(m.Source, name, resp.Header.Get("X-Clip-Bundle") == "1", data, sum)
		return
	}
	cmime, cdata, ok := forClipboard(hubMime, data)
	if !ok {
		log.Printf("ignoring %s clip from %s", hubMime, m.Source)
		return
	}
	a.localSHA = shaHex(cdata) // before writing, so the watcher skips the echo
	if err := a.cb.Write(cmime, cdata); err != nil {
		log.Printf("writing clipboard failed: %v", err)
		a.localSHA, a.hubSHA = "", ""
		return
	}
	log.Printf("applied %s, %d bytes from %s", cmime, len(cdata), m.Source)
}

// applyFiles saves a file clip where a file manager can paste it from, and
// puts it on the clipboard as a copy of those files.
func (a *Agent) applyFiles(source, name string, bundle bool, data []byte, sum string) {
	paths, err := stage(a.stage, sum[:16], name, data, bundle)
	if err != nil {
		log.Printf("saving files from %s failed: %v", source, err)
		a.hubSHA = ""
		return
	}
	a.localSHA = sum // before writing, so the watcher skips the echo
	if err := a.cb.WriteFiles(paths); err != nil {
		log.Printf("writing clipboard failed: %v", err)
		a.localSHA, a.hubSHA = "", ""
		return
	}
	log.Printf("applied file %s, %d bytes from %s", filepath.Base(paths[0]), len(data), source)
}

// isOpaque reports whether a clip of this type can only be a file: it is not
// text, a picture or rich text, which are pasted directly.
func isOpaque(mime string) bool {
	return mime != "" && !strings.HasPrefix(mime, "text/") && !strings.HasPrefix(mime, "image/") && mime != "application/rtf"
}

func (a *Agent) onEvent(ctx context.Context, event string, payload []byte) {
	switch event {
	case "hello":
		var h struct {
			You  string    `json:"you"`
			Max  int64     `json:"max"`
			Clip *clipMeta `json:"clip"`
		}
		if json.Unmarshal(payload, &h) != nil {
			return
		}
		a.mu.Lock()
		a.me, a.max = h.You, h.Max
		dirty := a.dirty
		a.mu.Unlock()
		if dirty {
			a.pushLocal(ctx) // a copy made while offline is newer than the hub's
		} else if h.Clip != nil {
			a.apply(ctx, *h.Clip)
		}
		a.readyOnce.Do(func() { close(a.ready) })
	case "clip":
		var m clipMeta
		if json.Unmarshal(payload, &m) != nil {
			return
		}
		a.mu.Lock()
		own := m.Source == a.me
		a.mu.Unlock()
		if !own {
			a.apply(ctx, m)
		}
	}
}

// stream holds the event connection open, reconnecting with backoff.
func (a *Agent) stream(ctx context.Context) {
	delay := time.Second
	for ctx.Err() == nil {
		_ = a.streamOnce(ctx, func() { delay = time.Second }) // failures are logged inside
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		if delay *= 2; delay > 30*time.Second {
			delay = 30 * time.Second
		}
	}
}

func (a *Agent) streamOnce(parent context.Context, connected func()) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	idle := time.AfterFunc(streamIdle, cancel)
	defer idle.Stop()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.url+"/events", nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-Token", a.token)
	resp, err := (&http.Client{}).Do(req) // no overall timeout: the stream is long-lived
	if err != nil {
		log.Printf("hub connection lost: %v", err)
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		log.Printf("hub refused the event stream: HTTP %d", resp.StatusCode)
		return io.ErrUnexpectedEOF
	}
	log.Print("connected to hub")
	var event string
	var data []byte
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		idle.Reset(streamIdle)
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			event = line[len("event: "):]
		case strings.HasPrefix(line, "data: "):
			data = []byte(line[len("data: "):])
		case line == "" && event != "":
			if event == "hello" {
				connected()
			}
			a.onEvent(ctx, event, data)
			event, data = "", nil
		}
	}
	log.Print("hub closed the stream")
	return sc.Err()
}

// watch pushes local copies. It waits briefly for the hub's first message so a
// stale local clipboard never overwrites the hub's clip at start-up.
func (a *Agent) watch(ctx context.Context) {
	select {
	case <-a.ready:
	case <-time.After(10 * time.Second): // hub unreachable: carry on, copies go out on reconnect
	case <-ctx.Done():
		return
	}
	for ctx.Err() == nil {
		if err := a.cb.Watch(ctx, func() { a.pushLocal(ctx) }); err != nil && ctx.Err() == nil {
			log.Printf("clipboard watcher stopped: %v", err)
		}
		select {
		case <-ctx.Done():
		case <-time.After(2 * time.Second): // no compositor yet, or it restarted
		}
	}
}

// Run blocks until ctx ends.
func (a *Agent) Run(ctx context.Context) {
	go a.watch(ctx)
	a.stream(ctx)
}
