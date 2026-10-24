package main

import (
	"archive/zip"
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func write(t *testing.T, path, content string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

const goodID = "0123456789abcdef"

// --- what file managers put on the clipboard -----------------------------------------

func TestParseCopiedFiles(t *testing.T) {
	for _, tc := range []struct {
		name, in string
		want     []string
	}{
		{"gnome copy", "copy\nfile:///home/igor/a%20b.txt\nfile:///tmp/x", []string{"/home/igor/a b.txt", "/tmp/x"}},
		{"gnome cut", "cut\nfile:///a", []string{"/a"}},
		{"uri list with CRLF and comments", "# from a file manager\r\nfile:///a\r\nfile:///b\r\n", []string{"/a", "/b"}},
		{"only local files count", "https://example.com/x\nfile://otherhost/x\nfile:///ok\nfile:relative", []string{"/ok"}},
		{"localhost is local", "file://localhost/x", []string{"/x"}},
		{"duplicates collapse", "copy\nfile:///a\nfile:///a\nfile:///b", []string{"/a", "/b"}},
		{"a plain path is not a URI", "/home/igor/x", nil},
		{"unicode", "copy\nfile:///h/%C3%A9t%C3%A9.txt", []string{"/h/été.txt"}},
		{"empty", "", nil},
	} {
		if got := parseCopiedFiles(tc.in); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestFilePathsSurviveTheClipboardFormat(t *testing.T) {
	paths := []string{"/h/plain.txt", "/h/with space.txt", "/h/été #1 100%.txt", "/h/q?x=y&z", "/h/a+b"}
	payload := copiedFilesPayload(paths)
	if !strings.HasPrefix(payload, "copy\nfile:///h/plain.txt\nfile:///h/with%20space.txt\n") || strings.HasSuffix(payload, "\n") {
		t.Fatalf("payload = %q", payload)
	}
	if got := parseCopiedFiles(payload); !reflect.DeepEqual(got, paths) {
		t.Fatalf("round trip gave %q", got)
	}
}

// --- sending ------------------------------------------------------------------------

func TestSingleFileIsSentAsItIs(t *testing.T) {
	dir := t.TempDir()
	p := write(t, filepath.Join(dir, "report.pdf"), "%PDF-1.7 pretend")
	got, err := packFiles([]string{p}, 1<<20)
	if err != nil || got.bundle || got.name != "report.pdf" || string(got.data) != "%PDF-1.7 pretend" {
		t.Fatalf("got %+v, %v", got, err)
	}
}

func TestSingleFileProblems(t *testing.T) {
	dir := t.TempDir()
	if _, err := packFiles([]string{write(t, filepath.Join(dir, "empty"), "")}, 1<<20); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("empty file: %v", err)
	}
	if _, err := packFiles([]string{write(t, filepath.Join(dir, "big"), strings.Repeat("x", 100))}, 99); err == nil || !strings.Contains(err.Error(), "over the hub's limit of 99") {
		t.Fatalf("too big: %v", err)
	}
	if _, err := packFiles([]string{filepath.Join(dir, "gone")}, 1<<20); err == nil {
		t.Fatal("missing file accepted")
	}
	if _, err := packFiles(nil, 1<<20); err == nil {
		t.Fatal("nothing to pack accepted")
	}
}

func TestCopiedSymlinkSendsWhatItPointsAt(t *testing.T) {
	dir := t.TempDir()
	target := write(t, filepath.Join(dir, "real.txt"), "contents")
	link := filepath.Join(dir, "shortcut.txt")
	if err := os.Symlink(target, link); err != nil {
		t.Skip("no symlinks here")
	}
	got, err := packFiles([]string{link}, 1<<20)
	if err != nil || got.bundle || got.name != "shortcut.txt" || string(got.data) != "contents" {
		t.Fatalf("got %+v, %v", got, err)
	}
}

// tree builds ~/Photos/{a.txt, sub/b.bin, link -> a.txt} and ~/loose.txt.
func tree(t *testing.T) (folder, loose string) {
	t.Helper()
	home := t.TempDir()
	folder = filepath.Join(home, "Photos")
	write(t, filepath.Join(folder, "a.txt"), "alpha")
	write(t, filepath.Join(folder, "sub", "b.bin"), "\x00\x01bravo")
	os.MkdirAll(filepath.Join(folder, "empty-dir"), 0o755)
	os.Symlink(filepath.Join(folder, "a.txt"), filepath.Join(folder, "link"))
	loose = write(t, filepath.Join(home, "loose.txt"), "charlie")
	return
}

func TestFolderAndFilesTravelAsAZipAndComeBackIntact(t *testing.T) {
	folder, loose := tree(t)
	got, err := packFiles([]string{folder, loose}, 1<<20)
	if err != nil || !got.bundle || got.name != "clipboard-files.zip" {
		t.Fatalf("got %+v, %v", got, err)
	}
	again, _ := packFiles([]string{folder, loose}, 1<<20)
	if !bytes.Equal(got.data, again.data) {
		t.Fatal("the same copy zipped to different bytes")
	}

	root := t.TempDir()
	tops, err := stage(root, goodID, got.name, got.data, true)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{filepath.Join(root, goodID, "Photos"), filepath.Join(root, goodID, "loose.txt")}
	if !reflect.DeepEqual(tops, want) {
		t.Fatalf("top-level paths = %q, want %q", tops, want)
	}
	if read(t, tops[0]+"/a.txt") != "alpha" || read(t, tops[0]+"/sub/b.bin") != "\x00\x01bravo" || read(t, tops[1]) != "charlie" {
		t.Fatal("contents differ")
	}
	if _, err := os.Lstat(tops[0] + "/link"); err == nil {
		t.Fatal("a symlink was carried over")
	}
	if st, err := os.Stat(tops[0] + "/empty-dir"); err != nil || !st.IsDir() {
		t.Fatalf("empty folder lost: %v", err)
	}
}

func TestOneFolderIsNamedAfterItself(t *testing.T) {
	folder, _ := tree(t)
	got, err := packFiles([]string{folder}, 1<<20)
	if err != nil || !got.bundle || got.name != "Photos.zip" {
		t.Fatalf("got %+v, %v", got, err)
	}
}

func TestSameNameFromTwoFoldersStaysApart(t *testing.T) {
	a := write(t, filepath.Join(t.TempDir(), "notes.txt"), "from a")
	b := write(t, filepath.Join(t.TempDir(), "notes.txt"), "from b")
	got, err := packFiles([]string{a, b}, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	tops, err := stage(t.TempDir(), goodID, got.name, got.data, true)
	if err != nil || len(tops) != 2 || read(t, tops[0]) != "from a" || read(t, tops[1]) != "from b" || filepath.Base(tops[1]) != "notes.txt (2)" {
		t.Fatalf("tops %q, %v", tops, err)
	}
}

func TestSelectionOverTheLimitIsRefused(t *testing.T) {
	folder, loose := tree(t) // 5 + 7 + 7 bytes of content
	if _, err := packFiles([]string{folder, loose}, 15); err == nil || !strings.Contains(err.Error(), "the selection") {
		t.Fatalf("err = %v", err)
	}
	if _, err := packFiles([]string{folder}, 8); err == nil || !strings.Contains(err.Error(), "Photos is") {
		t.Fatalf("err = %v", err)
	}
}

// --- receiving ----------------------------------------------------------------------

func TestStageWritesASingleFileUnderItsName(t *testing.T) {
	root := filepath.Join(t.TempDir(), "files")
	paths, err := stage(root, goodID, "report.pdf", []byte("bytes"), false)
	if err != nil || len(paths) != 1 || paths[0] != filepath.Join(root, goodID, "report.pdf") || read(t, paths[0]) != "bytes" {
		t.Fatalf("paths %q, %v", paths, err)
	}
	if st, _ := os.Stat(paths[0]); st.Mode().Perm() != 0o644 {
		t.Fatalf("mode %v", st.Mode())
	}
	if st, _ := os.Stat(root); st.Mode().Perm() != 0o700 {
		t.Fatalf("staging directory mode %v", st.Mode())
	}
}

func TestStageKeepsOnlyTheNewestClip(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "notes.txt"), "mine")
	os.MkdirAll(filepath.Join(root, "Documents"), 0o755)
	older, _ := stage(root, "aaaaaaaaaaaaaaaa", "one.txt", []byte("1"), false)
	newer, err := stage(root, "bbbbbbbbbbbbbbbb", "two.txt", []byte("2"), false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(older[0]); err == nil {
		t.Fatal("the previous clip was kept")
	}
	if read(t, newer[0]) != "2" || read(t, filepath.Join(root, "notes.txt")) != "mine" {
		t.Fatal("lost the new clip or touched something that is not ours")
	}
	if _, err := os.Stat(filepath.Join(root, "Documents")); err != nil {
		t.Fatal("removed a folder that is not ours")
	}
}

func TestStageRefusesAnIdThatIsNotOurs(t *testing.T) {
	root := t.TempDir()
	for _, id := range []string{"", "../escape", "short", "0123456789ABCDEF", goodID + "0"} {
		if _, err := stage(root, id, "x", []byte("x"), false); err == nil {
			t.Errorf("accepted id %q", id)
		}
	}
}

func TestSafeName(t *testing.T) {
	long := strings.Repeat("é", 150)
	for _, tc := range []struct{ in, want string }{
		{"report.pdf", "report.pdf"},
		{"my file é.txt", "my file é.txt"},
		{"../../etc/passwd", "passwd"},
		{`C:\Users\igor\a.txt`, "a.txt"},
		{"dir/", "fallback"},
		{"..", "fallback"},
		{".", "fallback"},
		{"", "fallback"},
		{"x\x00\ny.txt", "xy.txt"},
		{"  padded  ", "padded"},
		{".bashrc", ".bashrc"},
		{long, strings.Repeat("é", 100)}, // 200 bytes, cut on a character boundary
	} {
		if got := safeName(tc.in, "fallback"); got != tc.want {
			t.Errorf("safeName(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func makeZip(t *testing.T, entries ...[3]string) []byte { // name, content, "link" or ""
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, e := range entries {
		h := &zip.FileHeader{Name: e[0], Method: zip.Deflate}
		switch {
		case e[2] == "link":
			h.SetMode(fs.ModeSymlink | 0o777)
		case e[2] == "exec":
			h.SetMode(0o755)
		}
		w, err := zw.CreateHeader(h)
		if err != nil {
			t.Fatal(err)
		}
		w.Write([]byte(e[1]))
	}
	zw.Close()
	return buf.Bytes()
}

func TestHostileArchivesAreRefused(t *testing.T) {
	for name, entry := range map[string]string{
		"parent":   "../evil.txt",
		"deep":     "a/../../evil.txt",
		"absolute": "/tmp/evil.txt",
		"nul":      "evil\x00.txt",
		"dot":      "./x",
		"empty":    "/",
	} {
		outer := t.TempDir()
		root := filepath.Join(outer, "files")
		if _, err := stage(root, goodID, "x.zip", makeZip(t, [3]string{entry, "pwned", ""}), true); err == nil {
			t.Errorf("%s: archive accepted", name)
		}
		if _, err := os.Stat(filepath.Join(root, goodID)); err == nil {
			t.Errorf("%s: left a half-extracted clip behind", name)
		}
		if got, _ := filepath.Glob(filepath.Join(outer, "*")); len(got) != 1 {
			t.Errorf("%s: wrote outside the staging directory: %q", name, got)
		}
	}
}

func TestArchiveQuirksAreHandledSafely(t *testing.T) {
	root := t.TempDir()
	if _, err := stage(root, goodID, "x.zip", makeZip(t, [3]string{"a.txt", "1", ""}, [3]string{"a.txt", "2", ""}), true); err == nil {
		t.Error("two entries with one name were accepted")
	}
	if _, err := stage(root, goodID, "x.zip", makeZip(t, [3]string{"a", "file", ""}, [3]string{"a/b", "inside a file", ""}), true); err == nil {
		t.Error("a file used as a folder was accepted")
	}
	if _, err := stage(root, goodID, "x.zip", makeZip(t, [3]string{"only-a-link", "/etc/passwd", "link"}), true); err == nil {
		t.Error("an archive with nothing but a link was accepted")
	}
	tops, err := stage(root, goodID, "x.zip", makeZip(t, [3]string{"run.sh", "#!/bin/sh", "exec"}, [3]string{"link", "/etc/passwd", "link"}), true)
	if err != nil || len(tops) != 1 {
		t.Fatalf("tops %q, %v", tops, err)
	}
	if st, _ := os.Stat(tops[0]); st.Mode().Perm() != 0o644 {
		t.Errorf("received file mode %v: it must not arrive executable", st.Mode())
	}
	if _, err := os.Lstat(filepath.Join(root, goodID, "link")); err == nil {
		t.Error("a link was created")
	}
	if _, err := stage(root, goodID, "x.zip", []byte("not a zip at all"), true); err == nil {
		t.Error("garbage accepted as an archive")
	}
}

func TestArchiveThatExpandsTooFarIsRefused(t *testing.T) {
	old := maxExtract
	maxExtract = 1000
	t.Cleanup(func() { maxExtract = old })
	root := t.TempDir()
	bomb := makeZip(t, [3]string{"zeros", strings.Repeat("\x00", 5000), ""}) // a few dozen bytes zipped
	if len(bomb) > 200 {
		t.Fatalf("test archive is %d bytes; it should compress well", len(bomb))
	}
	if _, err := stage(root, goodID, "x.zip", bomb, true); err == nil || !strings.Contains(err.Error(), "expands") {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, goodID)); err == nil {
		t.Fatal("left the partial extraction behind")
	}
}

func TestStageRootHonoursTheEnvironment(t *testing.T) {
	t.Setenv("CLIP_FILES_DIR", "relative/dir")
	if got := stageRoot(); !filepath.IsAbs(got) || !strings.HasSuffix(got, "/relative/dir") {
		t.Fatalf("stageRoot() = %q", got)
	}
	t.Setenv("CLIP_FILES_DIR", "")
	t.Setenv("XDG_CACHE_HOME", "/cache")
	if got := stageRoot(); got != "/cache/central-clipboard/files" {
		t.Fatalf("stageRoot() = %q", got)
	}
}
