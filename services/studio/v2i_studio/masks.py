"""Mask logic of the sidecar: box parsing, the BiRefNet crop, SAM output selection and
clean-up, the "rescue union" and the statistics the Go gates read (internal/studio.MaskStats).

Everything here is numpy + OpenCV on uint8 arrays (255 = car) and needs no model, so it is
unit-tested on synthetic masks. Ported from the research spike (studio_mask.py and
combine3.py in data/research/studio-spike/prototype); the numbers must stay the same, so
the spike's rounding and structuring elements are kept on purpose.
"""

import math
import re

import cv2
import numpy as np

# BiRefNet runs on the review box plus this share of the box size on every side.
BOX_PAD = 0.06
# Rescue union: SAM's core is its mask eroded by RESCUE_RADIUS × the long image side
# (at least 2 px); rescued components smaller than RESCUE_MIN_AREA × the image are ignored.
RESCUE_RADIUS = 0.004
RESCUE_MIN_AREA = 2e-5
# A box smaller than this (in pixels, before padding) is not a car worth cutting out.
MIN_BOX_PX = 16

_NUM = re.compile(r"^[+-]?(?:\d+(?:\.\d*)?|\.\d+)(?:[eE][+-]?\d+)?$")


class InputError(ValueError):
    """The request is wrong (answered with 400 and the message)."""


def parse_box(raw):
    """Parses "x0,y0,x1,y1" (normalised, 0 <= x0 < x1 <= 1, 0 <= y0 < y1 <= 1).

    Only plain decimal numbers are accepted (no NaN/Inf, no spaces)."""
    if raw is None or raw == "":
        raise InputError("missing box (box=x0,y0,x1,y1)")
    parts = raw.split(",")
    if len(parts) != 4:
        raise InputError("box must be four comma-separated numbers x0,y0,x1,y1")
    if not all(_NUM.match(p) for p in parts):
        raise InputError("box values must be plain decimal numbers")
    x0, y0, x1, y1 = (float(p) for p in parts)
    if not all(math.isfinite(v) for v in (x0, y0, x1, y1)):
        raise InputError("box values must be finite")
    if not (0 <= x0 < x1 <= 1 and 0 <= y0 < y1 <= 1):
        raise InputError("box must satisfy 0 <= x0 < x1 <= 1 and 0 <= y0 < y1 <= 1")
    return (x0, y0, x1, y1)


def check_box_size(box, width, height):
    """Rejects a box that covers fewer than MIN_BOX_PX pixels on either side."""
    x0, y0, x1, y1 = box
    if (x1 - x0) * width < MIN_BOX_PX or (y1 - y0) * height < MIN_BOX_PX:
        raise InputError(f"box is smaller than {MIN_BOX_PX} px on a side")


def crop_rect(box, width, height, pad=BOX_PAD):
    """The BiRefNet input crop (x0, y0, x1, y1 in pixels): the box plus pad × its size on
    every side, clipped to the image (the spike's exact integer rounding)."""
    x0, y0, x1, y1 = box
    bw, bh = x1 - x0, y1 - y0
    cx0 = max(0, int((x0 - pad * bw) * width))
    cy0 = max(0, int((y0 - pad * bh) * height))
    cx1 = min(width, int(round((x1 + pad * bw) * width)))
    cy1 = min(height, int(round((y1 + pad * bh) * height)))
    return cx0, cy0, cx1, cy1


def rescue_radius(height, width, frac=RESCUE_RADIUS):
    return max(2, int(round(frac * max(height, width))))


def disk(r):
    return cv2.getStructuringElement(cv2.MORPH_ELLIPSE, (2 * r + 1, 2 * r + 1))


def birefnet_alpha(logits):
    """BiRefNet's 2-D logits → uint8 alpha (sigmoid, then min-max stretch like rembg)."""
    p = 1 / (1 + np.exp(-logits))
    p = (p - p.min()) / max(float(p.max() - p.min()), 1e-6)
    return (p * 255).astype(np.uint8)


def select_sam_output(masks, scores):
    """Index of the whole-car output among SAM's multimask outputs: the largest area among
    the outputs whose predicted IoU is within 0.02 of the best one."""
    areas = [(masks[k] > 0).mean() for k in range(masks.shape[0])]
    cand = [k for k in range(masks.shape[0]) if scores[k] >= scores.max() - 0.02]
    return max(cand, key=lambda i: areas[i])


def sam_clean(m):
    """Conservative clean-up of SAM's hard mask (uint8 0/1): drops far specks and tiny
    islands, fills enclosed holes. Returns a new uint8 0/1 mask."""
    m = m.copy()
    H, W = m.shape
    g = max(3, int(0.01 * np.hypot(H, W)))
    n, lab, _, _ = cv2.connectedComponentsWithStats(cv2.dilate(m, disk(g)), 8)
    if n > 2:
        cnt = np.bincount(lab[m > 0], minlength=n)
        cnt[0] = 0
        keep = np.zeros(n, bool)
        keep[int(np.argmax(cnt))] = True
        keep |= cnt > 0.01 * H * W
        keep[0] = False
        m = (keep[lab] & (m > 0)).astype(np.uint8)
    n, lab, st, _ = cv2.connectedComponentsWithStats(m, 8)
    if n > 2:
        keep = st[:, cv2.CC_STAT_AREA] >= 2.5e-5 * H * W
        keep[0] = False
        keep[1 + int(np.argmax(st[1:, cv2.CC_STAT_AREA]))] = True
        m = keep[lab].astype(np.uint8)
    ff = np.pad(m, 1)
    cv2.floodFill(ff, np.zeros((H + 4, W + 4), np.uint8), (0, 0), 2)
    m[ff[1:-1, 1:-1] == 0] = 1
    return m


def sam_alpha(logits):
    """SAM logits at image size (float32) → soft uint8 alpha: the cleaned hard mask with a
    soft edge from the logits (only used where SAM rescues a region)."""
    m = sam_clean((logits > 0).astype(np.uint8))
    soft = np.clip(128 + logits * 32, 0, 255)
    a_s = np.where(m > 0, np.clip(soft, 128, 255), np.clip(soft, 0, 127)).astype(np.uint8)
    a_s[(m == 0) & (logits > 0)] = 0  # removed specks
    a_s[(m > 0) & (logits <= 0)] = 255  # filled holes
    return a_s


def rescue_union(a_b, a_s, r, min_area=RESCUE_MIN_AREA):
    """Puts back car regions BiRefNet dropped. a_b BiRefNet alpha, a_s SAM alpha (uint8).

    core = SAM's mask eroded by r; the core pixels where BiRefNet is below 250 are rescued
    (components of at least min_area of the image), grown back by r+2 inside SAM, alpha
    max(BiRefNet, SAM) there and 255 in the rescued core. Everywhere else BiRefNet's alpha
    is kept, so alpha is only ever raised. Returns (alpha, rescued_fraction, components)."""
    H, W = a_b.shape
    core = cv2.erode((a_s >= 128).astype(np.uint8), disk(r)) > 0
    rec = (core & (a_b < 250)).astype(np.uint8)
    n, lab, st, _ = cv2.connectedComponentsWithStats(rec, 8)
    keep = np.zeros(n, bool)
    keep[1:] = st[1:, cv2.CC_STAT_AREA] >= min_area * H * W
    reck = keep[lab]
    if not reck.any():
        return a_b.copy(), 0.0, 0
    grown = ((cv2.dilate(reck.astype(np.uint8), disk(r + 2)) > 0) & (a_s > 0)) | reck
    out = np.maximum(a_b, np.where(grown, a_s, 0)).astype(np.uint8)
    out[reck] = 255
    rescued = float(((out.astype(np.int16) - a_b) > 64).mean())
    return out, rescued, int(keep.sum())


def edge_band(height, width):
    return max(2, int(0.003 * max(height, width)))


def mask_stats(alpha, a_b, a_s, box, rescued):
    """The statistics of internal/studio.MaskStats (keys as in its JSON tags) plus details
    for logs. Fractions are of the image area unless noted; the mask is alpha >= 128."""
    H, W = alpha.shape
    m = alpha >= 128
    area = int(m.sum())
    x0, y0, x1, y1 = [int(round(v * s)) for v, s in zip(box, (W, H, W, H), strict=True)]
    boxm = np.zeros_like(m)
    boxm[y0:y1, x0:x1] = True
    _, _, st, _ = cv2.connectedComponentsWithStats(m.astype(np.uint8), 8)
    areas = sorted((st[1:, cv2.CC_STAT_AREA] / (H * W)).tolist(), reverse=True)
    B, S = a_b >= 128, a_s >= 128
    e = edge_band(H, W)
    ys, xs = np.nonzero(m)
    stats = {
        "iou": float((B & S).sum() / max(1, (B | S).sum())),
        "rescuedFraction": float(rescued),
        "coverage": float(area / (H * W)),
        "boxFill": float((m & boxm).sum() / max(1, boxm.sum())),
        "outsideBox": float((m & ~boxm).sum() / max(1, area)),
        "secondComponent": float(areas[1]) if len(areas) > 1 else 0.0,
        "semiTransparent": float(((alpha > 0) & (alpha < 250)).sum() / max(1, area)),
        "edgeContact": {
            "left": float(m[:, :e].any(1).mean()),
            "right": float(m[:, -e:].any(1).mean()),
            "top": float(m[:e].any(0).mean()),
            "bottom": float(m[-e:].any(0).mean()),
        },
    }
    details = {
        "components": len(areas),
        "largest": float(areas[0]) if areas else 0.0,
        "maskBox": [xs.min() / W, ys.min() / H, (xs.max() + 1) / W, (ys.max() + 1) / H] if area else None,
    }
    return rounded(stats), rounded(details)


def rounded(v, nd=6):
    """Rounds floats (recursively) so the JSON stays short and stable."""
    if isinstance(v, float):
        return round(v, nd)
    if isinstance(v, dict):
        return {k: rounded(x, nd) for k, x in v.items()}
    if isinstance(v, (list, tuple)):
        return [rounded(x, nd) for x in v]
    return v
