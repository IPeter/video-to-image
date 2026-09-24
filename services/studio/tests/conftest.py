import http.client
import io
import json
import threading

import numpy as np
import pytest
from PIL import Image

from v2i_studio import masks
from v2i_studio.server import Server, State

MODELS = {"birefnet": "sha256:" + "a" * 64, "sam": "sha256:" + "b" * 64}


class StubRunner:
    """Stands in for models.Runner: the mask is an ellipse inscribed in the box. With
    gate set, mask() blocks until gate is set (entered is set on entry)."""

    def __init__(self):
        self.models = dict(MODELS)
        self.calls = []
        self.gate = None
        self.entered = threading.Event()
        self.fail = False

    def mask(self, pil, box):
        self.calls.append((pil.size, box))
        self.entered.set()
        if self.gate is not None:
            self.gate.wait(10)
        if self.fail:
            raise RuntimeError("model exploded")
        W, H = pil.size
        yy, xx = np.mgrid[0:H, 0:W]
        cx, cy = (box[0] + box[2]) / 2 * W, (box[1] + box[3]) / 2 * H
        rx, ry = (box[2] - box[0]) / 2 * W, (box[3] - box[1]) / 2 * H
        alpha = ((((xx - cx) / rx) ** 2 + ((yy - cy) / ry) ** 2) <= 1).astype(np.uint8) * 255
        stats, details = masks.mask_stats(alpha, alpha, alpha, box, 0.0)
        return {"alpha": alpha, "stats": stats, "details": details, "timings": {"birefnet": 1, "sam": 2}}

    def close(self):
        pass


def jpeg_bytes(w=320, h=240, color=(120, 130, 140), mode="RGB"):
    buf = io.BytesIO()
    Image.new(mode, (w, h), color if mode == "RGB" else 128).save(buf, "JPEG", quality=90)
    return buf.getvalue()


class Client:
    def __init__(self, port):
        self.port = port

    def request(self, method, path, body=None, headers=None):
        c = http.client.HTTPConnection("127.0.0.1", self.port, timeout=10)
        c.request(method, path, body=body, headers=headers or {})
        r = c.getresponse()
        data = r.read()
        c.close()
        try:
            payload = json.loads(data)
        except ValueError:
            payload = data
        return r.status, dict(r.getheaders()), payload

    def mask(self, body, box="0.1,0.2,0.9,0.8", ctype="image/jpeg"):
        path = "/v1/mask" if box is None else f"/v1/mask?box={box}"
        return self.request("POST", path, body, {"Content-Type": ctype})


@pytest.fixture
def server():
    """A running Server with a StubRunner in phase "ready" (small limits for the tests)."""
    runner = StubRunner()
    state = State()
    state.phase = "ready"
    srv = Server(("127.0.0.1", 0), runner, state, max_body_bytes=2 << 20, max_pixels=4_000_000)
    t = threading.Thread(target=srv.serve_forever, kwargs={"poll_interval": 0.05}, daemon=True)
    t.start()
    srv.client = Client(srv.server_address[1])
    try:
        yield srv
    finally:
        if runner.gate is not None:
            runner.gate.set()
        srv.shutdown()
        srv.server_close()
