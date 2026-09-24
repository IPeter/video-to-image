#!/usr/bin/env python3
"""Compares two sets of sidecar masks (e.g. fp32 vs int8 SAM encoder, or before/after a
model re-export), as written by `bench.py --save DIR` (<name>.png + <name>.json).

For every photo: IoU of the final masks (alpha >= 128), share of pixels whose alpha moved
by more than 32, and the mask-gate decision of both (a mirror of the default thresholds of
internal/studio.Gates, G4-G10 of the research spike). Writes one overlay per photo to
--out: the cut-out of A and of B on flat magenta (what the QA check sees) and a diff panel
(red: only in A, green: only in B), plus full-resolution crops for --zoom regions.

    python tools/compare_masks.py A_DIR B_DIR --fixtures DIR --out DIR \\
        [--zoom user02_sx4_trunk_open=0.3,0.0,0.75,0.55 ...] [--min-iou 0.995]

Exit status 1 when any IoU is below --min-iou or any gate decision differs.
"""

import argparse
import json
import os
import sys

import cv2
import numpy as np

MAGENTA = np.array([255, 0, 255], np.float32)  # RGB

# internal/studio.Gates defaults (research spike G4-G10)
GATES = {
    "maxEdgeContact": 0.02,
    "maxTopEdgeContact": 0.10,
    "minBoxFill": 0.45,
    "maxBoxFill": 0.90,
    "maxOutsideBox": 0.03,
    "maxSecondComponent": 0.002,
    "maxSemiTransparent": 0.05,
    "minIoU": 0.80,
    "hardIoU": 0.95,
    "hardRescued": 0.01,
    "minCoverage": 0.03,
    "maxCoverage": 0.97,
}


def gate_decision(s, g=GATES):
    """(passed, hard, failed checks) for one MaskStats dict."""
    ec = s["edgeContact"]
    failed = []
    if max(ec["left"], ec["right"], ec["bottom"]) > g["maxEdgeContact"]:
        failed.append("edge")
    if ec["top"] > g["maxTopEdgeContact"]:
        failed.append("topEdge")
    if not g["minBoxFill"] <= s["boxFill"] <= g["maxBoxFill"]:
        failed.append("boxFill")
    if s["outsideBox"] > g["maxOutsideBox"]:
        failed.append("outsideBox")
    if s["secondComponent"] > g["maxSecondComponent"]:
        failed.append("secondComponent")
    if s["semiTransparent"] > g["maxSemiTransparent"]:
        failed.append("semiTransparent")
    if s["iou"] < g["minIoU"]:
        failed.append("modelsDisagree")
    if not g["minCoverage"] <= s["coverage"] <= g["maxCoverage"]:
        failed.append("coverage")
    hard = s["iou"] < g["hardIoU"] or s["rescuedFraction"] > g["hardRescued"]
    return (not failed, hard, failed)


def cutout(rgb, alpha):
    a = alpha.astype(np.float32)[..., None] / 255
    return (rgb * a + MAGENTA * (1 - a)).astype(np.uint8)


def diff_panel(rgb, a, b):
    out = (rgb * 0.35).astype(np.uint8)
    only_a, only_b = (a >= 128) & (b < 128), (b >= 128) & (a < 128)
    out[only_a] = (255, 40, 40)
    out[only_b] = (40, 255, 40)
    return out


def panels(rgb, a, b, width):
    row = np.concatenate([cutout(rgb, a), cutout(rgb, b), diff_panel(rgb, a, b)], 1)
    s = width / row.shape[1]
    if s < 1:
        row = cv2.resize(row, (width, int(row.shape[0] * s)), interpolation=cv2.INTER_AREA)
    return row


def save_jpeg(path, rgb):
    cv2.imwrite(path, cv2.cvtColor(rgb, cv2.COLOR_RGB2BGR), [cv2.IMWRITE_JPEG_QUALITY, 90])


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("a")
    ap.add_argument("b")
    ap.add_argument("--fixtures", required=True)
    ap.add_argument("--out", required=True)
    ap.add_argument("--zoom", action="append", default=[], help="name=x0,y0,x1,y1 (normalised)")
    ap.add_argument("--min-iou", type=float, default=0.995)
    ap.add_argument("--width", type=int, default=2400)
    a = ap.parse_args()
    os.makedirs(a.out, exist_ok=True)
    zooms = {}
    for z in a.zoom:
        name, box = z.split("=")
        zooms.setdefault(name, []).append([float(v) for v in box.split(",")])
    rows, bad = [], False
    for f in sorted(os.listdir(a.a)):
        if not f.endswith(".png") or not os.path.exists(os.path.join(a.b, f)):
            continue
        name = f[:-4]
        ma = cv2.imread(os.path.join(a.a, f), cv2.IMREAD_UNCHANGED)
        mb = cv2.imread(os.path.join(a.b, f), cv2.IMREAD_UNCHANGED)
        bgr = cv2.imread(os.path.join(a.fixtures, name + ".jpg"))
        rgb = cv2.cvtColor(bgr, cv2.COLOR_BGR2RGB).astype(np.float32)
        A, B = ma >= 128, mb >= 128
        iou = float((A & B).sum() / max(1, (A | B).sum()))
        moved = float((np.abs(ma.astype(np.int16) - mb) > 32).mean())
        sa = json.load(open(os.path.join(a.a, name + ".json")))["stats"]
        sb = json.load(open(os.path.join(a.b, name + ".json")))["stats"]
        da, db = gate_decision(sa), gate_decision(sb)
        same = da == db
        bad |= iou < a.min_iou or not same
        rows.append(
            {
                "name": name,
                "iou": round(iou, 5),
                "moved32": round(moved, 5),
                "gatesA": da,
                "gatesB": db,
                "sameGates": same,
                "iouModelsA": sa["iou"],
                "iouModelsB": sb["iou"],
                "rescuedA": sa["rescuedFraction"],
                "rescuedB": sb["rescuedFraction"],
            }
        )
        print(
            f"{name:40s} IoU {iou:.5f}  moved>32 {moved:.5f}  gates {'same' if same else 'DIFFER'} {da} vs {db}"
            f"  models IoU {sa['iou']:.4f}/{sb['iou']:.4f}  rescued {sa['rescuedFraction']:.4f}/{sb['rescuedFraction']:.4f}"
        )
        save_jpeg(os.path.join(a.out, name + ".jpg"), panels(rgb, ma, mb, a.width))
        H, W = ma.shape
        for k, (x0, y0, x1, y1) in enumerate(zooms.get(name, [])):
            sl = (slice(int(y0 * H), int(y1 * H)), slice(int(x0 * W), int(x1 * W)))
            save_jpeg(os.path.join(a.out, f"{name}_zoom{k}.jpg"), panels(rgb[sl], ma[sl], mb[sl], a.width))
    with open(os.path.join(a.out, "compare.json"), "w") as f:
        json.dump(rows, f, indent=1)
    differ = [r["name"] for r in rows if not r["sameGates"]]
    print("min IoU", min((r["iou"] for r in rows), default=None), "| gate decisions differ:", differ)
    return 1 if bad else 0


if __name__ == "__main__":
    sys.exit(main())
