#!/usr/bin/env python3
"""Try the generative studio background on your own car photos.

    make studio-try PHOTOS="path/a.jpg path/b.jpg"            # exterior photos
    make studio-try PHOTOS="path/dash.jpg" ARGS="--interior"  # cabin photos: the studio shows through the glass

Exterior: the car is cut out by the local segmentation sidecar (make studio-up), put on a
magenta canvas with a margin, and the image model fills the magenta with a photo studio.
The car pixels in the result are the original ones, pasted back through the mask; the
script reports how well the model kept the car in place (alignment, 0-255, under 15 is
good) and marks the result FAIL when it moved the car. Interior: the model replaces
only what is seen through the windows; pixels it did not change are taken from the
original. Outputs go to data/studio-try/<time>/. Needs GEMINI_API_KEY in .env.
"""
import argparse, base64, io, json, os, sys, time, urllib.request
from pathlib import Path

import numpy as np
from PIL import Image, ImageFilter
from google import genai
from google.genai import types

ROOT = Path(__file__).resolve().parent.parent
STYLES = {
    "grey": "a seamless light-grey cyclorama curving from the wall into a glossy floor, a large round dark turntable platform on the floor exactly under the car",
    "white": "a seamless pure-white cyclorama curving from the wall into a glossy white floor, a large round light turntable platform on the floor exactly under the car",
    "dark": "a dark charcoal showroom studio with a black glossy floor, a large round turntable platform under the car, rim lights along the walls",
}

def prompt_exterior(style):
    return ("The image shows a used car cut out of its photo: every pixel that is not the car is flat magenta (#FF00FF). "
            f"Fill ONLY the magenta area with a professional car photography studio seen from the same camera position as this photo: {STYLES[style]}, "
            "matching the car's footprint and perspective, soft even softbox lighting, a soft contact shadow under the car and a subtle floor reflection of the car. "
            "The output must be pixel-aligned with the input: the car stays exactly where it is, at exactly the same size and angle, and every car pixel is unchanged. "
            "Do not repaint, clean, move, crop or resize the car. Photorealistic.")

def prompt_interior(style):
    return ("This is a photo of a used car's interior for a sales listing. Replace ONLY what is visible OUTSIDE the car through the windscreen, the side windows and any open door "
            f"with a professional car photography studio: {STYLES[style]}, large softbox lights. Everything inside the car must stay exactly as it is, pixel for pixel: "
            "dashboard, steering wheel, seats, trim, dirt, wear, objects lying around. Same camera perspective and framing.")

def api_key():
    for line in (ROOT / ".env").read_text().splitlines():
        if line.startswith("GEMINI_API_KEY="):
            return line.split("=", 1)[1].strip()
    sys.exit("GEMINI_API_KEY not found in .env")

def aspect(w, h):
    opts = {"1:1": 1, "3:2": 1.5, "2:3": 2/3, "4:3": 4/3, "3:4": 0.75, "16:9": 16/9, "9:16": 9/16, "4:5": 0.8, "5:4": 1.25, "21:9": 21/9}
    return min(opts, key=lambda k: abs(opts[k] - w / h))

def jpeg_bytes(img, long=1536, q=90):
    im = img.copy(); im.thumbnail((long, long))
    b = io.BytesIO(); im.save(b, "JPEG", quality=q); return b.getvalue()

def generate(client, model, img, prompt):
    cfg = types.GenerateContentConfig(response_modalities=["IMAGE", "TEXT"], image_config=types.ImageConfig(aspect_ratio=aspect(*img.size)))
    resp = client.models.generate_content(model=model, contents=[types.Part.from_bytes(data=jpeg_bytes(img), mime_type="image/jpeg"), prompt], config=cfg)
    out, text = None, ""
    for p in resp.candidates[0].content.parts:
        if p.inline_data and out is None:
            out = Image.open(io.BytesIO(p.inline_data.data)).convert("RGB")
        elif p.text:
            text += p.text
    u = resp.usage_metadata
    return out, text, (u.prompt_token_count or 0, u.candidates_token_count or 0)

def find_box(client, img, model="gemini-3.5-flash-lite"):
    """The car's bounding box from a cheap text model (the pipeline gets it from its review
    step); the segmentation sidecar needs a box close to the car. Returns "x0,y0,x1,y1"."""
    schema = types.Schema(type="OBJECT", properties={"box_2d": types.Schema(type="ARRAY", items=types.Schema(type="INTEGER"),
                          description="[ymin, xmin, ymax, xmax] of the whole car, 0-1000")}, required=["box_2d"])
    prompt = "Return the bounding box of the whole car in this photo as box_2d [ymin, xmin, ymax, xmax], integers 0-1000. JSON only."
    for cfg in (types.GenerateContentConfig(response_mime_type="application/json", response_schema=schema, thinking_config=types.ThinkingConfig(thinking_level="minimal")),
                types.GenerateContentConfig(response_mime_type="application/json", response_schema=schema)):
        try:
            resp = client.models.generate_content(model=model, contents=[types.Part.from_bytes(data=jpeg_bytes(img, 768), mime_type="image/jpeg"), prompt], config=cfg)
            b = json.loads(resp.text)["box_2d"]
            y0, x0, y1, x1 = [min(max(v / 1000, 0), 1) for v in b]
            if x1 - x0 > 0.05 and y1 - y0 > 0.05:
                return f"{x0:.3f},{y0:.3f},{x1:.3f},{y1:.3f}"
        except Exception as e:  # noqa: BLE001 - try the next config, then give up
            err = e
    print(f"  (could not get a car box from {model}: {err}; using the whole frame)")
    return "0.02,0.02,0.98,0.98"

def sidecar_mask(path, box, url):
    data = Path(path).read_bytes()
    req = urllib.request.Request(f"{url}/v1/mask?box={box}", data=data, method="POST",
                                 headers={"Content-Type": "image/jpeg", "Content-Length": str(len(data))})
    try:
        with urllib.request.urlopen(req, timeout=300) as r:
            d = json.load(r)
    except urllib.error.URLError as e:
        sys.exit(f"segmentation sidecar not reachable at {url} ({e}); start it with: make studio-up")
    return Image.open(io.BytesIO(base64.b64decode(d["maskPng"]))).convert("L"), d["stats"]

def padded(orig, alpha, pad):
    a = np.asarray(alpha.resize(orig.size, Image.BILINEAR)) > 128
    o = np.asarray(orig).copy(); o[~a] = (255, 0, 255)
    w, h = orig.size; px, py = int(w * pad), int(h * pad)
    cut = Image.new("RGB", (w + 2 * px, h + 2 * py), (255, 0, 255)); cut.paste(Image.fromarray(o), (px, py))
    full = Image.new("RGB", cut.size, (255, 0, 255)); full.paste(orig, (px, py))
    am = Image.new("L", cut.size, 0); am.paste(alpha.resize(orig.size, Image.BILINEAR), (px, py))
    return cut, full, am

def alpha_composite(base, gen, alpha):
    g = gen.resize(base.size, Image.LANCZOS)
    a = np.asarray(alpha).astype(np.float32)[..., None] / 255
    o = np.asarray(base).astype(np.float32); gg = np.asarray(g).astype(np.float32)
    return Image.fromarray(np.clip(a * o + (1 - a) * gg, 0, 255).astype(np.uint8)), g

def car_alignment(base, gen_full, alpha):
    small = (768, max(1, int(768 * base.size[1] / base.size[0])))
    o = np.asarray(base.resize(small)).astype(np.float32); g = np.asarray(gen_full.resize(small)).astype(np.float32)
    d = np.abs(o - g).mean(axis=2)
    core = np.asarray(alpha.resize(small).filter(ImageFilter.MinFilter(15))) > 128
    return float(d[core].mean()) if core.any() else -1.0

def interior_composite(orig, gen):
    g = gen.resize(orig.size, Image.LANCZOS)
    small = (768, max(1, int(768 * orig.size[1] / orig.size[0])))
    d = np.abs(np.asarray(orig.resize(small)).astype(np.float32) - np.asarray(g.resize(small)).astype(np.float32)).mean(axis=2)
    dm = Image.fromarray(np.clip(d * 3, 0, 255).astype(np.uint8)).resize(orig.size).filter(ImageFilter.GaussianBlur(6))
    a = np.clip((np.asarray(dm).astype(np.float32)[..., None] / 255 - 0.25) / 0.35, 0, 1)
    o = np.asarray(orig).astype(np.float32); gg = np.asarray(g).astype(np.float32)
    return Image.fromarray(np.clip((1 - a) * o + a * gg, 0, 255).astype(np.uint8)), float((d > 25).mean())

def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("photos", nargs="+")
    ap.add_argument("--interior", action="store_true", help="cabin photos: replace only what is seen through the glass")
    ap.add_argument("--style", choices=sorted(STYLES), default="grey")
    ap.add_argument("--model", default="gemini-3.1-flash-image")
    ap.add_argument("--pad", type=float, default=0.25, help="magenta margin around the photo, fraction of its size")
    ap.add_argument("--box", default="", help="where the car is, x0,y0,x1,y1 in 0-1; found automatically when empty")
    ap.add_argument("--sidecar", default="http://127.0.0.1:4100")
    ap.add_argument("--out", default=str(ROOT / "data" / "studio-try" / time.strftime("%Y%m%d-%H%M%S")))
    args = ap.parse_args()
    out = Path(args.out); out.mkdir(parents=True, exist_ok=True)
    client = genai.Client(api_key=api_key())
    for path in args.photos:
        name = Path(path).stem
        orig = Image.open(path).convert("RGB")
        t = time.time()
        if args.interior:
            gen, text, toks = generate(client, args.model, orig, prompt_interior(args.style))
            if gen is None:
                print(f"{name}: no image returned ({text[:120]})"); continue
            gen.save(out / f"{name}_gen.jpg", quality=92)
            comp, changed = interior_composite(orig, gen)
            comp.save(out / f"{name}_studio.jpg", quality=92)
            print(f"{name}: interior, changed {changed:.0%} of the picture, {time.time()-t:.0f}s, tokens {toks} -> {out / (name + '_studio.jpg')}")
            continue
        box = args.box or find_box(client, orig)
        alpha, stats = sidecar_mask(path, box, args.sidecar)
        cut, full, am = padded(orig, alpha, args.pad)
        cut.save(out / f"{name}_cutout.jpg", quality=88)
        gen, text, toks = generate(client, args.model, cut, prompt_exterior(args.style))
        if gen is None:
            print(f"{name}: no image returned ({text[:120]})"); continue
        gen.save(out / f"{name}_gen.jpg", quality=92)
        comp, gen_full = alpha_composite(full, gen, am)
        comp.save(out / f"{name}_studio.jpg", quality=92)
        align = car_alignment(full, gen_full, am)
        verdict = "OK" if 0 <= align < 15 else "FAIL (the model moved the car; try a bigger --pad)"
        print(f"{name}: box {box}, mask iou {stats['iou']:.2f}, alignment {align:.1f} {verdict}, {time.time()-t:.0f}s, tokens {toks} -> {out / (name + '_studio.jpg')}")

if __name__ == "__main__":
    main()
