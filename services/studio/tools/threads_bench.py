#!/usr/bin/env python3
"""Measures the intra-op thread split (and memory) of the runner, in-process, one fresh
process per configuration so the peaks do not mix. Configurations are interleaved over
rounds, so drifting host load hits all of them alike. Run it where the limits are real:

    docker run --rm --cpus 4 --memory 4g -v "$PWD/tools:/tools:ro" -v FIXTURES:/fx:ro \\
      --entrypoint python v2i-studio /tools/threads_bench.py --fixtures /fx \\
      --configs seq:4+4,par:2+2,par:4+4,par:3+1,par:1+3 --rounds 2 --only pub01,user02

A configuration is seq|par:<birefnet threads>+<sam threads>[:spin][:int8].
Memory (Linux): VmHWM (peak RSS) and the sampled peak of RssAnon of each process.
"""

import argparse
import json
import os
import statistics
import subprocess
import sys
import threading
import time

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, os.path.dirname(HERE))


def parse_config(s):
    parts = s.split(":")
    mode, split = parts[0], parts[1]
    b, t = (int(x) for x in split.split("+"))
    flags = set(parts[2:])
    return {"parallel": mode == "par", "b": b, "s": t, "spin": "spin" in flags, "enc": "int8" if "int8" in flags else "fp32"}


def proc_status():
    out = {}
    try:
        with open("/proc/self/status") as f:
            for line in f:
                k, _, v = line.partition(":")
                if k in ("VmHWM", "VmRSS", "RssAnon", "RssFile"):
                    out[k] = int(v.split()[0]) * 1024
    except OSError:
        pass
    return out


def child(a):
    import resource

    from PIL import Image

    from v2i_studio import config
    from v2i_studio.models import Runner, RunnerOptions

    c = parse_config(a.one)
    cfg = config.load()
    opts = RunnerOptions(
        birefnet_threads=c["b"], sam_threads=c["s"], parallel=c["parallel"], sam_encoder=c["enc"], spinning=c["spin"]
    )
    peak = {"RssAnon": 0}
    stop = threading.Event()

    def sample():
        while not stop.is_set():
            peak["RssAnon"] = max(peak["RssAnon"], proc_status().get("RssAnon", 0))
            time.sleep(0.01)

    threading.Thread(target=sample, daemon=True).start()
    r = Runner(cfg.models_dir, cfg.manifest, opts)
    load = r.load()
    warm = r.warmup()
    manifest = json.load(open(os.path.join(a.fixtures, "manifest.json")))
    only = [p for p in a.only.split(",") if p]
    photos = {}
    for e in manifest:
        name = os.path.splitext(os.path.basename(e["file"]))[0]
        if only and not any(name.startswith(p) for p in only):
            continue
        pil = Image.open(os.path.join(a.fixtures, name + ".jpg")).convert("RGB")
        runs = []
        for _ in range(a.repeat):
            t0 = time.perf_counter()
            res = r.mask(pil, tuple(e["box"]))
            runs.append({"ms": (time.perf_counter() - t0) * 1000, **res["timings"]})
        photos[name] = {k: round(statistics.median(x[k] for x in runs)) for k in ("ms", "birefnet", "sam", "samEncoder")}
    stop.set()
    st = proc_status()
    cpu = resource.getrusage(resource.RUSAGE_SELF)
    ru = cpu.ru_maxrss
    print(
        json.dumps(
            {
                "config": a.one,
                "load": load,
                "warmupMs": warm["warmup"],
                "photos": photos,
                "vmHWM": st.get("VmHWM", ru if sys.platform == "darwin" else ru * 1024),
                "rssAnonPeak": peak["RssAnon"],
                "cpuS": round(cpu.ru_utime + cpu.ru_stime, 1),
            }
        ),
        flush=True,
    )


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--fixtures", required=True)
    ap.add_argument("--configs", default="seq:4+4,par:2+2,par:4+4,par:3+1,par:1+3")
    ap.add_argument("--rounds", type=int, default=2)
    ap.add_argument("--repeat", type=int, default=2)
    ap.add_argument("--only", default="")
    ap.add_argument("--env", action="append", default=[], help="KEY=VALUE for the child processes")
    ap.add_argument("--one", help=argparse.SUPPRESS)
    ap.add_argument("--out", default="")
    a = ap.parse_args()
    if a.one:
        return child(a)
    env = dict(os.environ)
    env.update(kv.split("=", 1) for kv in a.env)
    results = []
    for rnd in range(a.rounds):
        for cfg in a.configs.split(","):
            cmd = [sys.executable, os.path.abspath(__file__), "--one", cfg, "--fixtures", a.fixtures]
            cmd += ["--repeat", str(a.repeat), "--only", a.only]
            p = subprocess.run(cmd, env=env, capture_output=True, text=True)
            if p.returncode != 0:
                print(f"{cfg}: exit {p.returncode}\n{p.stderr[-2000:]}", file=sys.stderr)
                results.append({"config": cfg, "round": rnd, "failed": p.returncode})
                continue
            res = json.loads(p.stdout.strip().splitlines()[-1])
            res["round"] = rnd
            results.append(res)
            ms = [v["ms"] for v in res["photos"].values()]
            print(
                f"round {rnd} {cfg:16s} median {statistics.median(ms):6.0f} ms  per photo {ms}"
                f"  HWM {res['vmHWM'] / 2**20:5.0f} MiB  anon {res['rssAnonPeak'] / 2**20:5.0f} MiB",
                flush=True,
            )
    print("\nsummary (median over rounds and photos):")
    for cfg in a.configs.split(","):
        rs = [r for r in results if r["config"] == cfg and "photos" in r]
        if not rs:
            print(f"{cfg:16s} FAILED")
            continue
        ms = [v["ms"] for r in rs for v in r["photos"].values()]
        print(
            f"{cfg:16s} {statistics.median(ms):6.0f} ms (min {min(ms):.0f}, max {max(ms):.0f})"
            f"  peak RSS {max(r['vmHWM'] for r in rs) / 2**20:5.0f} MiB"
            f"  anon {max(r['rssAnonPeak'] for r in rs) / 2**20:5.0f} MiB"
        )
    if a.out:
        with open(a.out, "w") as f:
            json.dump(results, f, indent=1)
    return 0


if __name__ == "__main__":
    sys.exit(main())
