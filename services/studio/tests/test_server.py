import base64
import http.client
import io
import json
import os
import signal
import socket
import subprocess
import sys
import textwrap
import threading
import time
from pathlib import Path

import pytest
from PIL import Image

from conftest import MODELS, jpeg_bytes
from test_masks import CONTRACT_KEYS
from v2i_studio import logs

SERVICE_DIR = Path(__file__).resolve().parent.parent


def test_healthz_and_readyz(server):
    c = server.client
    assert c.request("GET", "/healthz")[::2] == (200, {"ok": True})
    assert c.request("GET", "/readyz")[::2] == (200, {"ready": True})
    for phase in ("loading", "stopping", "failed"):
        server.state.phase = phase
        assert c.request("GET", "/healthz")[0] == 200
        assert c.request("GET", "/readyz")[::2] == (503, {"ready": False, "phase": phase})


def test_mask_ok_matches_contract(server):
    status, headers, p = server.client.mask(jpeg_bytes(320, 240), "0.1,0.2,0.9,0.8")
    assert status == 200
    assert headers["Content-Type"] == "application/json"
    assert (p["width"], p["height"]) == (320, 240)
    png = Image.open(io.BytesIO(base64.b64decode(p["maskPng"])))
    assert png.format == "PNG" and png.mode == "L" and png.size == (320, 240)
    assert png.getpixel((160, 120)) == 255 and png.getpixel((2, 2)) == 0
    assert set(p["stats"]) == CONTRACT_KEYS
    assert set(p["stats"]["edgeContact"]) == {"left", "right", "top", "bottom"}
    assert {"birefnet", "sam", "total", "decode", "png"} <= set(p["timingsMs"])
    assert p["models"] == MODELS
    assert server.runner.calls == [((320, 240), (0.1, 0.2, 0.9, 0.8))]


def test_mask_converts_grayscale_jpeg_to_rgb(server):
    status, _, p = server.client.mask(jpeg_bytes(100, 80, mode="L"), "0,0,1,1")
    assert status == 200 and (p["width"], p["height"]) == (100, 80)


def _png_bytes():
    buf = io.BytesIO()
    Image.new("RGB", (64, 64)).save(buf, "PNG")
    return buf.getvalue()


@pytest.mark.parametrize(
    "body, box, ctype, msg",
    [
        (None, None, "image/jpeg", "missing box"),
        (None, "0.1,0.2,0.9", "image/jpeg", "four comma-separated"),
        (None, "0.9,0.2,0.1,0.8", "image/jpeg", "0 <= x0 < x1"),
        (None, "0.1,0.2,0.9,0.8&box=0,0,1,1", "image/jpeg", "more than once"),
        (None, "0.1,0.2,0.9,0.8", "image/png", "Content-Type"),
        (_png_bytes(), "0.1,0.2,0.9,0.8", "image/jpeg", "not a JPEG"),
        (b"\xff\xd8\xff\xe0garbage-after-the-marker" * 4, "0.1,0.2,0.9,0.8", "image/jpeg", "JPEG"),
        (jpeg_bytes(320, 240)[:400], "0.1,0.2,0.9,0.8", "image/jpeg", "JPEG"),
        (jpeg_bytes(20, 20), "0,0,1,1", "image/jpeg", "smaller than 32 px"),
        (jpeg_bytes(2100, 2000), "0,0,1,1", "image/jpeg", "more than 4000000 pixels"),
        (None, "0.5,0.5,0.52,0.9", "image/jpeg", "smaller than 16 px"),
    ],
)
def test_mask_bad_request(server, body, box, ctype, msg):
    status, _, p = server.client.mask(body if body is not None else jpeg_bytes(320, 240), box, ctype)
    assert status == 400
    assert msg in p["error"]
    assert server.runner.calls == []


def test_mask_body_limits(server):
    c = http.client.HTTPConnection("127.0.0.1", server.server_address[1], timeout=5)
    c.putrequest("POST", "/v1/mask?box=0,0,1,1")
    c.putheader("Content-Type", "image/jpeg")
    c.endheaders()
    r = c.getresponse()
    assert r.status == 411 and "Content-Length" in json.loads(r.read())["error"]
    c.close()
    # larger than max_body_bytes (2 MiB here): refused before the body is read
    c = http.client.HTTPConnection("127.0.0.1", server.server_address[1], timeout=5)
    c.putrequest("POST", "/v1/mask?box=0,0,1,1")
    c.putheader("Content-Type", "image/jpeg")
    c.putheader("Content-Length", str(3 << 20))
    c.endheaders()
    r = c.getresponse()
    assert r.status == 413
    c.close()


def test_mask_busy_answers_503_with_retry_after(server):
    assert server.state.busy.acquire(blocking=False)
    try:
        status, headers, p = server.client.mask(jpeg_bytes())
    finally:
        server.state.busy.release()
    assert (status, p) == (503, {"error": "busy"})
    assert headers["Retry-After"] == "1"
    assert server.client.mask(jpeg_bytes())[0] == 200


def test_mask_one_at_a_time(server):
    runner = server.runner
    runner.gate = threading.Event()
    first = {}
    t = threading.Thread(target=lambda: first.update(r=server.client.mask(jpeg_bytes())))
    t.start()
    assert runner.entered.wait(5)
    status, headers, p = server.client.mask(jpeg_bytes())
    assert (status, p["error"], headers["Retry-After"]) == (503, "busy", "1")
    runner.gate.set()
    t.join(5)
    assert first["r"][0] == 200
    assert len(runner.calls) == 1


def test_mask_not_ready(server):
    server.state.phase = "loading"
    status, headers, p = server.client.mask(jpeg_bytes())
    assert (status, p, headers["Retry-After"]) == (503, {"error": "not ready"}, "1")
    assert server.runner.calls == []


def test_mask_runner_failure_is_500_and_frees_the_lock(server):
    server.runner.fail = True
    assert server.client.mask(jpeg_bytes())[::2] == (500, {"error": "internal error"})
    assert server.state.busy.acquire(blocking=False)
    server.state.busy.release()


def test_routes(server):
    c = server.client
    assert c.request("GET", "/nope")[::2] == (404, {"error": "not found"})
    status, headers, _ = c.request("GET", "/v1/mask?box=0,0,1,1")
    assert (status, headers["Allow"]) == (405, "POST")
    assert c.request("POST", "/healthz", b"")[0] == 405
    assert c.request("DELETE", "/v1/mask")[0] == 405
    status, _, info = c.request("GET", "/v1/info")
    assert status == 200 and info["phase"] == "ready" and info["rssPeakBytes"] > 0


def test_request_log_is_one_json_line(server):
    out = io.StringIO()
    logs.configure("INFO", out)
    try:
        server.client.mask(jpeg_bytes())
        server.client.mask(jpeg_bytes(), box="bad")
    finally:
        logs.configure("INFO", sys.stderr)
    lines = [json.loads(line) for line in out.getvalue().splitlines()]
    assert [(r["msg"], r["status"]) for r in lines] == [("request", 200), ("request", 400)]
    assert lines[0]["level"] == "INFO" and lines[0]["path"] == "/v1/mask"
    assert lines[0]["width"] == 320 and "total" in lines[0]["timingsMs"]


# ---- process: readiness after warm-up, graceful SIGTERM -------------------------------------

STUB_MAIN = textwrap.dedent(
    """
    import sys, threading, time
    sys.path[:0] = [{service!r}, {tests!r}]
    import conftest
    import v2i_studio.__main__ as m

    class Runner(conftest.StubRunner):
        def __init__(self, *a):
            super().__init__()
        def load(self):
            time.sleep({load_s})
            return {{}}
        def warmup(self):
            time.sleep(0.3)
            return {{}}
        def mask(self, pil, box):
            time.sleep(1.0)  # a request in flight when SIGTERM arrives
            return super().mask(pil, box)

    m.Runner = Runner
    sys.exit(m.main())
    """
)


def _free_port():
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


def _get(port, path):
    try:
        c = http.client.HTTPConnection("127.0.0.1", port, timeout=2)
        c.request("GET", path)
        r = c.getresponse()
        r.read()
        return r.status
    except OSError:
        return None


def test_process_ready_after_warmup_and_graceful_sigterm(tmp_path):
    port = _free_port()
    p = _start_stub(tmp_path, port, load_s=0.3)
    try:
        seen = set()
        deadline = time.time() + 10
        while time.time() < deadline:
            h, r = _get(port, "/healthz"), _get(port, "/readyz")
            seen.add((h, r))
            if r == 200:
                break
            time.sleep(0.05)
        assert (200, 503) in seen  # listening (healthz) before the models are warm
        assert _get(port, "/readyz") == 200

        result = {}
        t = threading.Thread(target=lambda: result.update(r=_post(port)))
        t.start()
        time.sleep(0.3)  # the request is inside the (1 s) stub inference
        p.send_signal(signal.SIGTERM)
        t.join(10)
        assert result["r"] == 200  # finished, not cut off
        assert p.wait(10) == 0
    finally:
        if p.poll() is None:
            p.kill()
    lines = [json.loads(line) for line in p.stderr.read().splitlines()]
    msgs = [r["msg"] for r in lines]
    assert msgs[0] == "listening" and "ready" in msgs and msgs[-1] == "stopped"
    assert msgs.index("stopping") < msgs.index("stopped")


def _start_stub(tmp_path, port, load_s):
    script = tmp_path / "main.py"
    script.write_text(STUB_MAIN.format(service=str(SERVICE_DIR), tests=str(SERVICE_DIR / "tests"), load_s=load_s))
    env = {**os.environ, "STUDIO_PORT": str(port), "STUDIO_HOST": "127.0.0.1"}
    return subprocess.Popen([sys.executable, str(script)], env=env, stderr=subprocess.PIPE, text=True)


def test_process_sigterm_while_loading_exits_at_once(tmp_path):
    port = _free_port()
    p = _start_stub(tmp_path, port, load_s=60)
    try:
        deadline = time.time() + 10
        while _get(port, "/healthz") != 200 and time.time() < deadline:
            time.sleep(0.05)
        assert _get(port, "/readyz") == 503
        t0 = time.time()
        p.send_signal(signal.SIGTERM)
        assert p.wait(5) == 0
        assert time.time() - t0 < 3
    finally:
        if p.poll() is None:
            p.kill()
    last = json.loads(p.stderr.read().splitlines()[-1])
    assert (last["msg"], last["phase"]) == ("stopped", "loading")


def _post(port):
    c = http.client.HTTPConnection("127.0.0.1", port, timeout=10)
    c.request("POST", "/v1/mask?box=0.1,0.1,0.9,0.9", jpeg_bytes(), {"Content-Type": "image/jpeg"})
    r = c.getresponse()
    r.read()
    return r.status


def test_process_exits_1_when_models_fail(tmp_path):
    env = {**os.environ, "STUDIO_PORT": str(_free_port()), "STUDIO_MODELS_DIR": str(tmp_path)}
    p = subprocess.run([sys.executable, "-m", "v2i_studio"], env=env, cwd=SERVICE_DIR, capture_output=True, text=True, timeout=30)
    assert p.returncode == 1
    assert "model load failed" in p.stderr and "fetch-models.sh" in p.stderr


def test_process_exits_2_on_bad_config():
    env = {**os.environ, "STUDIO_THREADS": "many"}
    p = subprocess.run([sys.executable, "-m", "v2i_studio"], env=env, cwd=SERVICE_DIR, capture_output=True, text=True, timeout=30)
    assert p.returncode == 2 and "STUDIO_THREADS" in p.stderr


def test_probe_exit_codes(server):
    port = str(server.server_address[1])

    def run(*args, **env):
        env = {**os.environ, "STUDIO_HOST": "0.0.0.0", "STUDIO_PORT": port, **env}
        cmd = [sys.executable, "-m", "v2i_studio.probe", *args]
        return subprocess.run(cmd, cwd=SERVICE_DIR, env=env, capture_output=True, timeout=20).returncode

    assert run() == 0
    assert run("/healthz") == 0
    server.state.phase = "loading"
    assert run() == 1 and run("/healthz") == 0
    assert run(STUDIO_PORT=str(_free_port())) == 1  # nothing listening
