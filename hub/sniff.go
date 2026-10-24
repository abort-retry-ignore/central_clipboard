package main

import (
	"bytes"
	"strings"
	"unicode/utf8"
)

var heicBrands = []string{"heic", "heix", "heif", "heim", "heis", "hevc", "hevx", "mif1", "msf1"}

// sniff names what an upload is, and the extension a file of that kind usually
// has. The hub stores bytes as they arrive; agents convert whatever they cannot
// paste (HEIC, RTF) on their own side.
func sniff(b []byte) (mime, ext string) {
	has := func(prefix string) bool { return bytes.HasPrefix(b, []byte(prefix)) }
	riff := func(kind string) bool { return len(b) >= 12 && string(b[:4]) == "RIFF" && string(b[8:12]) == kind }
	switch {
	case has("\x89PNG\r\n\x1a\n"):
		return "image/png", ".png"
	case has("\xff\xd8\xff"):
		return "image/jpeg", ".jpg"
	case has("GIF8"):
		return "image/gif", ".gif"
	case riff("WEBP"):
		return "image/webp", ".webp"
	case riff("WAVE"):
		return "audio/wav", ".wav"
	case riff("AVI "):
		return "video/x-msvideo", ".avi"
	case has("II*\x00"), has("MM\x00*"):
		return "image/tiff", ".tiff"
	case len(b) > 12 && string(b[4:8]) == "ftyp":
		return sniffFtyp(string(b[8:12]))
	case has(`{\rtf`):
		return "application/rtf", ".rtf"
	case has("%PDF-"):
		return "application/pdf", ".pdf"
	case has("PK\x03\x04"):
		return "application/zip", ".zip"
	case has("\x1f\x8b"):
		return "application/gzip", ".gz"
	case has("7z\xbc\xaf\x27\x1c"):
		return "application/x-7z-compressed", ".7z"
	case has("Rar!\x1a\x07"):
		return "application/vnd.rar", ".rar"
	case has("\x1a\x45\xdf\xa3"): // EBML: WebM or Matroska
		if bytes.Contains(b[:min(len(b), 64)], []byte("webm")) {
			return "video/webm", ".webm"
		}
		return "video/x-matroska", ".mkv"
	case len(b) >= 10 && has("ID3") && b[3] >= 2 && b[3] <= 4 && b[4] == 0, // ID3v2 tag
		len(b) >= 2 && b[0] == 0xff && b[1]&0xe0 == 0xe0: // bare MPEG audio frame
		return "audio/mpeg", ".mp3"
	case has("fLaC"):
		return "audio/flac", ".flac"
	case has("OggS"):
		return "audio/ogg", ".ogg"
	case len(b) > 262 && string(b[257:262]) == "ustar":
		return "application/x-tar", ".tar"
	case bytes.IndexByte(b, 0) < 0 && utf8.Valid(b):
		return "text/plain", ".txt"
	}
	return "application/octet-stream", ".bin"
}

func sniffFtyp(brand string) (string, string) {
	switch {
	case isHeicBrand(brand):
		return "image/heic", ".heic"
	case brand == "avif", brand == "avis":
		return "image/avif", ".avif"
	case brand == "M4A ", brand == "M4B ", brand == "M4P ":
		return "audio/mp4", ".m4a"
	case brand == "qt  ":
		return "video/quicktime", ".mov"
	case brand == "M4V ":
		return "video/x-m4v", ".m4v"
	case strings.HasPrefix(brand, "3gp"):
		return "video/3gpp", ".3gp"
	}
	return "video/mp4", ".mp4"
}

func isHeicBrand(s string) bool {
	for _, b := range heicBrands {
		if s == b {
			return true
		}
	}
	return false
}

// isFileType reports whether a clip of this type is a file as opposed to
// something an agent can paste directly: text, a picture, or rich text.
func isFileType(mime string) bool {
	return !strings.HasPrefix(mime, "text/") && !strings.HasPrefix(mime, "image/") && mime != "application/rtf"
}
