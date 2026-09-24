"""Settings from the environment (STUDIO_*)."""

import os
from dataclasses import dataclass
from pathlib import Path

from .models import SAM_ENCODERS, RunnerOptions

SERVICE_DIR = Path(__file__).resolve().parent.parent


@dataclass(frozen=True)
class Config:
    host: str
    port: int
    models_dir: str
    manifest: str
    threads: int
    runner: RunnerOptions
    max_body_bytes: int
    max_pixels: int
    log_level: str


def _int(env, key, default, lo, hi):
    raw = env.get(key, "")
    if raw == "":
        return default
    try:
        v = int(raw)
    except ValueError:
        raise ValueError(f"{key}={raw!r} is not an integer") from None
    if not lo <= v <= hi:
        raise ValueError(f"{key}={v} is outside {lo}..{hi}")
    return v


def _bool(env, key, default):
    raw = env.get(key, "").strip().lower()
    if raw == "":
        return default
    if raw in ("1", "true", "yes", "on"):
        return True
    if raw in ("0", "false", "no", "off"):
        return False
    raise ValueError(f"{key}={raw!r} is not a boolean")


def split_threads(total, parallel):
    """Intra-op threads for (BiRefNet, SAM). In parallel both models run at once, so the
    CPUs are shared: half each, rounded up for BiRefNet (the slower model); sequential
    runs give every model all of them."""
    if not parallel:
        return total, total
    b = (total + 1) // 2
    return b, max(1, total - b)


def load(env=None):
    env = os.environ if env is None else env
    threads = _int(env, "STUDIO_THREADS", 4, 1, 64)
    parallel = _bool(env, "STUDIO_PARALLEL", True)
    bt, st = split_threads(threads, parallel)
    bt = _int(env, "STUDIO_BIREFNET_THREADS", bt, 1, 64)
    st = _int(env, "STUDIO_SAM_THREADS", st, 1, 64)
    enc = env.get("STUDIO_SAM_ENCODER", "") or "fp32"
    if enc not in SAM_ENCODERS:
        raise ValueError(f"STUDIO_SAM_ENCODER={enc!r} (one of {', '.join(SAM_ENCODERS)})")
    models_dir = env.get("STUDIO_MODELS_DIR", "") or str(SERVICE_DIR / "models")
    return Config(
        host=env.get("STUDIO_HOST", "") or "127.0.0.1",
        port=_int(env, "STUDIO_PORT", 4100, 1, 65535),
        models_dir=models_dir,
        manifest=env.get("STUDIO_MODELS_SHA256", "") or str(SERVICE_DIR / "models.sha256"),
        threads=threads,
        runner=RunnerOptions(
            birefnet_threads=bt,
            sam_threads=st,
            parallel=parallel,
            sam_encoder=enc,
            spinning=_bool(env, "STUDIO_SPINNING", False),
        ),
        max_body_bytes=_int(env, "STUDIO_MAX_BODY_BYTES", 40 << 20, 1024, 1 << 30),
        max_pixels=_int(env, "STUDIO_MAX_PIXELS", 36_000_000, 1024, 200_000_000),
        log_level=(env.get("STUDIO_LOG_LEVEL", "") or "INFO").upper(),
    )
