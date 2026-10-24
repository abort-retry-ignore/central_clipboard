package main

import (
	"archive/zip"
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	agentClipCap   = 256 << 20 // never hold more than this in memory, whatever the hub allows
	maxFileEntries = 10000
)

// maxExtract is the most a received archive may expand to; a variable so tests can lower it.
var maxExtract int64 = 512 << 20

// parseCopiedFiles reads the paths out of a file manager's copy: either
// x-special/gnome-copied-files ("copy" or "cut", then one URI per line) or a
// text/uri-list (CRLF lines, # comments). Anything that is not a local file is dropped.
func parseCopiedFiles(s string) []string {
	var paths []string
	seen := map[string]bool{}
	for i, line := range strings.Split(s, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" || strings.HasPrefix(line, "#") || (i == 0 && (line == "copy" || line == "cut")) {
			continue
		}
		if p, ok := pathFromURI(line); ok && !seen[p] {
			seen[p] = true
			paths = append(paths, p)
		}
	}
	return paths
}

func pathFromURI(s string) (string, bool) {
	u, err := url.Parse(s)
	if err != nil || u.Scheme != "file" || (u.Host != "" && u.Host != "localhost") {
		return "", false
	}
	if !filepath.IsAbs(u.Path) || strings.ContainsRune(u.Path, 0) {
		return "", false
	}
	return filepath.Clean(u.Path), true
}

// fileURI is a file:// URI with everything but unreserved characters and "/" escaped.
func fileURI(path string) string {
	var b strings.Builder
	b.WriteString("file://")
	for _, c := range []byte(path) {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_', c == '.', c == '~', c == '/':
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// copiedFilesPayload is what x-special/gnome-copied-files holds for a copy of paths.
func copiedFilesPayload(paths []string) string {
	uris := make([]string, len(paths))
	for i, p := range paths {
		uris[i] = fileURI(p)
	}
	return "copy\n" + strings.Join(uris, "\n")
}

// --- sending ------------------------------------------------------------------------

type packed struct {
	data   []byte
	name   string
	bundle bool // data is a zip of several files or a folder
}

// packFiles turns copied paths into one clip: a single file as it is, anything
// else (several files, a folder) as a zip. limit is the most the hub accepts.
func packFiles(paths []string, limit int64) (packed, error) {
	if limit <= 0 || limit > agentClipCap {
		limit = agentClipCap
	}
	type item struct {
		path, name string
		info       fs.FileInfo
	}
	var items []item
	for _, p := range paths {
		info, err := os.Stat(p) // a copied symlink stands for what it points at
		if err != nil {
			return packed{}, err
		}
		items = append(items, item{p, filepath.Base(p), info})
	}
	if len(items) == 0 {
		return packed{}, errors.New("no files")
	}

	if len(items) == 1 && items[0].info.Mode().IsRegular() {
		it := items[0]
		switch size := it.info.Size(); {
		case size == 0:
			return packed{}, fmt.Errorf("%s is empty", it.name)
		case size > limit:
			return packed{}, tooLarge(it.name, size, limit)
		}
		data, err := os.ReadFile(it.path)
		if err != nil {
			return packed{}, err
		}
		if int64(len(data)) > limit { // it grew while we read it
			return packed{}, tooLarge(it.name, int64(len(data)), limit)
		}
		return packed{data: data, name: it.name}, nil
	}

	// Several items, or a folder: a zip with the copied items at its top level.
	// Entries are sorted and carry no timestamps, so the same copy zips to the same bytes.
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	var total int64
	entries := 0
	what := "the selection"
	if len(items) == 1 {
		what = items[0].name
	}
	used := map[string]bool{}
	for _, it := range items {
		top := uniqueName(it.name, used)
		add := func(rel string, path string, d fs.DirEntry) error {
			if entries++; entries > maxFileEntries {
				return fmt.Errorf("more than %d files", maxFileEntries)
			}
			name := top
			if rel != "." {
				name = top + "/" + filepath.ToSlash(rel)
			}
			switch {
			case d.IsDir():
				_, err := zw.CreateHeader(&zip.FileHeader{Name: name + "/", Method: zip.Store, Modified: zipEpoch})
				return err
			case d.Type()&fs.ModeSymlink != 0 || !d.Type().IsRegular():
				log.Printf("skipping %s: not a regular file", path) // links and sockets are not followed
				return nil
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			if total += info.Size(); total > limit {
				return tooLarge(what, total, limit)
			}
			w, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Deflate, Modified: zipEpoch})
			if err != nil {
				return err
			}
			f, err := os.Open(path)
			if err != nil {
				return err
			}
			defer f.Close()
			_, err = io.Copy(w, f)
			return err
		}
		if it.info.IsDir() {
			err := filepath.WalkDir(it.path, func(path string, d fs.DirEntry, err error) error {
				if err != nil {
					return err
				}
				rel, _ := filepath.Rel(it.path, path)
				return add(rel, path, d)
			})
			if err != nil {
				return packed{}, err
			}
		} else if it.info.Mode().IsRegular() {
			if err := add(".", it.path, fs.FileInfoToDirEntry(it.info)); err != nil {
				return packed{}, err
			}
		}
	}
	if err := zw.Close(); err != nil {
		return packed{}, err
	}
	if int64(buf.Len()) > limit {
		return packed{}, tooLarge(what, int64(buf.Len()), limit)
	}
	name := "clipboard-files.zip"
	if len(items) == 1 {
		name = items[0].name + ".zip"
	}
	return packed{data: buf.Bytes(), name: name, bundle: true}, nil
}

var zipEpoch = time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC)

func tooLarge(what string, size, limit int64) error {
	return fmt.Errorf("%s is %d bytes, over the hub's limit of %d", what, size, limit)
}

// uniqueName keeps two copied items with the same name (from different folders) apart.
func uniqueName(name string, used map[string]bool) string {
	for n, i := name, 2; ; i++ {
		if !used[n] {
			used[n] = true
			return n
		}
		n = fmt.Sprintf("%s (%d)", name, i)
	}
}

// --- receiving ----------------------------------------------------------------------

var stageDirRE = regexp.MustCompile(`^[0-9a-f]{16}$`)

// stage writes a received file clip under root/<id> and returns the top-level
// paths to offer on the clipboard. Only the newest clip's files are kept: older
// staged clips are removed, and nothing else in root is touched.
func stage(root, id, name string, data []byte, bundle bool) ([]string, error) {
	if !stageDirRE.MatchString(id) {
		return nil, fmt.Errorf("bad staging id %q", id)
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	dir := filepath.Join(root, id)
	os.RemoveAll(dir)
	if err := os.Mkdir(dir, 0o700); err != nil {
		return nil, err
	}
	var paths []string
	var err error
	if bundle {
		paths, err = unzipInto(dir, data)
	} else {
		p := filepath.Join(dir, safeName(name, "clipboard-file"))
		paths, err = []string{p}, os.WriteFile(p, data, 0o644)
	}
	if err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	if entries, err := os.ReadDir(root); err == nil {
		for _, e := range entries {
			if e.Name() != id && e.IsDir() && stageDirRE.MatchString(e.Name()) {
				os.RemoveAll(filepath.Join(root, e.Name()))
			}
		}
	}
	return paths, nil
}

// safeName reduces a name from the network to one plain file name: no
// directories, no control characters, a sane length.
func safeName(name, fallback string) string {
	name = strings.ReplaceAll(name, `\`, "/")
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	name = strings.TrimSpace(strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, name))
	for len(name) > 200 {
		_, n := utf8.DecodeLastRuneInString(name)
		name = name[:len(name)-n]
	}
	if name == "" || name == "." || name == ".." {
		return fallback
	}
	return name
}

// unzipInto extracts a received archive below dir and returns its top-level
// entries. Names that would escape dir are an error; links and special files
// are skipped; files are created without execute bits; the total size is capped.
func unzipInto(dir string, data []byte) ([]string, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if zr == nil { // an insecure name is reported but still readable; we check names ourselves
		return nil, err
	}
	if len(zr.File) > maxFileEntries {
		return nil, fmt.Errorf("archive holds more than %d files", maxFileEntries)
	}
	budget := maxExtract
	var tops []string
	seen := map[string]bool{}
	for _, f := range zr.File {
		name := strings.TrimSuffix(f.Name, "/")
		target := filepath.Join(dir, filepath.FromSlash(name))
		if !fs.ValidPath(name) || name == "." || strings.ContainsRune(name, 0) || !strings.HasPrefix(target, dir+string(filepath.Separator)) {
			return nil, fmt.Errorf("unsafe path %q in archive", f.Name)
		}
		switch {
		case f.FileInfo().IsDir():
			err = os.MkdirAll(target, 0o755)
		case f.Mode().IsRegular():
			err = extractFile(f, target, &budget)
		default:
			continue
		}
		if err != nil {
			return nil, err
		}
		if top, _, _ := strings.Cut(name, "/"); !seen[top] {
			seen[top] = true
			tops = append(tops, filepath.Join(dir, top))
		}
	}
	if len(tops) == 0 {
		return nil, errors.New("archive holds no files")
	}
	return tops, nil
}

func extractFile(f *zip.File, target string, budget *int64) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	n, err := io.Copy(out, io.LimitReader(rc, *budget+1))
	*budget -= n
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err == nil && *budget < 0 {
		err = fmt.Errorf("archive expands to more than %d bytes", maxExtract)
	}
	return err
}

// stageRoot is where received files wait to be pasted: CLIP_FILES_DIR, or the user's cache.
func stageRoot() string {
	dir := os.Getenv("CLIP_FILES_DIR")
	if dir == "" {
		cache, err := os.UserCacheDir()
		if err != nil {
			cache = os.TempDir()
		}
		dir = filepath.Join(cache, "central-clipboard", "files")
	}
	if abs, err := filepath.Abs(dir); err == nil {
		return abs
	}
	return filepath.Clean(dir)
}
