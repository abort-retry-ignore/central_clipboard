package main

import (
	"bufio"
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Content is what the local clipboard holds: text or a picture, or files that
// were copied in a file manager.
type Content struct {
	Mime  string // text/plain or an image type, with Data
	Data  []byte
	Files []string // absolute paths; Mime and Data are then empty
}

// Clipboard is the local clipboard. The real one drives wl-clipboard.
type Clipboard interface {
	// Read returns the current clipboard, or ok=false if it is empty, holds
	// something we do not sync, or is marked as a secret.
	Read() (Content, bool)
	Write(mime string, data []byte) error
	// WriteFiles puts files on the clipboard the way a file manager's copy does.
	WriteFiles(paths []string) error
	// Watch calls changed after every clipboard change until ctx ends or the watcher dies.
	Watch(ctx context.Context, changed func()) error
}

const secretHint = "x-kde-passwordManagerHint" // set by password managers on secrets

var textTypes = []string{"text/plain;charset=utf-8", "text/plain", "UTF8_STRING", "STRING", "TEXT"}

// File managers (Nautilus, Flea, Dolphin and the like) offer a copy as these,
// along with the paths as plain text, which is why files are checked first.
const gnomeCopiedFiles = "x-special/gnome-copied-files"

var fileTypes = []string{gnomeCopiedFiles, "text/uri-list"}

type wlClipboard struct{}

// wlEnv finds the compositor socket when the service started before it, or
// after it restarted under another name.
func wlEnv() []string {
	env := os.Environ()
	runtime := os.Getenv("XDG_RUNTIME_DIR")
	if runtime == "" {
		runtime = "/run/user/" + itoa(os.Getuid())
		env = append(env, "XDG_RUNTIME_DIR="+runtime)
	}
	if d := os.Getenv("WAYLAND_DISPLAY"); d != "" {
		if _, err := os.Stat(filepath.Join(runtime, d)); err == nil || filepath.IsAbs(d) {
			return env
		}
	}
	socks, _ := filepath.Glob(filepath.Join(runtime, "wayland-*"))
	sort.Strings(socks)
	for _, s := range socks {
		if !strings.HasSuffix(s, ".lock") {
			return append(env, "WAYLAND_DISPLAY="+filepath.Base(s))
		}
	}
	return env
}

func wlPaste(timeout time.Duration, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "wl-paste", args...)
	cmd.Env = wlEnv()
	return cmd.Output()
}

func (wlClipboard) Read() (Content, bool) {
	out, err := wlPaste(5*time.Second, "-l")
	if err != nil {
		return Content{}, false
	}
	types := strings.Fields(string(out))
	has := map[string]bool{}
	for _, t := range types {
		has[t] = true
	}
	if has[secretHint] {
		return Content{}, false
	}
	for _, t := range fileTypes { // files before text: a file manager's copy also offers the path as text
		if !has[t] {
			continue
		}
		if raw, err := wlPaste(5*time.Second, "-n", "-t", t); err == nil {
			if paths := parseCopiedFiles(string(raw)); len(paths) > 0 {
				return Content{Files: paths}, true
			}
		}
	}
	want := ""
	for _, t := range textTypes {
		if has[t] {
			want = t
			break
		}
	}
	mime := "text/plain"
	if want == "" { // images only when there is no text alongside
		mime = ""
		if has["image/png"] {
			mime = "image/png"
		} else {
			for _, t := range types {
				if strings.HasPrefix(t, "image/") {
					mime = t
					break
				}
			}
		}
		want = mime
	}
	if want == "" {
		return Content{}, false
	}
	data, err := wlPaste(15*time.Second, "-n", "-t", want)
	if err != nil || len(data) == 0 {
		return Content{}, false
	}
	return Content{Mime: mime, Data: data}, true
}

func (wlClipboard) Write(mime string, data []byte) error { return wlCopy(mime, data) }

// WriteFiles offers one type, since wl-copy offers only one: the one Nautilus and
// Flea paste files from.
func (wlClipboard) WriteFiles(paths []string) error {
	return wlCopy(gnomeCopiedFiles, []byte(copiedFilesPayload(paths)))
}

func wlCopy(mime string, data []byte) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	// wl-copy forks to keep serving the selection. Leaving stdout and stderr
	// unset keeps us from waiting on pipes the forked copy would inherit.
	cmd := exec.CommandContext(ctx, "wl-copy", "--type", mime)
	cmd.Env = wlEnv()
	cmd.Stdin = bytes.NewReader(data)
	return cmd.Run()
}

func (wlClipboard) Watch(ctx context.Context, changed func()) error {
	cmd := exec.CommandContext(ctx, "wl-paste", "--watch", "echo")
	cmd.Env = wlEnv()
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	sc := bufio.NewScanner(out)
	for sc.Scan() { // one line per clipboard change
		changed()
	}
	return cmd.Wait()
}
