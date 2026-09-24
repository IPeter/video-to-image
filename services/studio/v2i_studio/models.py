"""The two segmentation models on ONNX Runtime (CPU) and the runner that combines them.

BiRefNet-general-lite (MIT, DeformConv re-export) draws clean, tight edges and keeps thin
parts, but sometimes drops whole car regions (an open tailgate, a B-pillar + window).
SAM 2.1 base-plus (Apache-2.0) prompted with the review box almost never drops a large
region but leaves fringes; its core puts back what BiRefNet dropped (masks.rescue_union).

The BiRefNet pass and the SAM image encoder are independent, so they run at the same time
on two threads (ONNX Runtime releases the GIL), each with its own intra-op thread pool.
"""

import ctypes
import ctypes.util
import hashlib
import os
import sys
import time
from concurrent.futures import ThreadPoolExecutor
from dataclasses import dataclass

import cv2
import numpy as np
import onnxruntime as ort
from PIL import Image

from . import masks

MEAN = np.array([0.485, 0.456, 0.406], np.float32)
STD = np.array([0.229, 0.224, 0.225], np.float32)
SIZE = 1024  # both models run at 1024×1024

BIREFNET_FILE = "birefnet-general-lite-dc.onnx"
SAM_DIR = "sam2.1-hiera-base-plus"
SAM_DECODER = "prompt_encoder_mask_decoder.onnx"
SAM_ENCODERS = {"fp32": "vision_encoder.onnx", "int8": "vision_encoder_int8.onnx"}


@dataclass(frozen=True)
class RunnerOptions:
    birefnet_threads: int = 2
    sam_threads: int = 2
    parallel: bool = True  # BiRefNet and the SAM encoder at the same time
    sam_encoder: str = "fp32"
    spinning: bool = False  # ORT intra-op spin-waiting (burns CPU quota while idle)


def model_files(sam_encoder):
    """Model name → files (relative to the models dir) for the chosen SAM encoder."""
    enc = SAM_ENCODERS[sam_encoder]
    sam = [f"{SAM_DIR}/{enc}", f"{SAM_DIR}/{enc}_data", f"{SAM_DIR}/{SAM_DECODER}", f"{SAM_DIR}/{SAM_DECODER}_data"]
    return {"birefnet": [BIREFNET_FILE], "sam": sorted(sam)}


def sha256_file(path):
    h = hashlib.sha256()
    with open(path, "rb") as f:
        for chunk in iter(lambda: f.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


def read_manifest(path):
    """models.sha256 (`shasum -a 256` format) → {relative path: sha256}."""
    out = {}
    with open(path, encoding="utf-8") as f:
        for line in f:
            line = line.strip()
            if not line or line.startswith("#"):
                continue
            digest, name = line.split(None, 1)
            out[name.lstrip("*")] = digest.lower()
    return out


def verify_models(models_dir, files, manifest):
    """Hashes every model file, checks it against the manifest and returns the provenance
    map: a single-file model → its sha256; a multi-file model → the sha256 of its
    manifest lines ("<sha256>  <path>\\n", sorted by path), reproducible with
    `grep <files> models.sha256 | sort -k2 | shasum -a 256`."""
    prov = {}
    for model, names in files.items():
        lines = []
        for name in names:
            path = os.path.join(models_dir, name)
            if not os.path.isfile(path):
                raise FileNotFoundError(f"model file missing: {path} (run fetch-models.sh)")
            want = manifest.get(name)
            if want is None:
                raise ValueError(f"model file {name} is not listed in the manifest")
            got = sha256_file(path)
            if got != want:
                raise ValueError(f"model file {name}: sha256 {got}, manifest says {want}")
            lines.append(f"{got}  {name}\n")
        digest = got if len(names) == 1 else hashlib.sha256("".join(lines).encode()).hexdigest()
        prov[model] = "sha256:" + digest
    return prov


def session(path, threads, spinning):
    so = ort.SessionOptions()
    so.intra_op_num_threads = threads
    so.inter_op_num_threads = 1
    so.execution_mode = ort.ExecutionMode.ORT_SEQUENTIAL
    so.graph_optimization_level = ort.GraphOptimizationLevel.ORT_ENABLE_ALL
    # Memory for the 3.5 GiB container: without the arena (and without memory patterns,
    # which pre-plan one big block per run) activation memory goes back to the allocator
    # after every run; with it, the peak of both models stays reserved for good.
    so.enable_cpu_mem_arena = False
    so.enable_mem_pattern = False
    so.add_session_config_entry("session.intra_op.allow_spinning", "1" if spinning else "0")
    return ort.InferenceSession(path, so, providers=["CPUExecutionProvider"])


def normalise(rgb):
    x = rgb.astype(np.float32) / 255.0
    return np.ascontiguousarray(((x - MEAN) / STD).transpose(2, 0, 1)[None])


class BiRefNet:
    """BiRefNet-lite on the box crop (rembg's pre/post-processing: LANCZOS to 1024², ImageNet
    normalisation, sigmoid, min-max, LANCZOS back)."""

    def __init__(self, path, threads, spinning):
        self.s = session(path, threads, spinning)
        self.inp = self.s.get_inputs()[0].name

    def alpha(self, pil, box):
        W, H = pil.size
        cx0, cy0, cx1, cy1 = masks.crop_rect(box, W, H)
        sub = pil.crop((cx0, cy0, cx1, cy1))
        x = normalise(np.asarray(sub.resize((SIZE, SIZE), Image.Resampling.LANCZOS)))
        y = self.s.run(None, {self.inp: x})[0][0, 0]
        m = Image.fromarray(masks.birefnet_alpha(y), "L").resize(sub.size, Image.Resampling.LANCZOS)
        out = np.zeros((H, W), np.uint8)
        out[cy0:cy1, cx0:cx1] = np.asarray(m)
        return out


class Sam2:
    """SAM 2.1 (onnx-community export) prompted with the box; returns its soft alpha."""

    def __init__(self, encoder_path, decoder_path, threads, spinning):
        self.enc = session(encoder_path, threads, spinning)
        self.dec = session(decoder_path, threads, spinning)
        self.enc_out = [o.name for o in self.enc.get_outputs()]

    def encode(self, rgb):
        x = normalise(cv2.resize(rgb, (SIZE, SIZE), interpolation=cv2.INTER_AREA))
        return dict(zip(self.enc_out, self.enc.run(None, {"pixel_values": x}), strict=True))

    def decode(self, emb, box, height, width):
        b = np.array([[[v * SIZE for v in box]]], np.float32)
        feeds = {
            "input_points": np.zeros((1, 1, 1, 2), np.float32),
            "input_labels": np.full((1, 1, 1), -10, np.int64),  # padding point (ignored)
            "input_boxes": b,
            "image_embeddings.0": emb["image_embeddings.0"],
            "image_embeddings.1": emb["image_embeddings.1"],
            "image_embeddings.2": emb["image_embeddings.2"],
        }
        scores, logits, _ = self.dec.run(None, feeds)
        lg, scores = logits[0, 0], scores[0, 0]
        k = masks.select_sam_output(lg, scores)
        full = cv2.resize(lg[k].astype(np.float32), (width, height), interpolation=cv2.INTER_LINEAR)
        return full, float(scores[k])


def ms(t0, t1):
    return int(round((t1 - t0) * 1000))


def _libc_trim():
    """glibc's malloc_trim, or None elsewhere (macOS, musl)."""
    if not sys.platform.startswith("linux"):
        return None
    try:
        fn = ctypes.CDLL(ctypes.util.find_library("c") or "libc.so.6").malloc_trim
    except (OSError, AttributeError):
        return None
    fn.argtypes = [ctypes.c_size_t]
    fn.restype = ctypes.c_int
    return fn


_malloc_trim = _libc_trim()


def release_memory():
    """Hands freed heap pages back to the OS. Without the ORT arena, every run frees
    hundreds of MB of activations into glibc's heaps; malloc_trim(0) returns the free
    pages of all arenas, so the RSS between requests does not creep up (fragmentation)."""
    if _malloc_trim is not None:
        _malloc_trim(0)


class Runner:
    """Loads both models once and cuts out one image at a time (the caller serialises).

    All model work runs on two long-lived threads ("infer", which also runs SAM, and
    "birefnet"), never on the HTTP server's per-connection threads, so the same threads
    (and heaps) serve every request."""

    def __init__(self, models_dir, manifest_path, opts):
        if opts.sam_encoder not in SAM_ENCODERS:
            raise ValueError(f"unknown SAM encoder {opts.sam_encoder!r}")
        self.opts = opts
        self.models_dir = models_dir
        self.manifest_path = manifest_path
        self.models = {}
        self.birefnet = None
        self.sam = None
        self.infer = ThreadPoolExecutor(max_workers=1, thread_name_prefix="infer")
        self.side = ThreadPoolExecutor(max_workers=1, thread_name_prefix="birefnet") if opts.parallel else None

    def load(self):
        """Verifies the model files and creates the sessions; returns timings in ms."""
        ort.disable_telemetry_events()
        t0 = time.perf_counter()
        files = model_files(self.opts.sam_encoder)
        self.models = verify_models(self.models_dir, files, read_manifest(self.manifest_path))
        t1 = time.perf_counter()
        o = self.opts
        d = os.path.join(self.models_dir, SAM_DIR)
        self.birefnet = BiRefNet(os.path.join(self.models_dir, BIREFNET_FILE), o.birefnet_threads, o.spinning)
        self.sam = Sam2(os.path.join(d, SAM_ENCODERS[o.sam_encoder]), os.path.join(d, SAM_DECODER), o.sam_threads, o.spinning)
        return {"verify": ms(t0, t1), "sessions": ms(t1, time.perf_counter())}

    def warmup(self):
        """One full run on a synthetic image, so the first real request is not the slow one
        (kernel selection and first allocations happen on the first run)."""
        rng = np.random.default_rng(1)
        h, w = 960, 1280
        yy, xx = np.mgrid[0:h, 0:w]
        img = np.stack([xx * 200 // w, yy * 200 // h, np.full_like(xx, 90)], -1).astype(np.uint8)
        car = ((xx - w / 2) / (w * 0.35)) ** 2 + ((yy - h * 0.55) / (h * 0.25)) ** 2 <= 1
        img[car] = (40, 40, 45)
        img = np.clip(img.astype(np.int16) + rng.integers(-12, 13, img.shape), 0, 255).astype(np.uint8)
        t0 = time.perf_counter()
        self.mask(Image.fromarray(img, "RGB"), (0.1, 0.25, 0.9, 0.85))
        return {"warmup": ms(t0, time.perf_counter())}

    def _sam_branch(self, rgb, box):
        H, W = rgb.shape[:2]
        t0 = time.perf_counter()
        emb = self.sam.encode(rgb)
        t1 = time.perf_counter()
        lg, score = self.sam.decode(emb, box, H, W)
        t2 = time.perf_counter()
        a_s = masks.sam_alpha(lg)
        t3 = time.perf_counter()
        return a_s, score, {"samEncoder": ms(t0, t1), "samDecoder": ms(t1, t2), "samClean": ms(t2, t3), "sam": ms(t0, t3)}

    def _birefnet_branch(self, pil, box):
        t0 = time.perf_counter()
        a_b = self.birefnet.alpha(pil, box)
        return a_b, ms(t0, time.perf_counter())

    def mask(self, pil, box):
        """pil: RGB image; box: normalised (x0, y0, x1, y1). Returns a dict with alpha
        (uint8 H×W, 255 = car), stats (MaskStats keys), details and timings (ms)."""
        return self.infer.submit(self._mask, pil, box).result()

    def _mask(self, pil, box):
        try:
            return self._mask_once(pil, box)
        finally:
            release_memory()

    def _mask_once(self, pil, box):
        t0 = time.perf_counter()
        rgb = np.asarray(pil)
        if self.side is not None:
            fut = self.side.submit(self._birefnet_branch, pil, box)
            try:
                a_s, score, tim = self._sam_branch(rgb, box)
            finally:
                a_b, tim_b = fut.result()
        else:
            a_b, tim_b = self._birefnet_branch(pil, box)
            a_s, score, tim = self._sam_branch(rgb, box)
        tim["birefnet"] = tim_b
        t1 = time.perf_counter()
        H, W = a_b.shape
        r = masks.rescue_radius(H, W)
        alpha, rescued, ncomp = masks.rescue_union(a_b, a_s, r)
        t2 = time.perf_counter()
        stats, details = masks.mask_stats(alpha, a_b, a_s, box, rescued)
        t3 = time.perf_counter()
        details.update(samScore=round(score, 4), rescuedComponents=ncomp, rescueRadiusPx=r)
        tim.update(models=ms(t0, t1), union=ms(t1, t2), stats=ms(t2, t3))
        return {"alpha": alpha, "stats": stats, "details": details, "timings": tim}

    def close(self):
        self.infer.shutdown(wait=True)
        if self.side is not None:
            self.side.shutdown(wait=True)
