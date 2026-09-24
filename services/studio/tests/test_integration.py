"""The real models through the real HTTP server on two fixtures, compared with the research
spike's final masks (pytest -m slow). Needs models/ (fetch-models.sh), the spike fixtures and
the spike masks; skipped when any of them is missing.

    STUDIO_FIXTURES_DIR   default data/research/studio-spike/fixtures (with manifest.json)
    STUDIO_REFERENCE_DIR  the spike's synth/masks/S1_box_bp directory
"""

import base64
import io
import json
import os
import threading
from pathlib import Path

import numpy as np
import pytest
from PIL import Image

from conftest import Client
from v2i_studio import config
from v2i_studio.models import Runner
from v2i_studio.server import Server, State

REPO = Path(__file__).resolve().parents[3]
FIXTURES = Path(os.environ.get("STUDIO_FIXTURES_DIR", REPO / "data/research/studio-spike/fixtures"))
REFERENCE = Path(
    os.environ.get(
        "STUDIO_REFERENCE_DIR",
        "/private/tmp/claude-501/-Users-ipetike--ai-coding-video-to-image/"
        "4d1facb2-4aaf-4dba-a4b6-0167fa1d9baf/scratchpad/studio/synth/masks/S1_box_bp",
    )
)
# pub01: thin roof antenna, car nearly fills the frame; user02: open tailgate that only the
# SAM rescue keeps (BiRefNet/SAM IoU 0.86, 6 % of the image rescued).
CASES = ["pub01_suzuki_sx4_silver_front34", "user02_sx4_trunk_open"]

pytestmark = pytest.mark.slow


@pytest.fixture(scope="module")
def real_server():
    cfg = config.load()
    if not (Path(cfg.models_dir) / "birefnet-general-lite-dc.onnx").exists():
        pytest.skip("models missing (run fetch-models.sh)")
    if not (FIXTURES / "manifest.json").exists() or not REFERENCE.is_dir():
        pytest.skip("spike fixtures or reference masks missing")
    runner = Runner(cfg.models_dir, cfg.manifest, cfg.runner)
    runner.load()
    runner.warmup()
    state = State()
    state.phase = "ready"
    srv = Server(("127.0.0.1", 0), runner, state, cfg.max_body_bytes, cfg.max_pixels)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    yield Client(srv.server_address[1])
    srv.shutdown()
    srv.server_close()
    runner.close()


@pytest.mark.parametrize("name", CASES)
def test_real_models_match_spike_masks(real_server, name):
    manifest = {Path(e["file"]).stem: e for e in json.loads((FIXTURES / "manifest.json").read_text())}
    ref_path = REFERENCE / f"{name}.png"
    if name not in manifest or not ref_path.exists():
        pytest.skip(f"{name} not available")
    box = ",".join(repr(v) for v in manifest[name]["box"])
    status, _, p = real_server.mask((FIXTURES / f"{name}.jpg").read_bytes(), box)
    assert status == 200, p
    got = np.asarray(Image.open(io.BytesIO(base64.b64decode(p["maskPng"]))))
    ref = np.asarray(Image.open(ref_path))
    assert got.shape == ref.shape == (p["height"], p["width"])
    a, b = got >= 128, ref >= 128
    iou = (a & b).sum() / (a | b).sum()
    assert iou >= 0.99, iou
    spike = json.loads((REFERENCE / f"{name}.json").read_text())
    assert p["stats"]["iou"] == pytest.approx(spike["iouBirefnetSam"], abs=0.005)
    assert p["stats"]["rescuedFraction"] == pytest.approx(spike["rescued"], abs=0.005)
    assert set(p["models"]) == {"birefnet", "sam"}
