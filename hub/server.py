#!/usr/bin/env python3
"""Central clipboard hub.

Holds the latest clip, serves it to the iPhone Shortcuts (same wire protocol as
Unayung/omarchy-iphone-clipboard: raw body in, image/png or text out, 204 when
empty), and pushes changes to desktop agents over Server-Sent Events.

    GET  /          latest clip (also /clip)
    POST /          replace the clip (also /clip)
    GET  /events    SSE: `hello` on connect, then `clip` on every change
    GET  /healthz   unauthenticated liveness probe

Auth: X-Token header, `Authorization: Bearer`, or ?t= query parameter.
"""
import hashlib
import io
import json
import os
import re
import secrets
import signal
import sys
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from urllib.parse import parse_qs, urlparse

try:
    from PIL import Image, ImageOps
    try:
        import pillow_heif
        pillow_heif.register_heif_opener()
    except ImportError:
        pass
except ImportError:
    Image = None

PING_SECONDS = 15
MIN_TOKEN_LEN = 16

PNG_MAGIC = b"\x89PNG\r\n\x1a\n"
HEIC_BRANDS = {b"heic", b"heix", b"heif", b"heim", b"heis", b"hevc", b"hevx", b"mif1", b"msf1"}


def parse_tokens(spec):
    """'phone:abc...,laptop1:def...' -> {token: name}. A bare token gets a numbered name."""
    tokens = {}
    for i, item in enumerate(filter(None, (s.strip() for s in spec.split(",")))):
        name, _, tok = item.rpartition(":")
        if len(tok) < MIN_TOKEN_LEN:
            raise SystemExit(f"CLIP_TOKENS: token #{i + 1} is shorter than {MIN_TOKEN_LEN} characters")
        tokens[tok] = name or f"device{i + 1}"
    if not tokens:
        raise SystemExit("CLIP_TOKENS is empty; set e.g. CLIP_TOKENS=phone:$(openssl rand -hex 24)")
    return tokens


# --- normalising what arrives -------------------------------------------------
# Adapted from the RTF/HEIC patch in Unayung/omarchy-iphone-clipboard PR #1.

def rtf_to_text(rtf):
    pattern = re.compile(
        r"\\([a-z]{1,32})(-?\d{1,10})?[ ]?|\\'([0-9a-f]{2})|\\([^a-z])|([{}])|[\r\n]+|(.)",
        re.I | re.S)
    destinations = {
        "fonttbl", "colortbl", "expandedcolortbl", "stylesheet", "info", "pict", "object",
        "header", "footer", "footnote", "filetbl", "listtable", "listoverridetable",
        "listname", "listtext", "levelnumbers", "leveltext", "levelmarker", "themedata",
        "generator", "revtbl", "xmlnstbl", "datastore", "colorschememapping",
    }
    special = {"par": "\n", "sect": "\n\n", "page": "\n\n", "line": "\n", "tab": "\t",
               "emdash": "\u2014", "endash": "\u2013", "emspace": "\u2003", "enspace": "\u2002",
               "bullet": "\u2022", "lquote": "\u2018", "rquote": "\u2019",
               "ldblquote": "\u201c", "rdblquote": "\u201d"}
    stack, out = [], []
    ignorable, ucskip, curskip = False, 1, 0
    for match in pattern.finditer(rtf):
        word, arg, hexc, char, brace, tchar = match.groups()
        if brace == "{":
            curskip = 0
            stack.append((ucskip, ignorable))
        elif brace == "}":
            curskip = 0
            if stack:
                ucskip, ignorable = stack.pop()
        elif char:
            curskip = 0
            if char == "~":
                if not ignorable:
                    out.append("\u00a0")
            elif char in "{}\\":
                if not ignorable:
                    out.append(char)
            elif char == "*":
                ignorable = True
        elif word:
            curskip = 0
            if word in destinations:
                ignorable = True
            elif ignorable:
                pass
            elif word in special:
                out.append(special[word])
            elif word == "uc":
                ucskip = int(arg)
            elif word == "u":
                c = int(arg)
                out.append(chr(c + 65536 if c < 0 else c))
                curskip = ucskip
        elif hexc:
            if curskip > 0:
                curskip -= 1
            elif not ignorable:
                out.append(bytes([int(hexc, 16)]).decode("cp1252", errors="replace"))
        elif tchar:
            if curskip > 0:
                curskip -= 1
            elif not ignorable:
                out.append(tchar)
    return "".join(out)


def sniff_image(body):
    """Return a mime type if `body` looks like an image, else None."""
    if body.startswith(PNG_MAGIC):
        return "image/png"
    if body.startswith(b"\xff\xd8\xff"):
        return "image/jpeg"
    if body[:4] == b"GIF8":
        return "image/gif"
    if body[:4] == b"RIFF" and body[8:12] == b"WEBP":
        return "image/webp"
    if body[:4] in (b"II*\x00", b"MM\x00*"):
        return "image/tiff"
    if len(body) > 12 and body[4:8] == b"ftyp" and body[8:12] in HEIC_BRANDS:
        return "image/heic"
    return None


def to_png(body):
    img = Image.open(io.BytesIO(body))
    img = ImageOps.exif_transpose(img)
    if img.mode not in ("RGB", "RGBA", "L", "LA"):
        img = img.convert("RGBA" if "A" in img.mode or "transparency" in img.info else "RGB")
    out = io.BytesIO()
    img.save(out, "PNG")
    return out.getvalue()


def normalize(body):
    """Raw upload -> (bytes, mime). Images become PNG, RTF becomes plain text."""
    if body[:5] == b"{\\rtf":
        text = rtf_to_text(body.decode("cp1252", errors="replace"))
        if text.strip():
            return text.encode(), "text/plain"
    mime = sniff_image(body)
    if mime:
        if mime != "image/png" and Image is not None:
            try:
                return to_png(body), "image/png"
            except Exception as e:  # unreadable or too large: keep the original bytes
                print(f"png conversion failed ({e}); keeping {mime}", file=sys.stderr, flush=True)
        return body, mime
    try:
        body.decode("utf-8")
        return body, "text/plain"
    except UnicodeDecodeError:
        return body, "application/octet-stream"


# --- storage ------------------------------------------------------------------

class Clip:
    __slots__ = ("seq", "mime", "data", "sha", "source", "ts")

    def __init__(self, seq, mime, data, sha, source, ts):
        self.seq, self.mime, self.data, self.sha, self.source, self.ts = seq, mime, data, sha, source, ts

    def meta(self):
        return {"seq": self.seq, "mime": self.mime, "size": len(self.data), "sha": self.sha,
                "source": self.source, "ts": self.ts}


class Store:
    """The current clip, persisted under `data_dir` so a restart keeps it."""

    def __init__(self, data_dir):
        self.dir = Path(data_dir)
        self.dir.mkdir(parents=True, exist_ok=True)
        self.cond = threading.Condition()
        self.clip = self._load()

    def _load(self):
        try:
            meta = json.loads((self.dir / "current.json").read_text())
            data = (self.dir / "current.bin").read_bytes()
            if hashlib.sha256(data).hexdigest() == meta["sha"]:
                return Clip(meta["seq"], meta["mime"], data, meta["sha"], meta["source"], meta["ts"])
        except (OSError, ValueError, KeyError):
            pass
        return Clip(0, "", b"", "", "", 0)

    def _persist(self, clip):
        for name, content in (("current.bin", clip.data), ("current.json", json.dumps(clip.meta()).encode())):
            tmp = self.dir / (name + ".tmp")
            tmp.write_bytes(content)
            os.replace(tmp, self.dir / name)

    def set(self, data, mime, source):
        """Replace the clip. Returns (clip, changed); identical content is a no-op."""
        sha = hashlib.sha256(data).hexdigest()
        with self.cond:
            if sha == self.clip.sha:
                return self.clip, False
            clip = Clip(self.clip.seq + 1, mime, data, sha, source, time.time())
            self._persist(clip)
            self.clip = clip
            self.cond.notify_all()
            return clip, True

    def wait_newer(self, seq, timeout):
        with self.cond:
            if self.clip.seq <= seq:
                self.cond.wait(timeout)
            return self.clip if self.clip.seq > seq else None


# --- http ---------------------------------------------------------------------

class Handler(BaseHTTPRequestHandler):
    timeout = 30  # slow or stalled clients must not pin a thread
    store = None
    tokens = {}
    max_bytes = 50 << 20
    who = None  # device name for the token on this request

    def log_request(self, code="-", size="-"):  # never log ?t=token
        self.log_message('"%s %s" %s', self.command, urlparse(self.path).path, code)

    def log_message(self, fmt, *args):
        print(f"{self.client_address[0]} {self.who or '-'} {fmt % args}", file=sys.stderr, flush=True)

    def authenticate(self):
        given = (self.headers.get("X-Token")
                 or self.headers.get("Authorization", "").removeprefix("Bearer ").strip()
                 or parse_qs(urlparse(self.path).query).get("t", [""])[0])
        for token, name in self.tokens.items():
            if secrets.compare_digest(given, token):
                self.who = name
                return True
        return False

    def reply(self, code, body=b"", mime="text/plain; charset=utf-8", headers=()):
        self.send_response(code)
        for h in headers:
            self.send_header(*h)
        if body:
            self.send_header("Content-Type", mime)
            self.send_header("Content-Length", str(len(body)))
        elif code != 204:
            self.send_header("Content-Length", "0")
        self.end_headers()
        if body:
            self.wfile.write(body)

    def do_GET(self):
        path = urlparse(self.path).path
        if path == "/healthz":
            return self.reply(200, b"ok")
        if not self.authenticate():
            return self.reply(403)
        if path in ("/", "/clip"):
            clip = self.store.clip
            if not clip.data:
                return self.reply(204)
            mime = clip.mime + ("; charset=utf-8" if clip.mime.startswith("text/") else "")
            return self.reply(200, clip.data, mime,
                              [("X-Clip-Seq", str(clip.seq)), ("X-Clip-Sha", clip.sha),
                               ("X-Clip-Source", clip.source)])
        if path == "/events":
            return self.stream_events()
        self.reply(404)

    def do_POST(self):
        if urlparse(self.path).path not in ("/", "/clip"):
            return self.reply(404)
        if not self.authenticate():
            return self.reply(403)
        try:
            n = int(self.headers.get("Content-Length") or 0)
        except ValueError:
            n = 0
        if n <= 0:
            return self.reply(400)
        if n > self.max_bytes:
            return self.reply(413)
        data, mime = normalize(self.rfile.read(n))
        clip, changed = self.store.set(data, mime, self.who)
        self.reply(204, headers=[("X-Clip-Seq", str(clip.seq)), ("X-Clip-Sha", clip.sha)])

    def stream_events(self):
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Cache-Control", "no-cache")
        self.send_header("X-Accel-Buffering", "no")
        self.end_headers()

        def send(event, payload):
            self.wfile.write(f"event: {event}\ndata: {json.dumps(payload)}\n\n".encode())
            self.wfile.flush()

        try:
            clip = self.store.clip
            send("hello", {"you": self.who, "clip": clip.meta() if clip.data else None})
            seq = clip.seq
            while True:
                clip = self.store.wait_newer(seq, PING_SECONDS)
                if clip is None:
                    self.wfile.write(b": ping\n\n")
                    self.wfile.flush()
                else:
                    seq = clip.seq
                    send("clip", clip.meta())
        except (BrokenPipeError, ConnectionResetError, TimeoutError, OSError):
            pass


def make_server(host, port, data_dir, tokens, max_bytes):
    handler = type("BoundHandler", (Handler,), {
        "store": Store(data_dir), "tokens": tokens, "max_bytes": max_bytes})
    server = ThreadingHTTPServer((host, port), handler)
    server.daemon_threads = True
    return server


def main():
    tokens = parse_tokens(os.environ.get("CLIP_TOKENS", ""))
    server = make_server(
        os.environ.get("CLIP_HOST", "0.0.0.0"),
        int(os.environ.get("CLIP_PORT", 8766)),
        os.environ.get("CLIP_DATA", "/data"),
        tokens,
        int(os.environ.get("CLIP_MAX_BYTES", 50 << 20)))
    signal.signal(signal.SIGTERM, lambda *_: threading.Thread(target=server.shutdown).start())
    print(f"hub listening on {server.server_address[0]}:{server.server_address[1]}; "
          f"devices: {', '.join(sorted(tokens.values()))}", file=sys.stderr, flush=True)
    server.serve_forever()


if __name__ == "__main__":
    main()
