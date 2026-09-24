"""HTTP API of the sidecar (contract: internal/studio/types.go, Segmenter):

    POST /v1/mask?box=x0,y0,x1,y1   body: the JPEG (Content-Type: image/jpeg)
      200 {"width","height","maskPng","stats","timingsMs","models","details"}
      400 {"error":"…"}  bad image or box      411/413  no or too large Content-Length
      503 {"error":"busy"|"not ready"} + Retry-After: 1 (one inference at a time)
    GET /healthz → 200 {"ok":true}             GET /readyz → 200 once warmed up, else 503
    GET /v1/info → models, threads, memory     (for the bench and operators)

The standard library server runs a thread per connection; a lock lets one request at a
time into the models, the others get 503 at once (the Go client retries). Every response
closes the connection, so a graceful stop never waits for idle keep-alive sockets.
"""

import base64
import io
import json
import resource
import sys
import threading
import time
import traceback
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import parse_qs, urlsplit

import cv2
from PIL import Image

from . import logs, masks

MIN_SIDE_PX = 32


class State:
    """Lifecycle (loading → ready → stopping, or failed) and the inference lock."""

    def __init__(self):
        self.phase = "loading"
        self.busy = threading.Lock()
        self.requests = 0
        self.info = {}

    @property
    def ready(self):
        return self.phase == "ready"


def decode_jpeg(body, max_pixels):
    """The request body → RGB PIL image. EXIF orientation is ignored on purpose: the mask
    must match the pixels as Go's image/jpeg decodes them (the exported JPEG is upright)."""
    if len(body) < 4 or body[:3] != b"\xff\xd8\xff":
        raise masks.InputError("body is not a JPEG")
    try:
        im = Image.open(io.BytesIO(body), formats=["JPEG"])
    except Exception as e:  # noqa: BLE001 - PIL raises many types for bad input
        raise masks.InputError(f"cannot read the JPEG: {e}") from None
    w, h = im.size
    if w * h > max_pixels:
        raise masks.InputError(f"image is {w}x{h}, more than {max_pixels} pixels")
    if min(w, h) < MIN_SIDE_PX:
        raise masks.InputError(f"image is {w}x{h}, smaller than {MIN_SIDE_PX} px on a side")
    try:
        im.load()
        return im.convert("RGB")
    except Exception as e:  # noqa: BLE001
        raise masks.InputError(f"cannot decode the JPEG: {e}") from None


def encode_png(alpha):
    ok, buf = cv2.imencode(".png", alpha, [cv2.IMWRITE_PNG_COMPRESSION, 3])
    if not ok:
        raise RuntimeError("PNG encode failed")
    return buf.tobytes()


def memory():
    """Memory in bytes: the process's current and peak RSS (+ anonymous part on Linux) and
    the container's cgroup v2 current/peak when readable (the cgroup counts page cache)."""
    out = {}
    for key, path in (("cgroupBytes", "/sys/fs/cgroup/memory.current"), ("cgroupPeakBytes", "/sys/fs/cgroup/memory.peak")):
        try:
            with open(path, encoding="ascii") as f:
                out[key] = int(f.read().strip())
        except (OSError, ValueError):
            pass
    try:
        with open("/proc/self/status", encoding="ascii") as f:
            for line in f:
                k, _, v = line.partition(":")
                if k in ("VmRSS", "RssAnon"):
                    out["rssBytes" if k == "VmRSS" else "rssAnonBytes"] = int(v.split()[0]) * 1024
    except OSError:
        pass
    r = resource.getrusage(resource.RUSAGE_SELF).ru_maxrss
    out["rssPeakBytes"] = r if sys.platform == "darwin" else r * 1024
    return out


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    server_version = "v2i-studio"
    sys_version = ""
    timeout = 60  # socket timeout: a stalled client cannot hold a thread forever

    # -- plumbing ------------------------------------------------------------------------

    def _send(self, status, payload, headers=None):
        body = json.dumps(payload, separators=(",", ":")).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.send_header("Connection", "close")
        for k, v in (headers or {}).items():
            self.send_header(k, v)
        self.end_headers()
        self.wfile.write(body)
        self.close_connection = True
        self._status = status

    def _error(self, status, msg, headers=None):
        self._send(status, {"error": msg}, headers)

    def log_request(self, code="-", size="-"):
        pass  # one JSON line per request is written by _done

    def log_message(self, format, *args):  # noqa: A002 - BaseHTTPRequestHandler's name
        logs.warn("http", detail=format % args, remote=self.client_address[0])

    def _done(self, t0, **fields):
        path = urlsplit(self.path).path
        lvl = logs.debug if path in ("/healthz", "/readyz", "/v1/info") else logs.info
        lvl("request", method=self.command, path=path, status=self._status, ms=int((time.perf_counter() - t0) * 1000), **fields)

    def _route(self, method):
        t0 = time.perf_counter()
        self._status = 0
        self._fields = {}
        path = urlsplit(self.path).path
        try:
            if method == "GET" and path == "/healthz":
                self._send(200, {"ok": True})
            elif method == "GET" and path == "/readyz":
                st = self.server.state
                if st.ready:
                    self._send(200, {"ready": True})
                else:
                    self._send(503, {"ready": False, "phase": st.phase})
            elif method == "GET" and path == "/v1/info":
                self._send(200, self._info())
            elif path == "/v1/mask":
                if method != "POST":
                    self._error(405, "method not allowed", {"Allow": "POST"})
                else:
                    self._mask()
            elif path in ("/healthz", "/readyz", "/v1/info"):
                self._error(405, "method not allowed", {"Allow": "GET"})
            else:
                self._error(404, "not found")
        except (BrokenPipeError, ConnectionResetError):
            self.close_connection = True
            self._status = self._status or 499
        except Exception:  # noqa: BLE001 - never let a request kill the thread silently
            logs.error("request failed", path=path, error=traceback.format_exc(limit=5))
            if self._status == 0:
                try:
                    self._error(500, "internal error")
                except OSError:
                    self.close_connection = True
        self._done(t0, **self._fields)

    def do_GET(self):  # noqa: N802
        self._route("GET")

    def do_POST(self):  # noqa: N802
        self._route("POST")

    def do_PUT(self):  # noqa: N802
        self._route("PUT")

    def do_DELETE(self):  # noqa: N802
        self._route("DELETE")

    def do_PATCH(self):  # noqa: N802
        self._route("PATCH")

    def _info(self):
        st = self.server.state
        return {"phase": st.phase, "requests": st.requests, **st.info, **memory()}

    # -- POST /v1/mask ---------------------------------------------------------------------

    def _read_body(self):
        """Returns the body, or None after answering 411/413/400."""
        if self.headers.get("Transfer-Encoding"):
            self._error(411, "Content-Length required (no chunked bodies)")
            return None
        raw = self.headers.get("Content-Length")
        if raw is None:
            self._error(411, "Content-Length required")
            return None
        try:
            n = int(raw)
        except ValueError:
            self._error(400, "bad Content-Length")
            return None
        if n < 0:
            self._error(400, "bad Content-Length")
            return None
        if n > self.server.max_body_bytes:
            # not read: the connection is closed after the answer
            self._error(413, f"body larger than {self.server.max_body_bytes} bytes")
            return None
        body = self.rfile.read(n)
        if len(body) != n:
            self._error(400, "body shorter than Content-Length")
            return None
        return body

    def _mask(self):
        q = parse_qs(urlsplit(self.path).query, keep_blank_values=True)
        body = self._read_body()
        if body is None:
            return
        boxes = q.get("box", [])
        if len(boxes) > 1:
            return self._error(400, "box given more than once")
        try:
            box = masks.parse_box(boxes[0] if boxes else None)
        except masks.InputError as e:
            return self._error(400, str(e))
        ctype = (self.headers.get("Content-Type") or "").split(";")[0].strip().lower()
        if ctype != "image/jpeg":
            return self._error(400, "Content-Type must be image/jpeg")
        st = self.server.state
        if not st.ready:
            return self._error(503, "not ready", {"Retry-After": "1"})
        if not st.busy.acquire(blocking=False):
            self._fields = {"busy": True}
            return self._error(503, "busy", {"Retry-After": "1"})
        try:
            st.requests += 1
            self._mask_locked(body, box)
        finally:
            st.busy.release()

    def _mask_locked(self, body, box):
        t0 = time.perf_counter()
        try:
            pil = decode_jpeg(body, self.server.max_pixels)
            masks.check_box_size(box, *pil.size)
        except masks.InputError as e:
            return self._error(400, str(e))
        t1 = time.perf_counter()
        res = self.server.runner.mask(pil, box)
        t2 = time.perf_counter()
        png = encode_png(res["alpha"])
        t3 = time.perf_counter()
        tim = dict(res["timings"])
        tim.update(decode=int((t1 - t0) * 1000), png=int((t3 - t2) * 1000), total=int((t3 - t0) * 1000))
        w, h = pil.size
        self._send(
            200,
            {
                "width": w,
                "height": h,
                "maskPng": base64.b64encode(png).decode("ascii"),
                "stats": res["stats"],
                "timingsMs": tim,
                "models": self.server.runner.models,
                "details": res["details"],
            },
        )
        s = res["stats"]
        self._fields = {
            "width": w,
            "height": h,
            "timingsMs": tim,
            "iou": s["iou"],
            "rescued": s["rescuedFraction"],
            "coverage": s["coverage"],
        }


class Server(ThreadingHTTPServer):
    """block_on_close (the default) makes server_close() wait for running requests."""

    daemon_threads = False
    allow_reuse_address = True

    def __init__(self, addr, runner, state, max_body_bytes, max_pixels):
        super().__init__(addr, Handler)
        self.runner = runner
        self.state = state
        self.max_body_bytes = max_body_bytes
        self.max_pixels = max_pixels
