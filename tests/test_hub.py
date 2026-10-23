import http.client
import io
import json
import sys
import tempfile
import threading
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent / "hub"))
import server  # noqa: E402

PHONE = "p" * 32
LAPTOP = "l" * 32
TOKENS = {PHONE: "phone", LAPTOP: "laptop1"}


class HubCase(unittest.TestCase):
    def start(self, data_dir=None, max_bytes=1 << 20):
        self.data_dir = data_dir or tempfile.mkdtemp()
        self.srv = server.make_server("127.0.0.1", 0, self.data_dir, TOKENS, max_bytes)
        threading.Thread(target=self.srv.serve_forever, daemon=True).start()
        self.port = self.srv.server_address[1]
        self.addCleanup(self.stop)

    def stop(self):
        self.srv.shutdown()
        self.srv.server_close()

    def setUp(self):
        self.start()

    def req(self, method, path="/", body=None, token=PHONE, headers=None):
        conn = http.client.HTTPConnection("127.0.0.1", self.port, timeout=5)
        h = dict(headers or {})
        if token:
            h["X-Token"] = token
        conn.request(method, path, body=body, headers=h)
        r = conn.getresponse()
        data = r.read()
        conn.close()
        return r, data


class Protocol(HubCase):
    def test_healthz_needs_no_token(self):
        r, body = self.req("GET", "/healthz", token=None)
        self.assertEqual((r.status, body), (200, b"ok"))

    def test_rejects_missing_and_wrong_token(self):
        self.assertEqual(self.req("GET", token=None)[0].status, 403)
        self.assertEqual(self.req("GET", token="x" * 32)[0].status, 403)
        self.assertEqual(self.req("POST", body=b"hi", token="x" * 32)[0].status, 403)

    def test_query_and_bearer_tokens_work(self):
        self.assertEqual(self.req("POST", f"/?t={PHONE}", b"hi", token=None)[0].status, 204)
        r, _ = self.req("GET", "/", token=None, headers={"Authorization": f"Bearer {LAPTOP}"})
        self.assertEqual(r.status, 200)

    def test_empty_hub_returns_204(self):
        self.assertEqual(self.req("GET")[0].status, 204)

    def test_text_roundtrip_and_source(self):
        r, _ = self.req("POST", body="héllo wörld".encode())
        self.assertEqual(r.status, 204)
        r, body = self.req("GET", token=LAPTOP)
        self.assertEqual(body.decode(), "héllo wörld")
        self.assertEqual(r.getheader("Content-Type"), "text/plain; charset=utf-8")
        self.assertEqual(r.getheader("X-Clip-Source"), "phone")

    def test_clip_path_alias(self):
        self.req("POST", "/clip", b"aliased")
        self.assertEqual(self.req("GET", "/clip")[1], b"aliased")

    def test_identical_content_does_not_bump_seq(self):
        a, _ = self.req("POST", body=b"same")
        b, _ = self.req("POST", body=b"same", token=LAPTOP)
        c, _ = self.req("POST", body=b"different")
        self.assertEqual(a.getheader("X-Clip-Seq"), b.getheader("X-Clip-Seq"))
        self.assertNotEqual(a.getheader("X-Clip-Seq"), c.getheader("X-Clip-Seq"))

    def test_empty_and_oversized_bodies(self):
        self.assertEqual(self.req("POST", body=b"")[0].status, 400)
        self.assertEqual(self.req("POST", body=b"x" * ((1 << 20) + 1))[0].status, 413)

    def test_unknown_paths(self):
        self.assertEqual(self.req("GET", "/nope")[0].status, 404)
        self.assertEqual(self.req("POST", "/nope", b"x")[0].status, 404)


class Normalising(HubCase):
    def test_rtf_becomes_plain_text(self):
        # RTF 舒 is an em dash; the backslash is split off so no editor mangles the escape
        rtf = (b"{\\rtf1\\ansi{\\fonttbl{\\f0 Helvetica;}}\\f0 Hello \\" b"u8212? world\\par second line}")
        self.req("POST", body=rtf)
        r, body = self.req("GET")
        self.assertEqual(body.decode(), "Hello — world\nsecond line")
        self.assertTrue(r.getheader("Content-Type").startswith("text/plain"))

    def test_jpeg_becomes_png(self):
        from PIL import Image
        buf = io.BytesIO()
        Image.new("RGB", (4, 3), (200, 10, 10)).save(buf, "JPEG")
        self.req("POST", body=buf.getvalue())
        r, body = self.req("GET")
        self.assertEqual(r.getheader("Content-Type"), "image/png")
        self.assertEqual(Image.open(io.BytesIO(body)).size, (4, 3))

    def test_heic_becomes_png(self):
        import pillow_heif
        from PIL import Image
        buf = io.BytesIO()
        pillow_heif.from_pillow(Image.new("RGB", (8, 8), (10, 200, 10))).save(buf, format="HEIF")
        data = buf.getvalue()
        self.assertEqual(server.sniff_image(data), "image/heic")
        self.req("POST", body=data)
        r, body = self.req("GET")
        self.assertEqual(r.getheader("Content-Type"), "image/png")
        self.assertEqual(Image.open(io.BytesIO(body)).size, (8, 8))

    def test_png_is_stored_untouched(self):
        from PIL import Image
        buf = io.BytesIO()
        Image.new("RGBA", (2, 2), (1, 2, 3, 4)).save(buf, "PNG")
        self.req("POST", body=buf.getvalue())
        self.assertEqual(self.req("GET")[1], buf.getvalue())

    def test_binary_blob_keeps_octet_stream(self):
        self.req("POST", body=b"\x00\x01\xfe\xff binary")
        r, body = self.req("GET")
        self.assertEqual(r.getheader("Content-Type"), "application/octet-stream")
        self.assertEqual(body, b"\x00\x01\xfe\xff binary")


class Events(HubCase):
    def open_stream(self, token=LAPTOP):
        conn = http.client.HTTPConnection("127.0.0.1", self.port, timeout=5)
        conn.request("GET", "/events", headers={"X-Token": token})
        r = conn.getresponse()
        self.addCleanup(conn.close)
        return r

    @staticmethod
    def next_event(resp):
        event, data = None, None
        while True:
            line = resp.readline().decode().rstrip("\n")
            if line.startswith("event: "):
                event = line[7:]
            elif line.startswith("data: "):
                data = json.loads(line[6:])
            elif line == "" and event:
                return event, data

    def test_events_require_token(self):
        conn = http.client.HTTPConnection("127.0.0.1", self.port, timeout=5)
        conn.request("GET", "/events")
        self.assertEqual(conn.getresponse().status, 403)

    def test_hello_on_empty_hub(self):
        event, data = self.next_event(self.open_stream())
        self.assertEqual(event, "hello")
        self.assertEqual(data, {"you": "laptop1", "clip": None})

    def test_hello_carries_current_clip_then_changes_stream(self):
        self.req("POST", body=b"first")
        stream = self.open_stream()
        event, data = self.next_event(stream)
        self.assertEqual(event, "hello")
        self.assertEqual(data["clip"]["source"], "phone")
        self.assertEqual(data["clip"]["size"], 5)

        self.req("POST", body=b"second", token=LAPTOP)
        event, data = self.next_event(stream)
        self.assertEqual(event, "clip")
        self.assertEqual((data["source"], data["size"]), ("laptop1", 6))

    def test_duplicate_post_emits_no_event(self):
        self.req("POST", body=b"one")
        stream = self.open_stream()
        self.next_event(stream)
        self.req("POST", body=b"one")
        self.req("POST", body=b"two")
        event, data = self.next_event(stream)
        self.assertEqual(data["size"], 3)
        self.assertEqual(data["sha"], self.req("GET")[0].getheader("X-Clip-Sha"))


class Persistence(HubCase):
    def test_clip_survives_restart_and_seq_continues(self):
        self.req("POST", body=b"keep me")
        seq = int(self.req("GET")[0].getheader("X-Clip-Seq"))
        self.stop()
        self.start(data_dir=self.data_dir)
        r, body = self.req("GET")
        self.assertEqual(body, b"keep me")
        self.assertEqual(int(r.getheader("X-Clip-Seq")), seq)
        r, _ = self.req("POST", body=b"next")
        self.assertEqual(int(r.getheader("X-Clip-Seq")), seq + 1)

    def test_corrupt_state_starts_empty(self):
        self.req("POST", body=b"keep me")
        self.stop()
        (Path(self.data_dir) / "current.bin").write_bytes(b"tampered")
        self.start(data_dir=self.data_dir)
        self.assertEqual(self.req("GET")[0].status, 204)


class Tokens(unittest.TestCase):
    def test_parse(self):
        t = "a" * 16
        self.assertEqual(server.parse_tokens(f"phone:{t}, laptop:{t[::-1]}b"),
                         {t: "phone", t[::-1] + "b": "laptop"})
        self.assertEqual(server.parse_tokens(t), {t: "device1"})

    def test_rejects_short_or_empty(self):
        with self.assertRaises(SystemExit):
            server.parse_tokens("phone:short")
        with self.assertRaises(SystemExit):
            server.parse_tokens("")


if __name__ == "__main__":
    unittest.main()
