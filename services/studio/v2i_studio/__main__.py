"""python -m v2i_studio: listen at once (/healthz), load + warm up the models (/readyz), serve,
and stop gracefully on SIGTERM/SIGINT (the running inference finishes first)."""

import os
import signal
import sys
import threading
import time
import traceback

import cv2
import onnxruntime as ort

from . import config, logs
from .models import Runner
from .server import Server, State


def main():
    try:
        cfg = config.load()
        logs.configure(cfg.log_level)
    except ValueError as e:
        logs.error("bad configuration", error=str(e))
        return 2
    o = cfg.runner
    cv2.setNumThreads(cfg.threads)
    state = State()
    state.info = {
        "samEncoder": o.sam_encoder,
        "threads": {"birefnet": o.birefnet_threads, "sam": o.sam_threads, "parallel": o.parallel},
        "onnxruntime": ort.__version__,
    }
    runner = Runner(cfg.models_dir, cfg.manifest, o)
    try:
        srv = Server((cfg.host, cfg.port), runner, state, cfg.max_body_bytes, cfg.max_pixels)
    except OSError as e:
        logs.error("cannot listen", addr=f"{cfg.host}:{cfg.port}", error=str(e))
        return 1
    stop = threading.Event()
    exit_code = [0]

    def on_signal(signum, _frame):
        logs.info("stopping", signal=signal.Signals(signum).name)
        stop.set()

    signal.signal(signal.SIGTERM, on_signal)
    signal.signal(signal.SIGINT, on_signal)

    def load():
        t0 = time.perf_counter()
        try:
            tim = runner.load()
            logs.info("models loaded", models=runner.models, modelsDir=cfg.models_dir, timingsMs=tim, **state.info)
            tim.update(runner.warmup())
        except Exception as e:  # noqa: BLE001 - any failure means the sidecar is useless
            state.phase = "failed"
            logs.error("model load failed", error=str(e), trace=traceback.format_exc(limit=3))
            exit_code[0] = 1
            stop.set()
            return
        if state.phase == "loading":
            state.info["models"] = runner.models
            state.phase = "ready"
            logs.info("ready", readyMs=int((time.perf_counter() - t0) * 1000), timingsMs=tim)

    serve = threading.Thread(target=srv.serve_forever, kwargs={"poll_interval": 0.2}, name="http")
    serve.start()
    logs.info("listening", addr=f"{cfg.host}:{cfg.port}", maxBodyBytes=cfg.max_body_bytes, maxPixels=cfg.max_pixels)
    threading.Thread(target=load, name="load", daemon=True).start()

    while not stop.wait(1.0):
        pass
    if not state.ready:
        # Loading or failed: no request to finish, and a model run of the warm-up may still
        # be going (the executor would be joined at exit), so leave at once.
        logs.info("stopped", exitCode=exit_code[0], phase=state.phase)
        os._exit(exit_code[0])
    state.phase = "stopping"
    srv.shutdown()  # stop accepting
    srv.server_close()  # waits for the running requests
    serve.join()
    runner.close()
    logs.info("stopped", exitCode=exit_code[0])
    return exit_code[0]


if __name__ == "__main__":
    sys.exit(main())
