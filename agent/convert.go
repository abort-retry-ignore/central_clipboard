package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

func itoa(n int) string { return strconv.Itoa(n) }

func shaHex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// forClipboard turns what the hub holds into something Linux apps can paste:
// RTF becomes plain text, and photos (HEIC from an iPhone and the like) become
// PNG through ImageMagick when it is installed.
func forClipboard(mime string, data []byte) (string, []byte, bool) {
	switch {
	case strings.HasPrefix(mime, "text/"):
		return "text/plain", data, true
	case mime == "application/rtf":
		text := rtfToText(data)
		if strings.TrimSpace(text) == "" {
			return "", nil, false
		}
		return "text/plain", []byte(text), true
	case mime == "image/png":
		return mime, data, true
	case strings.HasPrefix(mime, "image/"):
		if png, ok := toPNG(mime, data); ok {
			return "image/png", png, true
		}
		switch mime { // apps can usually paste these as they are
		case "image/jpeg", "image/gif", "image/webp":
			return mime, data, true
		}
	}
	return "", nil, false
}

func toPNG(mime string, data []byte) ([]byte, bool) {
	in := "-"
	if mime == "image/heic" {
		in = "heic:-"
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	for _, bin := range []string{"magick", "convert"} {
		path, err := exec.LookPath(bin)
		if err != nil {
			continue
		}
		cmd := exec.CommandContext(ctx, path, in, "png:-")
		cmd.Stdin = bytes.NewReader(data)
		if out, err := cmd.Output(); err == nil && len(out) > 0 {
			return out, true
		}
	}
	return nil, false
}
