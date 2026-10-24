package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/abort-retry-ignore/central_clipboard"
)

var (
	phoneTok  = strings.Repeat("p", 32)
	laptopTok = strings.Repeat("l", 32)
)

type harness struct {
	t   *testing.T
	ts  *httptest.Server
	srv *Server
	dir string
}

func newEnv(t *testing.T, mut ...func(*config)) *harness {
	t.Helper()
	dir := t.TempDir()
	cfg := config{maxBytes: 1 << 20, passphrase: "correct horse battery"}
	for _, m := range mut {
		m(&cfg)
	}
	reg := NewRegistry(map[string]string{phoneTok: "phone", laptopTok: "laptop1"}, filepath.Join(dir, "tokens.json"))
	srv := NewServer(cfg, NewStore(""), reg)
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return &harness{t, ts, srv, dir}
}

func (e *harness) do(method, path string, body []byte, token string, hdr ...string) (*http.Response, []byte) {
	e.t.Helper()
	req, _ := http.NewRequest(method, e.ts.URL+path, bytes.NewReader(body))
	if token != "" {
		req.Header.Set("X-Token", token)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		if hdr[i] == "Host" {
			req.Host = hdr[i+1]
		} else {
			req.Header.Set(hdr[i], hdr[i+1])
		}
	}
	resp, err := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}).Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

func status(t *testing.T, got, want int) {
	t.Helper()
	if got != want {
		t.Fatalf("status = %d, want %d", got, want)
	}
}

func TestHealthzNeedsNoToken(t *testing.T) {
	e := newEnv(t)
	r, b := e.do("GET", "/healthz", nil, "")
	status(t, r.StatusCode, 200)
	if string(b) != "ok" {
		t.Fatalf("body = %q", b)
	}
}

func TestAuth(t *testing.T) {
	e := newEnv(t)
	for _, tc := range []struct{ method, path, tok string }{
		{"GET", "/", ""}, {"GET", "/", strings.Repeat("x", 32)}, {"POST", "/", strings.Repeat("x", 32)}, {"GET", "/events", ""},
	} {
		r, _ := e.do(tc.method, tc.path, []byte("hi"), tc.tok)
		status(t, r.StatusCode, 403)
	}
	r, _ := e.do("POST", "/?t="+phoneTok, []byte("hi"), "")
	status(t, r.StatusCode, 204)
	r, _ = e.do("GET", "/", nil, "", "Authorization", "Bearer "+laptopTok)
	status(t, r.StatusCode, 200)
}

func TestTextRoundtripAndSource(t *testing.T) {
	e := newEnv(t)
	r, _ := e.do("GET", "/", nil, phoneTok)
	status(t, r.StatusCode, 204)
	r, _ = e.do("POST", "/", []byte("héllo wörld"), phoneTok)
	status(t, r.StatusCode, 204)
	r, b := e.do("GET", "/clip", nil, laptopTok)
	if string(b) != "héllo wörld" || r.Header.Get("Content-Type") != "text/plain; charset=utf-8" || r.Header.Get("X-Clip-Source") != "phone" {
		t.Fatalf("got %q %q source=%q", b, r.Header.Get("Content-Type"), r.Header.Get("X-Clip-Source"))
	}
}

func TestIdenticalContentKeepsSeq(t *testing.T) {
	e := newEnv(t)
	a, _ := e.do("POST", "/", []byte("same"), phoneTok)
	b, _ := e.do("POST", "/", []byte("same"), laptopTok)
	c, _ := e.do("POST", "/", []byte("different"), phoneTok)
	if a.Header.Get("X-Clip-Seq") != b.Header.Get("X-Clip-Seq") || a.Header.Get("X-Clip-Seq") == c.Header.Get("X-Clip-Seq") {
		t.Fatalf("seq a=%s b=%s c=%s", a.Header.Get("X-Clip-Seq"), b.Header.Get("X-Clip-Seq"), c.Header.Get("X-Clip-Seq"))
	}
}

func TestSizeLimits(t *testing.T) {
	e := newEnv(t)
	r, _ := e.do("POST", "/", nil, phoneTok)
	status(t, r.StatusCode, 400)
	r, _ = e.do("POST", "/", bytes.Repeat([]byte("x"), 1<<20+1), phoneTok)
	status(t, r.StatusCode, 413)
	r, _ = e.do("POST", "/", bytes.Repeat([]byte("x"), 1<<20), phoneTok)
	status(t, r.StatusCode, 204)
}

func TestUnknownPathsAndMethods(t *testing.T) {
	e := newEnv(t)
	r, _ := e.do("GET", "/nope", nil, phoneTok)
	status(t, r.StatusCode, 404)
	r, _ = e.do("POST", "/nope", []byte("x"), phoneTok)
	status(t, r.StatusCode, 404)
	r, _ = e.do("DELETE", "/clip", nil, phoneTok)
	status(t, r.StatusCode, 405)
}

func TestStoredAsUploaded(t *testing.T) {
	// The hub does no conversion: agents convert what they cannot paste.
	e := newEnv(t)
	heic := append([]byte("\x00\x00\x00\x18ftypheic"), bytes.Repeat([]byte{1}, 16)...)
	e.do("POST", "/", heic, phoneTok)
	r, b := e.do("GET", "/", nil, phoneTok)
	if r.Header.Get("Content-Type") != "image/heic" || !bytes.Equal(b, heic) {
		t.Fatalf("got %s, %d bytes", r.Header.Get("Content-Type"), len(b))
	}
}

func TestSniff(t *testing.T) {
	for _, tc := range []struct {
		name      string
		in        []byte
		want, ext string
	}{
		{"text", []byte("hello ✓"), "text/plain", ".txt"},
		{"png", []byte("\x89PNG\r\n\x1a\nrest"), "image/png", ".png"},
		{"jpeg", []byte("\xff\xd8\xff\xe0rest"), "image/jpeg", ".jpg"},
		{"gif", []byte("GIF89arest"), "image/gif", ".gif"},
		{"webp", []byte("RIFF\x00\x00\x00\x00WEBPVP8 "), "image/webp", ".webp"},
		{"tiff", []byte("II*\x00rest"), "image/tiff", ".tiff"},
		{"heic", []byte("\x00\x00\x00\x18ftypheicmore"), "image/heic", ".heic"},
		{"rtf", []byte(`{\rtf1\ansi hi}`), "application/rtf", ".rtf"},
		{"m4a", []byte("\x00\x00\x00\x1cftypM4A \x00\x00\x02\x00"), "audio/mp4", ".m4a"},
		{"mov", []byte("\x00\x00\x00\x14ftypqt  \x00\x00\x00\x00"), "video/quicktime", ".mov"},
		{"mp4", []byte("\x00\x00\x00\x20ftypisom\x00\x00\x02\x00"), "video/mp4", ".mp4"},
		{"mp3 with tag", []byte("ID3\x04\x00\x00\x00\x00\x00\x00rest"), "audio/mpeg", ".mp3"},
		{"mp3 frame", []byte("\xff\xfb\x90\x00rest"), "audio/mpeg", ".mp3"},
		{"wav", []byte("RIFF\x00\x00\x00\x00WAVEfmt "), "audio/wav", ".wav"},
		{"webm", []byte("\x1a\x45\xdf\xa3\x01\x00\x00\x00B\x82\x84webm"), "video/webm", ".webm"},
		{"mkv", []byte("\x1a\x45\xdf\xa3\x01\x00\x00\x00B\x82\x88matroska"), "video/x-matroska", ".mkv"},
		{"pdf", []byte("%PDF-1.7\n"), "application/pdf", ".pdf"},
		{"zip", []byte("PK\x03\x04\x14\x00"), "application/zip", ".zip"},
		{"gzip", []byte("\x1f\x8b\x08\x00"), "application/gzip", ".gz"},
		{"text that merely starts like a tag", []byte("ID3 tags are binary"), "text/plain", ".txt"},
		{"invalid utf8", []byte("\x00\x01\xfe\xff binary"), "application/octet-stream", ".bin"},
		{"nul", []byte("a\x00b"), "application/octet-stream", ".bin"},
	} {
		if got, ext := sniff(tc.in); got != tc.want || ext != tc.ext {
			t.Errorf("%s: got %s %s, want %s %s", tc.name, got, ext, tc.want, tc.ext)
		}
	}
}

func TestOnlyOpaqueTypesAreFiles(t *testing.T) {
	for mime, want := range map[string]bool{
		"text/plain": false, "image/png": false, "image/heic": false, "application/rtf": false,
		"application/pdf": true, "video/mp4": true, "audio/mp4": true, "application/octet-stream": true, "application/zip": true,
	} {
		if isFileType(mime) != want {
			t.Errorf("isFileType(%s) = %v", mime, !want)
		}
	}
}

// --- files ------------------------------------------------------------------------

var m4a = append([]byte("\x00\x00\x00\x1cftypM4A \x00\x00\x02\x00"), bytes.Repeat([]byte{7}, 64)...)

func TestUnnamedFileFromThePhoneGetsAName(t *testing.T) {
	e := newEnv(t)
	r, _ := e.do("POST", "/", m4a, phoneTok)
	status(t, r.StatusCode, 204)
	r, b := e.do("GET", "/", nil, laptopTok)
	name, _ := url.PathUnescape(r.Header.Get("X-Clip-Name"))
	if !bytes.Equal(b, m4a) || r.Header.Get("Content-Type") != "audio/mp4" || !strings.HasPrefix(name, "clipboard-") || !strings.HasSuffix(name, ".m4a") {
		t.Fatalf("got %s %q, %d bytes", r.Header.Get("Content-Type"), name, len(b))
	}
	if cd := r.Header.Get("Content-Disposition"); !strings.HasPrefix(cd, "attachment; filename=\"clipboard-") || !strings.Contains(cd, "filename*=UTF-8''clipboard-") {
		t.Fatalf("Content-Disposition = %q", cd)
	}
	if r.Header.Get("X-Clip-Bundle") != "" {
		t.Fatal("a single file is not a bundle")
	}
}

func TestPlainTextAndPicturesStayUnnamed(t *testing.T) {
	e := newEnv(t)
	for _, body := range [][]byte{[]byte("hello"), []byte("\x89PNG\r\n\x1a\nrest"), []byte(`{\rtf1 hi}`)} {
		e.do("POST", "/", body, phoneTok)
		r, _ := e.do("GET", "/", nil, phoneTok)
		if r.Header.Get("X-Clip-Name") != "" || r.Header.Get("Content-Disposition") != "" {
			t.Fatalf("%q came back as a file: %v", body[:5], r.Header)
		}
	}
}

func TestDeclaredFilenameMakesATextClipAFile(t *testing.T) {
	e := newEnv(t)
	// "notes é.txt", percent-encoded as agents send it
	e.do("POST", "/", []byte("just words"), laptopTok, "X-Filename", "notes%20%C3%A9.txt")
	r, b := e.do("GET", "/", nil, phoneTok)
	if string(b) != "just words" || r.Header.Get("X-Clip-Name") != "notes%20%C3%A9.txt" {
		t.Fatalf("got %q name=%q", b, r.Header.Get("X-Clip-Name"))
	}
	if cd := r.Header.Get("Content-Disposition"); cd != `attachment; filename="notes _.txt"; filename*=UTF-8''notes%20%C3%A9.txt` {
		t.Fatalf("Content-Disposition = %q", cd)
	}
}

func TestFilenamesAreCleaned(t *testing.T) {
	for in, want := range map[string]string{
		"a%0D%0Ab.txt":           "ab.txt", // no header injection
		"%00hidden":              "hidden",
		"  padded.txt ":          "padded.txt",
		"%zz":                    "", // not valid percent-encoding
		"":                       "",
		strings.Repeat("é", 200): strings.Repeat("é", 127), // 254 bytes, cut on a character boundary
	} {
		if got := cleanName(in); got != want {
			t.Errorf("cleanName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBundleNeedsAZipAndTheFlag(t *testing.T) {
	e := newEnv(t)
	zipped := []byte("PK\x03\x04 pretend archive")
	e.do("POST", "/", zipped, laptopTok, "X-Filename", "files.zip", "X-Clip-Bundle", "1")
	r, _ := e.do("GET", "/", nil, phoneTok)
	if r.Header.Get("X-Clip-Bundle") != "1" {
		t.Fatal("bundle flag lost")
	}
	e.do("POST", "/", zipped, laptopTok, "X-Filename", "files.zip") // an ordinary zip file
	r, _ = e.do("GET", "/", nil, phoneTok)
	if r.Header.Get("X-Clip-Bundle") != "" || r.Header.Get("X-Clip-Name") != "files.zip" {
		t.Fatalf("plain zip: bundle=%q name=%q", r.Header.Get("X-Clip-Bundle"), r.Header.Get("X-Clip-Name"))
	}
	e.do("POST", "/", []byte("not a zip"), laptopTok, "X-Filename", "x.txt", "X-Clip-Bundle", "1")
	r, _ = e.do("GET", "/", nil, phoneTok)
	if r.Header.Get("X-Clip-Bundle") != "" {
		t.Fatal("bundle flag accepted on something that is not a zip")
	}
}

func TestSameBytesUnderAnotherNameIsAnotherClip(t *testing.T) {
	e := newEnv(t)
	a, _ := e.do("POST", "/", []byte("hello"), phoneTok)
	b, _ := e.do("POST", "/", []byte("hello"), phoneTok, "X-Filename", "hello.txt")
	c, _ := e.do("POST", "/", []byte("hello"), phoneTok, "X-Filename", "hello.txt")
	if a.Header.Get("X-Clip-Seq") == b.Header.Get("X-Clip-Seq") || b.Header.Get("X-Clip-Seq") != c.Header.Get("X-Clip-Seq") {
		t.Fatalf("seq %s %s %s", a.Header.Get("X-Clip-Seq"), b.Header.Get("X-Clip-Seq"), c.Header.Get("X-Clip-Seq"))
	}
}

func TestHelloAnnouncesTheLimitAndEventsCarryTheName(t *testing.T) {
	e := newEnv(t)
	st := e.stream(laptopTok)
	_, data := st.next()
	if data["max"] != float64(1<<20) {
		t.Fatalf("hello max = %v", data["max"])
	}
	e.do("POST", "/", m4a, phoneTok)
	ev, data := st.next()
	if ev != "clip" || !strings.HasSuffix(data["name"].(string), ".m4a") || data["mime"] != "audio/mp4" {
		t.Fatalf("event = %s %v", ev, data)
	}
}

func TestFileNameAndBundleSurviveARestart(t *testing.T) {
	dir := t.TempDir()
	NewStore(dir).Set([]byte("PK\x03\x04zip"), "application/zip", "laptop1", "my files.zip", true)
	c, _ := NewStore(dir).Snapshot()
	if c == nil || c.Name != "my files.zip" || !c.Bundle {
		t.Fatalf("reloaded %+v", c)
	}
}

// --- events ---------------------------------------------------------------------

type stream struct {
	t  *testing.T
	sc *bufio.Scanner
}

func (e *harness) stream(token string) *stream {
	e.t.Helper()
	req, _ := http.NewRequest("GET", e.ts.URL+"/events", nil)
	req.Header.Set("X-Token", token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { resp.Body.Close() })
	status(e.t, resp.StatusCode, 200)
	return &stream{e.t, bufio.NewScanner(resp.Body)}
}

func (s *stream) next() (string, map[string]any) {
	s.t.Helper()
	done := make(chan struct{})
	timer := time.AfterFunc(5*time.Second, func() { s.t.Error("timed out waiting for an event"); close(done) })
	defer timer.Stop()
	var event string
	var data map[string]any
	for s.sc.Scan() {
		line := s.sc.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			event = line[7:]
		case strings.HasPrefix(line, "data: "):
			json.Unmarshal([]byte(line[6:]), &data)
		case line == "" && event != "":
			return event, data
		}
	}
	s.t.Fatal("stream ended")
	return "", nil
}

func TestEventsHelloAndChanges(t *testing.T) {
	e := newEnv(t)
	ev, data := e.stream(laptopTok).next()
	if ev != "hello" || data["you"] != "laptop1" || data["clip"] != nil {
		t.Fatalf("empty hello = %s %v", ev, data)
	}

	e.do("POST", "/", []byte("first"), phoneTok)
	st := e.stream(laptopTok)
	ev, data = st.next()
	clip := data["clip"].(map[string]any)
	if ev != "hello" || clip["source"] != "phone" || clip["size"] != float64(5) {
		t.Fatalf("hello = %s %v", ev, data)
	}

	e.do("POST", "/", []byte("first"), phoneTok) // duplicate: no event
	e.do("POST", "/", []byte("second!"), laptopTok)
	ev, data = st.next()
	if ev != "clip" || data["source"] != "laptop1" || data["size"] != float64(7) {
		t.Fatalf("change = %s %v", ev, data)
	}
}

// --- persistence ------------------------------------------------------------------

func TestPersistedClipSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	s1 := NewStore(dir)
	s1.Set([]byte("keep me"), "text/plain", "phone", "", false)
	c, _ := s1.Set([]byte("and me"), "text/plain", "laptop1", "", false)

	c2, _ := NewStore(dir).Snapshot()
	if c2 == nil || string(c2.data) != "and me" || c2.Seq != c.Seq || c2.Source != "laptop1" {
		t.Fatalf("reloaded %+v", c2)
	}
	next, _ := NewStore(dir).Set([]byte("next"), "text/plain", "phone", "", false)
	if next.Seq != c.Seq+1 {
		t.Fatalf("seq did not continue: %d after %d", next.Seq, c.Seq)
	}
}

func TestCorruptPersistedClipStartsEmpty(t *testing.T) {
	dir := t.TempDir()
	NewStore(dir).Set([]byte("keep me"), "text/plain", "phone", "", false)
	os.WriteFile(filepath.Join(dir, "current.bin"), []byte("tampered"), 0o600)
	if c, _ := NewStore(dir).Snapshot(); c != nil {
		t.Fatalf("loaded %+v", c)
	}
}

func TestMemoryOnlyStoreWritesNothing(t *testing.T) {
	dir := t.TempDir()
	s := NewStore("")
	s.Set([]byte("ephemeral"), "text/plain", "phone", "", false)
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("files written: %v", entries)
	}
}

// --- tokens and enrollment -------------------------------------------------------------

func TestParseTokens(t *testing.T) {
	a, b := strings.Repeat("a", 16), strings.Repeat("b", 17)
	got, err := parseTokens("phone:" + a + ", laptop:" + b)
	if err != nil || got[a] != "phone" || got[b] != "laptop" {
		t.Fatalf("%v %v", got, err)
	}
	got, _ = parseTokens(a)
	if got[a] != "device1" {
		t.Fatalf("%v", got)
	}
	if _, err := parseTokens("phone:short"); err == nil {
		t.Fatal("short token accepted")
	}
	if got, err := parseTokens(""); err != nil || len(got) != 0 {
		t.Fatalf("empty: %v %v", got, err)
	}
}

func enrollForm(name, pass string) []byte {
	return []byte("name=" + name + "&passphrase=" + pass)
}

func TestEnrollmentIssuesWorkingToken(t *testing.T) {
	e := newEnv(t)
	r, b := e.do("POST", "/enroll", enrollForm("laptop2", "correct+horse+battery"), "",
		"Content-Type", "application/x-www-form-urlencoded", "Host", "hub:8766")
	status(t, r.StatusCode, 200)
	var got struct{ Name, Token, URL string }
	if err := json.Unmarshal(b, &got); err != nil || got.Name != "laptop2" || len(got.Token) != 48 {
		t.Fatalf("reply %s (%v)", b, err)
	}
	if got.URL != "http://hub:8766/?t="+got.Token {
		t.Fatalf("url = %s", got.URL)
	}
	// the shell installer parses the reply with sed, so keep the shape stable
	if !strings.Contains(string(b), `"token":"`+got.Token+`"`) {
		t.Fatalf("token not in the expected spot: %s", b)
	}
	r, _ = e.do("POST", "/", []byte("from laptop2"), got.Token)
	status(t, r.StatusCode, 204)
	r, _ = e.do("GET", "/", nil, got.Token)
	if r.Header.Get("X-Clip-Source") != "laptop2" {
		t.Fatalf("source = %s", r.Header.Get("X-Clip-Source"))
	}
}

func TestEnrollmentAcceptsJSON(t *testing.T) {
	e := newEnv(t)
	body, _ := json.Marshal(map[string]string{"name": "ipad", "passphrase": "correct horse battery"})
	r, _ := e.do("POST", "/enroll", body, "", "Content-Type", "application/json")
	status(t, r.StatusCode, 200)
}

func TestEnrollmentStoresOnlyAHash(t *testing.T) {
	e := newEnv(t)
	_, b := e.do("POST", "/enroll", enrollForm("laptop2", "correct+horse+battery"), "", "Content-Type", "application/x-www-form-urlencoded")
	var got struct{ Token string }
	json.Unmarshal(b, &got)
	path := filepath.Join(e.dir, "tokens.json")
	file, _ := os.ReadFile(path)
	if bytes.Contains(file, []byte(got.Token)) {
		t.Fatal("tokens.json contains the plaintext token")
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v", fi.Mode().Perm())
	}
}

func TestEnrollmentFailures(t *testing.T) {
	form := "application/x-www-form-urlencoded"
	t.Run("disabled", func(t *testing.T) {
		e := newEnv(t, func(c *config) { c.passphrase = "" })
		r, _ := e.do("POST", "/enroll", enrollForm("x", "anything-long"), "", "Content-Type", form)
		status(t, r.StatusCode, 404)
	})
	t.Run("wrong passphrase", func(t *testing.T) {
		e := newEnv(t)
		r, _ := e.do("POST", "/enroll", enrollForm("x", "wrong"), "", "Content-Type", form)
		status(t, r.StatusCode, 403)
	})
	t.Run("bad and taken names", func(t *testing.T) {
		e := newEnv(t)
		r, _ := e.do("POST", "/enroll", enrollForm("has+space", "correct+horse+battery"), "", "Content-Type", form)
		status(t, r.StatusCode, 400)
		r, _ = e.do("POST", "/enroll", enrollForm("phone", "correct+horse+battery"), "", "Content-Type", form)
		status(t, r.StatusCode, 409)
	})
	t.Run("get is not allowed", func(t *testing.T) {
		r, _ := newEnv(t).do("GET", "/enroll", nil, "")
		status(t, r.StatusCode, 405)
	})
	t.Run("bad host", func(t *testing.T) {
		e := newEnv(t)
		r, _ := e.do("POST", "/enroll", enrollForm("x", "correct+horse+battery"), "", "Content-Type", form, "Host", "evil host/x")
		status(t, r.StatusCode, 400)
	})
}

func TestWrongPassphraseIsThrottled(t *testing.T) {
	e := newEnv(t)
	form := "application/x-www-form-urlencoded"
	for i := 0; i < 5; i++ {
		r, _ := e.do("POST", "/enroll", enrollForm("x", "wrong"), "", "Content-Type", form)
		status(t, r.StatusCode, 403)
	}
	// even the right passphrase is refused until the window passes
	r, _ := e.do("POST", "/enroll", enrollForm("x", "correct+horse+battery"), "", "Content-Type", form)
	status(t, r.StatusCode, 429)
}

func TestReenrollingRotatesTheToken(t *testing.T) {
	e := newEnv(t)
	form := "application/x-www-form-urlencoded"
	tok := func() string {
		_, b := e.do("POST", "/enroll", enrollForm("laptop2", "correct+horse+battery"), "", "Content-Type", form)
		var got struct{ Token string }
		json.Unmarshal(b, &got)
		return got.Token
	}
	old, fresh := tok(), tok()
	r, _ := e.do("GET", "/", nil, old)
	status(t, r.StatusCode, 403)
	r, _ = e.do("GET", "/", nil, fresh)
	status(t, r.StatusCode, 204)
}

func TestRevokeTakesEffectWithoutRestart(t *testing.T) {
	e := newEnv(t)
	_, b := e.do("POST", "/enroll", enrollForm("laptop2", "correct+horse+battery"), "", "Content-Type", "application/x-www-form-urlencoded")
	var got struct{ Token string }
	json.Unmarshal(b, &got)

	// the CLI is a separate process working on the same file
	cli := NewRegistry(nil, filepath.Join(e.dir, "tokens.json"))
	if ok, err := cli.revoke("laptop2"); !ok || err != nil {
		t.Fatalf("revoke: %v %v", ok, err)
	}
	r, _ := e.do("GET", "/", nil, got.Token)
	status(t, r.StatusCode, 403)
	if ok, _ := cli.revoke("laptop2"); ok {
		t.Fatal("revoked twice")
	}
}

func TestRevokeClosesTheOpenStream(t *testing.T) {
	e := newEnv(t)
	e.srv.ping = 50 * time.Millisecond
	_, b := e.do("POST", "/enroll", enrollForm("laptop2", "correct+horse+battery"), "", "Content-Type", "application/x-www-form-urlencoded")
	var got struct{ Token string }
	json.Unmarshal(b, &got)

	st := e.stream(got.Token)
	if ev, _ := st.next(); ev != "hello" {
		t.Fatalf("first event = %s", ev)
	}
	if ok, _ := NewRegistry(nil, filepath.Join(e.dir, "tokens.json")).revoke("laptop2"); !ok {
		t.Fatal("revoke failed")
	}
	ended := make(chan struct{})
	go func() {
		for st.sc.Scan() {
		}
		close(ended)
	}()
	select {
	case <-ended:
	case <-time.After(3 * time.Second):
		t.Fatal("stream of a revoked device stayed open")
	}
}

// --- what the hub hands out ------------------------------------------------------------

func TestJoinPageAndRedirect(t *testing.T) {
	e := newEnv(t)
	r, b := e.do("GET", "/join", nil, "", "Host", "hub:8766")
	status(t, r.StatusCode, 200)
	if !strings.Contains(string(b), "curl -fsSL http://hub:8766/install.sh | bash") {
		t.Fatalf("page lacks the install line")
	}
	r, _ = e.do("GET", "/", nil, "", "Accept", "text/html,application/xhtml+xml")
	status(t, r.StatusCode, 302)
	if r.Header.Get("Location") != "/join" {
		t.Fatalf("location = %s", r.Header.Get("Location"))
	}
	r, _ = e.do("GET", "/", nil, "") // a script without a token still gets 403
	status(t, r.StatusCode, 403)
}

func TestJoinPageOffersOurShortcutOnceItIsSigned(t *testing.T) {
	e := newEnv(t)
	// a stand-in for the embedded files, so this holds whether or not the real one has been signed yet
	e.srv.files = fstest.MapFS{"shortcuts/Send to Computer.shortcut": {Data: []byte("AEA1send")}}
	_, b := e.do("GET", "/join", nil, "")
	if !strings.Contains(string(b), "Send%20to%20Computer.shortcut") || strings.Contains(string(b), "Clipboard.shortcut") {
		t.Fatal("without our shortcut the page should offer the Unayung pair")
	}

	e.srv.files = fstest.MapFS{"shortcuts/Clipboard.shortcut": {Data: []byte("AEA1signed")}}
	_, b = e.do("GET", "/join", nil, "")
	page := string(b)
	if !strings.Contains(page, `href="/shortcuts/Clipboard.shortcut"`) || strings.Contains(page, "Send%20to%20Computer.shortcut") {
		t.Fatal("with our shortcut the page should offer only that one")
	}
	r, body := e.do("GET", "/shortcuts/Clipboard.shortcut", nil, "")
	status(t, r.StatusCode, 200)
	if string(body) != "AEA1signed" || !strings.Contains(r.Header.Get("Content-Disposition"), "Clipboard.shortcut") {
		t.Fatalf("served %q with %q", body, r.Header.Get("Content-Disposition"))
	}
}

func TestInstallScriptPointsBackAtTheHub(t *testing.T) {
	e := newEnv(t)
	r, b := e.do("GET", "/install.sh", nil, "", "Host", "100.64.0.10:8766")
	status(t, r.StatusCode, 200)
	s := string(b)
	if !strings.Contains(s, "HUB='http://100.64.0.10:8766'") {
		t.Fatalf("hub address not substituted:\n%s", s[:300])
	}
	if strings.Contains(s, "@HUB@") {
		t.Fatal("placeholder left in the script")
	}
	r, _ = e.do("GET", "/install.sh", nil, "", "Host", "evil host/x")
	status(t, r.StatusCode, 400)
}

func TestPublicURLOverridesHost(t *testing.T) {
	e := newEnv(t, func(c *config) { c.publicURL = "http://clip.lan:9000" })
	_, b := e.do("GET", "/install.sh", nil, "", "Host", "ignored:1")
	if !strings.Contains(string(b), "HUB='http://clip.lan:9000'") {
		t.Fatal("public URL not used")
	}
}

func TestAssetsAreServed(t *testing.T) {
	e := newEnv(t)
	r, b := e.do("GET", "/shortcuts/Send%20to%20Computer.shortcut", nil, "")
	want, _ := assets.FS.ReadFile("shortcuts/Send to Computer.shortcut")
	status(t, r.StatusCode, 200)
	if !bytes.Equal(b, want) || !strings.Contains(r.Header.Get("Content-Disposition"), "Send%20to%20Computer.shortcut") {
		t.Fatalf("shortcut mismatch (%d bytes), disposition %q", len(b), r.Header.Get("Content-Disposition"))
	}
	r, _ = e.do("GET", "/shortcuts/missing.shortcut", nil, "")
	status(t, r.StatusCode, 404)
	r, _ = e.do("GET", "/shortcuts/..%2Fagent%2Fjoin.sh", nil, "")
	status(t, r.StatusCode, 404)

	r, b = e.do("GET", "/agent.service", nil, "")
	status(t, r.StatusCode, 200)
	if !strings.Contains(string(b), "ExecStart=%h/.local/bin/central-clipboard-agent") {
		t.Fatal("unit file looks wrong")
	}

	r, _ = e.do("GET", "/agent/linux-riscv64.gz", nil, "")
	status(t, r.StatusCode, 404)
	if _, err := assets.FS.ReadFile("agentbin/linux-amd64.gz"); err != nil {
		t.Skip("agent builds not present; run scripts/build.sh")
	}
	for _, arch := range []string{"amd64", "arm64"} {
		r, b = e.do("GET", "/agent/linux-"+arch+".gz", nil, "")
		status(t, r.StatusCode, 200)
		if len(b) < 2 || b[0] != 0x1f || b[1] != 0x8b {
			t.Fatalf("%s is not gzip", arch)
		}
	}
}

func TestEmbeddedShortcutsAreSigned(t *testing.T) {
	// iOS refuses unsigned shortcuts; a signed one starts with Apple's AEA1 header.
	files, _ := fs.Glob(assets.FS, "shortcuts/*.shortcut")
	if len(files) == 0 {
		t.Fatal("no shortcuts embedded")
	}
	for _, name := range files {
		b, _ := assets.FS.ReadFile(name)
		if !bytes.HasPrefix(b, []byte("AEA1")) {
			t.Errorf("%s is not signed (starts %q)", name, b[:min(len(b), 8)])
		}
	}
}
