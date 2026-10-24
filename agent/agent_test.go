package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// fakeClipboard stands in for wl-clipboard.
type fakeClipboard struct {
	mu     sync.Mutex
	mime   string
	data   []byte
	files  []string
	ok     bool
	writes int
}

func (f *fakeClipboard) set(mime, data string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mime, f.data, f.files, f.ok = mime, []byte(data), nil, true
}

// setFiles is a copy in a file manager.
func (f *fakeClipboard) setFiles(paths ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mime, f.data, f.files, f.ok = "", nil, paths, true
}

func (f *fakeClipboard) Read() (Content, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return Content{Mime: f.mime, Data: f.data, Files: f.files}, f.ok
}

func (f *fakeClipboard) Write(mime string, data []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mime, f.data, f.files, f.ok = mime, data, nil, true
	f.writes++
	return nil
}

func (f *fakeClipboard) WriteFiles(paths []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mime, f.data, f.files, f.ok = "", nil, paths, true
	f.writes++
	return nil
}

func (f *fakeClipboard) Watch(ctx context.Context, changed func()) error {
	<-ctx.Done()
	return nil
}

func (f *fakeClipboard) text() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return string(f.data)
}

// stubHub implements just enough of the hub for the agent's request/response calls.
type stubHub struct {
	mu     sync.Mutex
	srv    *httptest.Server
	clip   []byte
	mime   string
	name   string // as the hub reports a file, and as the last upload declared it
	bundle bool
	badSHA bool // serve the clip with a checksum that does not match
	posts  int
	hdr    http.Header // headers of the last upload
	down   bool
}

func newStubHub(t *testing.T) *stubHub {
	h := &stubHub{mime: "text/plain"}
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		defer h.mu.Unlock()
		if r.Header.Get("X-Token") != "tok" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		switch {
		case h.down:
			w.WriteHeader(http.StatusServiceUnavailable)
		case r.Method == http.MethodPost:
			h.clip, _ = io.ReadAll(r.Body)
			h.posts++
			h.hdr = r.Header.Clone()
			h.name, _ = url.PathUnescape(r.Header.Get("X-Filename"))
			h.bundle = r.Header.Get("X-Clip-Bundle") == "1"
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodGet && h.clip != nil:
			w.Header().Set("Content-Type", h.mime)
			w.Header().Set("X-Clip-Sha", shaHex(h.clip))
			if h.badSHA {
				w.Header().Set("X-Clip-Sha", shaHex([]byte("something else")))
			}
			if h.name != "" {
				w.Header().Set("X-Clip-Name", url.PathEscape(h.name))
			}
			if h.bundle {
				w.Header().Set("X-Clip-Bundle", "1")
			}
			w.Write(h.clip)
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	t.Cleanup(h.srv.Close)
	return h
}

func (h *stubHub) set(mime, data string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.mime, h.clip, h.name, h.bundle = mime, []byte(data), "", false
}

// setFile is a file clip, as the hub serves one.
func (h *stubHub) setFile(mime string, data []byte, name string, bundle bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.mime, h.clip, h.name, h.bundle = mime, data, name, bundle
}

// uploaded is what the last POST declared about itself.
func (h *stubHub) uploaded() (name string, bundle bool, body []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.name, h.bundle, h.clip
}

func (h *stubHub) state() (string, int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return string(h.clip), h.posts
}

func newAgentWith(t *testing.T) (*Agent, *fakeClipboard, *stubHub) {
	h := newStubHub(t)
	cb := &fakeClipboard{}
	a := NewAgent(h.srv.URL, "tok", cb)
	a.stage = filepath.Join(t.TempDir(), "files") // never the real cache
	return a, cb, h
}

func helloWith(h *stubHub, you, source string) []byte {
	clip, _ := h.state()
	if clip == "" {
		return []byte(`{"you":"` + you + `","clip":null}`)
	}
	return []byte(`{"you":"` + you + `","clip":{"seq":1,"mime":"text/plain","sha":"` + shaHex([]byte(clip)) + `","source":"` + source + `"}}`)
}

func clipEvent(h *stubHub, source string) []byte {
	clip, _ := h.state()
	return []byte(`{"seq":2,"mime":"text/plain","sha":"` + shaHex([]byte(clip)) + `","source":"` + source + `"}`)
}

func TestPushSendsOnceAndSkipsOwnEcho(t *testing.T) {
	ctx := context.Background()
	a, cb, h := newAgentWith(t)

	cb.set("text/plain", "hello")
	a.pushLocal(ctx)
	a.pushLocal(ctx) // same content again: nothing to send
	if got, posts := h.state(); got != "hello" || posts != 1 {
		t.Fatalf("hub has %q after %d posts", got, posts)
	}

	// another machine copies something; we apply it, and the watcher then sees our own write
	h.set("text/plain", "from elsewhere")
	a.onEvent(ctx, "clip", clipEvent(h, "laptop2"))
	if cb.text() != "from elsewhere" {
		t.Fatalf("clipboard = %q", cb.text())
	}
	a.pushLocal(ctx)
	if _, posts := h.state(); posts != 1 {
		t.Fatalf("our own write was pushed back (%d posts)", posts)
	}
}

func TestNothingToPushWhenClipboardIsEmptyOrSecret(t *testing.T) {
	a, _, h := newAgentWith(t) // fake Read reports ok=false
	a.pushLocal(context.Background())
	if _, posts := h.state(); posts != 0 {
		t.Fatalf("%d posts", posts)
	}
}

func TestHelloAdoptsHubClipAndOwnEventsAreIgnored(t *testing.T) {
	ctx := context.Background()
	a, cb, h := newAgentWith(t)
	h.set("text/plain", "hub clip")
	a.onEvent(ctx, "hello", helloWith(h, "laptop1", "phone"))
	if cb.text() != "hub clip" {
		t.Fatalf("hello did not apply the hub clip: %q", cb.text())
	}
	select {
	case <-a.ready:
	default:
		t.Fatal("not marked ready after hello")
	}

	h.set("text/plain", "my own copy, echoed")
	a.onEvent(ctx, "clip", clipEvent(h, "laptop1"))
	if cb.text() != "hub clip" {
		t.Fatalf("applied an event from ourselves: %q", cb.text())
	}
	a.onEvent(ctx, "clip", clipEvent(h, "phone"))
	if cb.text() != "my own copy, echoed" {
		t.Fatalf("did not apply an event from another device: %q", cb.text())
	}
}

func TestSameContentIsNotAppliedTwice(t *testing.T) {
	ctx := context.Background()
	a, cb, h := newAgentWith(t)
	h.set("text/plain", "once")
	ev := clipEvent(h, "phone")
	a.onEvent(ctx, "clip", ev)
	a.onEvent(ctx, "hello", helloWith(h, "laptop1", "phone")) // e.g. after a reconnect
	if cb.writes != 1 {
		t.Fatalf("wrote the clipboard %d times", cb.writes)
	}
}

func TestOfflineCopyIsPushedOnReconnect(t *testing.T) {
	ctx := context.Background()
	a, cb, h := newAgentWith(t)

	h.mu.Lock()
	h.down = true
	h.mu.Unlock()
	cb.set("text/plain", "copied offline")
	a.pushLocal(ctx)
	if !a.dirty {
		t.Fatal("failed push not remembered")
	}

	h.mu.Lock()
	h.down = false
	h.clip = []byte("hub moved on meanwhile")
	h.mu.Unlock()
	a.onEvent(ctx, "hello", helloWith(h, "laptop1", "phone"))
	if got, _ := h.state(); got != "copied offline" {
		t.Fatalf("hub has %q; the offline copy should win", got)
	}
	if a.dirty {
		t.Fatal("still dirty after a successful push")
	}
}

func TestRevokedTokenIsNotRetried(t *testing.T) {
	a, cb, h := newAgentWith(t)
	a.token = "revoked"
	cb.set("text/plain", "x")
	a.pushLocal(context.Background())
	if a.dirty {
		t.Fatal("a 403 should not be queued for retry")
	}
	if _, posts := h.state(); posts != 0 {
		t.Fatalf("%d posts", posts)
	}
}

func TestRTFFromThePhoneIsPastedAsText(t *testing.T) {
	ctx := context.Background()
	a, cb, h := newAgentWith(t)
	h.set("application/rtf", `{\rtf1\ansi{\fonttbl{\f0 Helvetica;}}\f0 Hello world}`)
	a.onEvent(ctx, "clip", clipEvent(h, "phone"))
	if cb.mime != "text/plain" || cb.text() != "Hello world" {
		t.Fatalf("clipboard = %s %q", cb.mime, cb.text())
	}
}

// --- conversions --------------------------------------------------------------------

func TestRTFToText(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"words and paragraphs", `{\rtf1\ansi{\fonttbl{\f0 Helvetica;}}\f0 one\par two}`, "one\ntwo"},
		{"unicode escape", `{\rtf1\ansi Hello \` + `u8212? world}`, "Hello — world"},
		{"hex escape", `{\rtf1\ansi caf\'e9}`, "café"},
		{"cp1252 hex", `{\rtf1\ansi \'93quoted\'94}`, "“quoted”"},
		{"escaped braces", `{\rtf1 a \{b\} c}`, "a {b} c"},
		{"hidden destinations", `{\rtf1{\*\generator Hidden;}{\colortbl;\red0;}shown}`, "shown"},
		{"nothing", `{\rtf1\ansi{\fonttbl{\f0 Helvetica;}}}`, ""},
	} {
		if got := rtfToText([]byte(tc.in)); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

func shim(t *testing.T, name, script string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
}

func TestForClipboard(t *testing.T) {
	heic := []byte("\x00\x00\x00\x18ftypheic....")

	m, d, ok := forClipboard("text/plain", []byte("hi"))
	if !ok || m != "text/plain" || string(d) != "hi" {
		t.Fatalf("text: %s %q %v", m, d, ok)
	}
	m, d, ok = forClipboard("image/png", []byte("png"))
	if !ok || m != "image/png" || string(d) != "png" {
		t.Fatalf("png: %s %q %v", m, d, ok)
	}
	if _, _, ok = forClipboard("application/octet-stream", []byte("x")); ok {
		t.Fatal("binary blob accepted")
	}
	if _, _, ok = forClipboard("application/rtf", []byte(`{\rtf1\ansi}`)); ok {
		t.Fatal("empty RTF accepted")
	}

	t.Setenv("PATH", t.TempDir()) // no ImageMagick
	if _, _, ok = forClipboard("image/heic", heic); ok {
		t.Fatal("HEIC accepted without a converter")
	}
	if m, _, ok = forClipboard("image/jpeg", []byte("jpg")); !ok || m != "image/jpeg" {
		t.Fatalf("jpeg should pass through: %s %v", m, ok)
	}

	shim(t, "magick", `printf PNGDATA`)
	m, d, ok = forClipboard("image/heic", heic)
	if !ok || m != "image/png" || string(d) != "PNGDATA" {
		t.Fatalf("heic with converter: %s %q %v", m, d, ok)
	}
}

func TestWlEnvFindsTheCompositorSocket(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "wayland-1"), nil, 0o600)
	os.WriteFile(filepath.Join(dir, "wayland-1.lock"), nil, 0o600)
	t.Setenv("XDG_RUNTIME_DIR", dir)
	t.Setenv("WAYLAND_DISPLAY", "")
	found := false
	for _, kv := range wlEnv() {
		if kv == "WAYLAND_DISPLAY=wayland-1" {
			found = true
		}
		if strings.HasSuffix(kv, ".lock") {
			t.Fatalf("picked a lock file: %s", kv)
		}
	}
	if !found {
		t.Fatal("did not discover wayland-1")
	}
}

// --- files --------------------------------------------------------------------------

func TestCopiedFileIsSentWithItsName(t *testing.T) {
	ctx := context.Background()
	a, cb, h := newAgentWith(t)
	p := write(t, filepath.Join(t.TempDir(), "my report é.pdf"), "%PDF-1.7 body")

	cb.setFiles(p)
	a.pushLocal(ctx)
	a.pushLocal(ctx) // the same copy again changes nothing
	name, bundle, body := h.uploaded()
	if _, posts := h.state(); posts != 1 || name != "my report é.pdf" || bundle || string(body) != "%PDF-1.7 body" {
		t.Fatalf("posts=%d name=%q bundle=%v body=%q", posts, name, bundle, body)
	}
	if got := h.hdr.Get("X-Filename"); got != "my%20report%20%C3%A9.pdf" {
		t.Fatalf("X-Filename = %q, want it percent-encoded", got)
	}
}

func TestSeveralCopiedFilesAreSentAsABundle(t *testing.T) {
	a, cb, h := newAgentWith(t)
	folder, loose := tree(t)
	cb.setFiles(folder, loose)
	a.pushLocal(context.Background())
	name, bundle, body := h.uploaded()
	if !bundle || name != "clipboard-files.zip" || !bytes.HasPrefix(body, []byte("PK\x03\x04")) {
		t.Fatalf("name=%q bundle=%v body=%q…", name, bundle, body[:min(len(body), 4)])
	}
}

func TestFilesTheHubCannotTakeAreNotSent(t *testing.T) {
	a, cb, h := newAgentWith(t)
	a.onEvent(context.Background(), "hello", []byte(`{"you":"laptop1","clip":null,"max":10}`))
	cb.setFiles(write(t, filepath.Join(t.TempDir(), "big.bin"), strings.Repeat("x", 100)))
	a.pushLocal(context.Background())
	if _, posts := h.state(); posts != 0 {
		t.Fatalf("%d posts of a file over the hub's limit", posts)
	}
	if a.dirty {
		t.Fatal("an oversized file must not be queued for retry")
	}
	cb.setFiles(write(t, filepath.Join(t.TempDir(), "small.txt"), "ok"))
	a.pushLocal(context.Background())
	if _, posts := h.state(); posts != 1 {
		t.Fatalf("a small file was not sent (%d posts)", posts)
	}
}

func TestFileFromAnotherMachineIsOfferedAsAFileCopy(t *testing.T) {
	ctx := context.Background()
	a, cb, h := newAgentWith(t)
	h.setFile("application/pdf", []byte("%PDF-1.7 from afar"), "invoice.pdf", false)
	a.onEvent(ctx, "clip", clipEvent(h, "laptop2"))

	if len(cb.files) != 1 || filepath.Base(cb.files[0]) != "invoice.pdf" || !strings.HasPrefix(cb.files[0], a.stage+"/") {
		t.Fatalf("clipboard files = %q", cb.files)
	}
	if read(t, cb.files[0]) != "%PDF-1.7 from afar" {
		t.Fatal("staged file differs")
	}
	if cb.mime != "" {
		t.Fatalf("also put %s on the clipboard", cb.mime)
	}

	// the clipboard change we just made is not a new copy
	a.pushLocal(ctx)
	if _, posts := h.state(); posts != 0 {
		t.Fatalf("our own staged files were sent back (%d posts)", posts)
	}
	a.onEvent(ctx, "hello", helloWith(h, "laptop1", "laptop2")) // and a reconnect does not redo it
	if cb.writes != 1 {
		t.Fatalf("wrote the clipboard %d times", cb.writes)
	}
}

func TestUnnamedFileFromThePhoneStillBecomesAFile(t *testing.T) {
	a, cb, h := newAgentWith(t) // an older hub, or a persisted clip, with no name
	h.setFile("application/octet-stream", []byte("\x00\x01 opaque"), "", false)
	a.onEvent(context.Background(), "clip", clipEvent(h, "phone"))
	if len(cb.files) != 1 || filepath.Base(cb.files[0]) != "clipboard-file" || read(t, cb.files[0]) != "\x00\x01 opaque" {
		t.Fatalf("clipboard files = %q", cb.files)
	}
}

func TestANamedTextClipStaysAFile(t *testing.T) {
	a, cb, h := newAgentWith(t)
	h.setFile("text/plain", []byte("just words"), "notes.txt", false)
	a.onEvent(context.Background(), "clip", clipEvent(h, "laptop2"))
	if len(cb.files) != 1 || filepath.Base(cb.files[0]) != "notes.txt" || read(t, cb.files[0]) != "just words" || cb.mime != "" {
		t.Fatalf("files=%q mime=%q", cb.files, cb.mime)
	}
}

func TestBundleFromTheHubIsUnpacked(t *testing.T) {
	a, cb, h := newAgentWith(t)
	folder, loose := tree(t)
	p, err := packFiles([]string{folder, loose}, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	h.setFile("application/zip", p.data, p.name, true)
	a.onEvent(context.Background(), "clip", clipEvent(h, "laptop2"))
	if len(cb.files) != 2 || filepath.Base(cb.files[0]) != "Photos" || read(t, cb.files[0]+"/sub/b.bin") != "\x00\x01bravo" || read(t, cb.files[1]) != "charlie" {
		t.Fatalf("clipboard files = %q", cb.files)
	}
}

func TestAPlainZipFileIsNotUnpacked(t *testing.T) {
	a, cb, h := newAgentWith(t)
	z := makeZip(t, [3]string{"inside.txt", "x", ""})
	h.setFile("application/zip", z, "archive.zip", false)
	a.onEvent(context.Background(), "clip", clipEvent(h, "laptop2"))
	if len(cb.files) != 1 || filepath.Base(cb.files[0]) != "archive.zip" || !bytes.Equal([]byte(read(t, cb.files[0])), z) {
		t.Fatalf("clipboard files = %q", cb.files)
	}
}

func TestDamagedOrHostileClipsAreIgnored(t *testing.T) {
	ctx := context.Background()
	a, cb, h := newAgentWith(t)
	h.setFile("application/pdf", []byte("%PDF- truncated in transit"), "doc.pdf", false)
	h.badSHA = true
	a.onEvent(ctx, "clip", clipEvent(h, "laptop2"))
	if cb.writes != 0 {
		t.Fatal("applied a clip whose checksum did not match")
	}

	h.badSHA = false
	h.setFile("application/zip", makeZip(t, [3]string{"../evil.txt", "x", ""}), "x.zip", true)
	a.onEvent(ctx, "clip", clipEvent(h, "laptop2"))
	if cb.writes != 0 {
		t.Fatal("applied a bundle that tries to leave the staging directory")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(a.stage), "evil.txt")); err == nil {
		t.Fatal("a file was written outside the staging directory")
	}
}

func TestTextStillWinsWhenNoFilesWereCopied(t *testing.T) {
	a, cb, h := newAgentWith(t)
	cb.set("text/plain", "plain words")
	a.pushLocal(context.Background())
	name, bundle, body := h.uploaded()
	if string(body) != "plain words" || name != "" || bundle {
		t.Fatalf("name=%q bundle=%v body=%q", name, bundle, body)
	}
	if h.hdr.Get("X-Filename") != "" {
		t.Fatal("text was sent as a file")
	}
}

// --- the wl-clipboard layer -----------------------------------------------------------

// shims puts fake commands in front of PATH.
func shims(t *testing.T, scripts map[string]string) {
	t.Helper()
	dir := t.TempDir()
	for name, script := range scripts {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
}

// fakeWlPaste answers for a clipboard offering types[i] with payloads[i].
func fakeWlPaste(types, payloads []string) string {
	script := "case \"$*\" in\n\"-l\") printf '%s\\n' " + strings.Join(types, " ") + " ;;\n"
	for i, ty := range types {
		script += "\"-n -t " + ty + "\") printf '%s' '" + payloads[i] + "' ;;\n"
	}
	return script + "esac"
}

func TestFileManagerCopyIsReadAsFilesNotAsItsPathText(t *testing.T) {
	// what Nautilus and Flea offer: the standard file formats, and the path as text
	shims(t, map[string]string{"wl-paste": fakeWlPaste(
		[]string{"application/x-flea-clip", "x-special/gnome-copied-files", "text/uri-list", "text/plain"},
		[]string{"copy 0123 99", "copy\nfile:///home/igor/a%20b.txt", "file:///home/igor/a%20b.txt\r\n", "/home/igor/a b.txt"})})
	c, ok := wlClipboard{}.Read()
	if !ok || !reflect.DeepEqual(c.Files, []string{"/home/igor/a b.txt"}) || c.Data != nil || c.Mime != "" {
		t.Fatalf("Read() = %+v, %v", c, ok)
	}
}

func TestPlainUriListFromAnotherProgramIsRead(t *testing.T) {
	shims(t, map[string]string{"wl-paste": fakeWlPaste([]string{"text/uri-list"}, []string{"file:///tmp/x\r\nfile:///tmp/y\r\n"})})
	c, ok := wlClipboard{}.Read()
	if !ok || !reflect.DeepEqual(c.Files, []string{"/tmp/x", "/tmp/y"}) {
		t.Fatalf("Read() = %+v, %v", c, ok)
	}
}

func TestLinksAndUnreadableOffersFallBackToText(t *testing.T) {
	// a browser's copied link offers a URI list too, but not of local files
	shims(t, map[string]string{"wl-paste": fakeWlPaste(
		[]string{"text/uri-list", "text/plain"}, []string{"https://example.com/page", "https://example.com/page"})})
	c, ok := wlClipboard{}.Read()
	if !ok || len(c.Files) != 0 || c.Mime != "text/plain" || string(c.Data) != "https://example.com/page" {
		t.Fatalf("Read() = %+v, %v", c, ok)
	}
}

func TestPasswordManagerFilesAreStillSecret(t *testing.T) {
	shims(t, map[string]string{"wl-paste": fakeWlPaste(
		[]string{"x-kde-passwordManagerHint", "x-special/gnome-copied-files"}, []string{"secret", "copy\nfile:///a"})})
	if _, ok := (wlClipboard{}).Read(); ok {
		t.Fatal("read a clipboard marked as a secret")
	}
}

func TestWriteFilesOffersTheFormatFileManagersPasteFrom(t *testing.T) {
	out := filepath.Join(t.TempDir(), "wl-copy")
	t.Setenv("WL_COPY_OUT", out)
	shims(t, map[string]string{"wl-copy": `echo "$*" > "$WL_COPY_OUT.args"; cat > "$WL_COPY_OUT.in"`})
	if err := (wlClipboard{}).WriteFiles([]string{"/c/files/0123/a b.pdf", "/c/files/0123/Photos"}); err != nil {
		t.Fatal(err)
	}
	if args := strings.TrimSpace(read(t, out+".args")); args != "--type x-special/gnome-copied-files" {
		t.Fatalf("wl-copy %s", args)
	}
	if got := read(t, out+".in"); got != "copy\nfile:///c/files/0123/a%20b.pdf\nfile:///c/files/0123/Photos" {
		t.Fatalf("clipboard payload = %q", got)
	}
}

func TestStagedBundleIsNotEchoedBackEvenIfItWouldZipDifferently(t *testing.T) {
	ctx := context.Background()
	a, cb, h := newAgentWith(t)
	// zipped by something other than us, so zipping the staged files again gives other bytes
	h.setFile("application/zip", makeZip(t, [3]string{"one.txt", "1", ""}, [3]string{"two.txt", "2", ""}), "files.zip", true)
	a.onEvent(ctx, "clip", clipEvent(h, "phone"))
	if len(cb.files) != 2 {
		t.Fatalf("clipboard files = %q", cb.files)
	}
	a.pushLocal(ctx)
	if _, posts := h.state(); posts != 0 {
		t.Fatalf("the files we staged were uploaded again (%d posts)", posts)
	}
}
