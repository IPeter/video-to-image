"""One JSON object per line on stderr, shaped like the Go services' slog output."""

import json
import sys
import threading
import time

LEVELS = {"DEBUG": 10, "INFO": 20, "WARN": 30, "ERROR": 40}

_lock = threading.Lock()
_min = LEVELS["INFO"]
_out = sys.stderr


def configure(level="INFO", out=None):
    global _min, _out
    level = level.upper()
    if level not in LEVELS:
        raise ValueError(f"unknown log level {level!r} (DEBUG, INFO, WARN, ERROR)")
    _min = LEVELS[level]
    if out is not None:
        _out = out


def _now():
    t = time.time()
    return time.strftime("%Y-%m-%dT%H:%M:%S", time.gmtime(t)) + f".{int(t % 1 * 1000):03d}Z"


def log(level, msg, **fields):
    if LEVELS[level] < _min:
        return
    rec = {"time": _now(), "level": level, "msg": msg, **fields}
    line = json.dumps(rec, separators=(",", ":"), default=str)
    with _lock:
        _out.write(line + "\n")
        _out.flush()


def debug(msg, **f):
    log("DEBUG", msg, **f)


def info(msg, **f):
    log("INFO", msg, **f)


def warn(msg, **f):
    log("WARN", msg, **f)


def error(msg, **f):
    log("ERROR", msg, **f)
