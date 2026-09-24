#!/usr/bin/env python3
"""Times every fixture through the sidecar's HTTP API: the first request per photo (cold)
and the median of the following ones (warm), client wall time and the server's stage
timings, plus the sidecar's peak memory (GET /v1/info) at the end. Standard library only.

    python bench.py [--url http://127.0.0.1:4100] [--fixtures DIR] [--repeat 3]
                    [--only pub01,user02] [--save DIR] [--out bench.json]

--fixtures is a directory with manifest.json ([{"file": ".../name.jpg", "box": [x0,y0,x1,y1]}],
the research spike's format; files are looked up by base name in that directory).
--save writes <name>.png (mask) and <name>.json (stats, details, timings) of the last run.
"""

import argparse
import base64
import http.client
import json
import os
import statistics
import sys
import time
from urllib.parse import urlsplit

DEFAULT_FIXTURES = os.path.join(os.path.dirname(os.path.abspath(__file__)), "../../data/research/studio-spike/fixtures")


def request(url, method, path, body=None, headers=None, timeout=300):
    u = urlsplit(url)
    c = http.client.HTTPConnection(u.hostname, u.port or 80, timeout=timeout)
    try:
        c.request(method, path, body=body, headers=headers or {})
        r = c.getresponse()
        data = r.read()
        return r.status, json.loads(data) if data else None
    finally:
        c.close()


def wait_ready(url, timeout):
    t0 = time.perf_counter()
    while True:
        try:
            if request(url, "GET", "/readyz", timeout=5)[0] == 200:
                return time.perf_counter() - t0
        except OSError:
            pass
        if time.perf_counter() - t0 > timeout:
            raise SystemExit(f"sidecar at {url} not ready after {timeout} s")
        time.sleep(0.2)


def post_mask(url, jpeg, box):
    q = ",".join(repr(float(v)) for v in box)
    while True:
        t0 = time.perf_counter()
        status, p = request(url, "POST", f"/v1/mask?box={q}", jpeg, {"Content-Type": "image/jpeg"})
        wall = (time.perf_counter() - t0) * 1000
        if status == 503:  # busy (someone else is using it): wait like the Go client
            time.sleep(1)
            continue
        if status != 200:
            raise SystemExit(f"POST /v1/mask: {status} {p}")
        return wall, p


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--url", default="http://127.0.0.1:4100")
    ap.add_argument("--fixtures", default=DEFAULT_FIXTURES)
    ap.add_argument("--repeat", type=int, default=3, help="requests per photo (1 cold + N-1 warm)")
    ap.add_argument("--only", default="", help="comma-separated name prefixes")
    ap.add_argument("--save", default="")
    ap.add_argument("--out", default="")
    ap.add_argument("--wait", type=float, default=300, help="seconds to wait for /readyz")
    ap.add_argument("--label", default="")
    ap.add_argument("--mem", action="store_true", help="read /v1/info memory after every request")
    a = ap.parse_args()

    ready_s = wait_ready(a.url, a.wait)
    manifest = json.load(open(os.path.join(a.fixtures, "manifest.json")))
    only = [p for p in a.only.split(",") if p]
    rows = []
    mem = []  # (request #, current RSS, anon RSS, cgroup) after each request
    first_ms = None
    for e in manifest:
        name = os.path.splitext(os.path.basename(e["file"]))[0]
        if only and not any(name.startswith(p) for p in only):
            continue
        jpeg = open(os.path.join(a.fixtures, name + ".jpg"), "rb").read()
        runs = []
        for _ in range(max(1, a.repeat)):
            runs.append(post_mask(a.url, jpeg, e["box"]))
            if a.mem:
                m = request(a.url, "GET", "/v1/info")[1]
                mem.append([len(mem) + 1, m.get("rssBytes"), m.get("rssAnonBytes"), m.get("cgroupBytes")])
        if first_ms is None:
            first_ms = runs[0][0]
        warm = runs[1:] or runs
        p = runs[-1][1]
        row = {
            "name": name,
            "size": [p["width"], p["height"]],
            "coldMs": int(runs[0][0]),
            "warmMs": int(statistics.median(r[0] for r in warm)),
            "warmServerMs": {k: int(statistics.median(r[1]["timingsMs"].get(k, 0) for r in warm)) for k in p["timingsMs"]},
            "iou": p["stats"]["iou"],
            "rescued": p["stats"]["rescuedFraction"],
        }
        rows.append(row)
        s = row["warmServerMs"]
        print(
            f"{name:40s} {p['width']}x{p['height']:<5} cold {row['coldMs']:6d} ms  warm {row['warmMs']:6d} ms"
            f"  (birefnet {s.get('birefnet', 0)}, sam {s.get('sam', 0)}, union {s.get('union', 0)},"
            f" png {s.get('png', 0)})" + (f"  rss after {mem[-1][1] / 2**20:.0f} MiB" if a.mem and mem[-1][1] else ""),
            flush=True,
        )
        if a.save:
            os.makedirs(a.save, exist_ok=True)
            with open(os.path.join(a.save, name + ".png"), "wb") as f:
                f.write(base64.b64decode(p["maskPng"]))
            with open(os.path.join(a.save, name + ".json"), "w") as f:
                json.dump({k: p[k] for k in ("stats", "details", "timingsMs", "models", "width", "height")}, f, indent=1)
    _, info = request(a.url, "GET", "/v1/info")
    warm = [r["warmMs"] for r in rows]
    summary = {
        "label": a.label,
        "url": a.url,
        "photos": len(rows),
        "readyWaitS": round(ready_s, 2),
        "firstRequestMs": int(first_ms or 0),
        "warmMedianMs": int(statistics.median(warm)) if warm else 0,
        "warmMeanMs": int(statistics.mean(warm)) if warm else 0,
        "warmMinMs": min(warm, default=0),
        "warmMaxMs": max(warm, default=0),
        "info": info,
    }
    if mem:
        summary["rssAfterRequestMiB"] = [round((m[1] or 0) / 2**20) for m in mem]
    print(json.dumps(summary), flush=True)
    if a.out:
        with open(a.out, "w") as f:
            json.dump({"summary": summary, "rows": rows}, f, indent=1)
    return 0


if __name__ == "__main__":
    sys.exit(main())
