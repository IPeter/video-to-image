# v2i-studio: segmentation sidecar

Cuts the car out of an exported exterior photo for the studio backgrounds
(`internal/studio`). It only produces the **mask** (alpha, 255 = car) and its statistics;
the Go worker composites `alpha·original + (1−alpha)·backdrop·shadow`, so car pixels
stay the original pixels. It runs as a second container in the worker pod and is reached
on `127.0.0.1:4100`; no image leaves the pod.

Python 3.12 with only `onnxruntime`, `numpy`, `opencv-python-headless` and `pillow`
(`requirements.txt`), and the standard library's `http.server`.

## API

The contract is the doc comment of `studio.Segmenter` in `internal/studio/types.go`.

```
POST /v1/mask?box=x0,y0,x1,y1      body: the exported JPEG, Content-Type: image/jpeg
  200 {"width":W,"height":H,
       "maskPng":"<base64 8-bit grayscale PNG, W×H>",
       "stats":{"iou","rescuedFraction","coverage","boxFill","outsideBox",
                "secondComponent","semiTransparent","edgeContact":{"left","right","top","bottom"}},
       "timingsMs":{"birefnet","sam","samEncoder","samDecoder","samClean","models",
                    "union","stats","decode","png","total"},
       "models":{"birefnet":"sha256:…","sam":"sha256:…"},
       "details":{"components","largest","maskBox","samScore","rescuedComponents","rescueRadiusPx"}}
  400 {"error":"…"}   bad box or image (message says which)
  411 / 413           no Content-Length (or chunked) / body above STUDIO_MAX_BODY_BYTES
  503 {"error":"busy"} or {"error":"not ready"}, Retry-After: 1
  500 {"error":"internal error"}  (logged with the trace)
GET /healthz  → 200 {"ok":true} as soon as the process listens
GET /readyz   → 200 {"ready":true} after the models are loaded, verified and warmed up;
                503 {"ready":false,"phase":"loading"|"stopping"|"failed"}
GET /v1/info  → phase, models, threads, current/peak memory (bench and operators)
```

- **Box**: exactly four plain decimal numbers, `0 ≤ x0 < x1 ≤ 1`, `0 ≤ y0 < y1 ≤ 1`, in
  the pixel coordinates of the JPEG sent; at least 16 px on each side.
- **Image**: a JPEG (magic bytes + Pillow's JPEG decoder), ≥ 32 px per side, at most
  `STUDIO_MAX_PIXELS`; grayscale/CMYK are converted to RGB. **EXIF orientation is
  ignored** on purpose: the mask must match the pixels as Go's `image/jpeg` decodes them.
- **One inference at a time**: a request that finds the models busy gets 503 at once
  (the body is read first, so the client always sees the answer). Every response
  closes the connection.
- `details` is informational (logs); the gates read only `stats`.

## Method (ported unchanged from the research spike)

1. **BiRefNet-general-lite** (MIT; our DeformConv re-export, see `tools/README.md`) on
   the box plus 6 % padding, LANCZOS to 1024², ImageNet normalisation, sigmoid, min-max,
   LANCZOS back into the full-size mask.
2. **SAM 2.1 base-plus** (Apache-2.0 weights; onnx-community export) on the whole image
   squashed to 1024², prompted with the box; of the three outputs, the largest among
   those within 0.02 of the best predicted IoU; logits resized to the image; clean-up
   (far specks and islands dropped, enclosed holes filled); soft edge `128 + 32·logit`.
3. **Rescue union**: `core` = SAM mask eroded by `r` = 0.4 % of the long side (≥ 2 px);
   core pixels where BiRefNet's alpha is below 250 are rescued (components ≥ 2e-5 of the
   image), grown back by `r+2` inside SAM with alpha `max(BiRefNet, SAM)` and 255 in the
   core. Everywhere else BiRefNet's alpha is used unchanged, so alpha is only ever raised.
4. **Stats** (mask = alpha ≥ 128): `iou` BiRefNet vs SAM; `rescuedFraction` share of the
   image whose alpha the union raised by more than 64; `coverage`; `boxFill` mask inside
   the box / box area; `outsideBox` mask outside the box / mask area; `secondComponent`
   area of the second-largest 8-connected component; `semiTransparent` pixels with
   `0 < alpha < 250` / mask area; `edgeContact` share of each edge's rows/columns that the
   mask touches within a band of max(2, 0.3 % of the long side) px.

The masks are byte-identical to the spike's final masks (`S1_box_bp`) on all ten
fixtures. Note on `semiTransparent`: the contract counts every `alpha > 0`; the spike
counted `alpha > 4`, so the values are ~0.5 percentage points higher than in the spike's
tables (1.6-2.0 % instead of 1.1-1.5 % on the fixtures; the gate is 5 %).

The BiRefNet pass and the SAM image encoder run **at the same time** on two long-lived
threads, each with its own ONNX Runtime intra-op pool; the SAM decoder, clean-up,
union and statistics follow. Model work never runs on the HTTP threads.

## Configuration

| Variable | Default | Meaning |
| --- | --- | --- |
| `STUDIO_HOST` / `STUDIO_PORT` | `127.0.0.1` / `4100` | listen address (in the pod only localhost) |
| `STUDIO_MODELS_DIR` | `services/studio/models` (image: `/models`) | model files |
| `STUDIO_MODELS_SHA256` | `services/studio/models.sha256` (image: `/app/models.sha256`) | manifest the files are verified against at start |
| `STUDIO_THREADS` | `4` | CPUs to use (the container's CPU limit) |
| `STUDIO_PARALLEL` | `true` | BiRefNet and the SAM encoder at the same time |
| `STUDIO_BIREFNET_THREADS` / `STUDIO_SAM_THREADS` | split of `STUDIO_THREADS` | explicit intra-op threads per model (tuning on a new node) |
| `STUDIO_SAM_ENCODER` | `fp32` | `fp32` or `int8` (see measurements) |
| `STUDIO_SPINNING` | `false` | ORT spin-waiting between ops (burns CPU quota) |
| `STUDIO_MAX_BODY_BYTES` | `41943040` (40 MiB) | request body limit |
| `STUDIO_MAX_PIXELS` | `36000000` | largest image (8K UHD = 33.2 MP) |
| `STUDIO_LOG_LEVEL` | `INFO` | `DEBUG` also logs health/readiness probes |

A bad value stops the process with exit code 2; missing or corrupt model files stop it
with exit code 1 (so Kubernetes shows a crash loop instead of a pod that never gets ready).

## Models

Not in git. `./fetch-models.sh` fills `models/` and verifies every file against
`models.sha256` (`--check` only verifies); it copies from the research spike
(`data/research/studio-spike/models`) when present, else downloads SAM from Hugging Face
`onnx-community/sam2.1-hiera-base-plus-ONNX` at the pinned revision
`bab18593f44e652f04cf18b60b3690f60e8996b0`. BiRefNet has no public download of our
export: set `STUDIO_BIREFNET_URL` (e.g. a copy in MinIO) or re-export it
(`tools/README.md`).

| File | Size | Licence |
| --- | --- | --- |
| `birefnet-general-lite-dc.onnx` | 181 MB | MIT (BiRefNet code and weights) |
| `sam2.1-hiera-base-plus/vision_encoder.onnx` + `_data` | 306 MB | Apache-2.0 (SAM 2.1 weights) |
| `sam2.1-hiera-base-plus/vision_encoder_int8.onnx` + `_data` | 100 MB | Apache-2.0 |
| `sam2.1-hiera-base-plus/prompt_encoder_mask_decoder.onnx` + `_data` | 21 MB | Apache-2.0 |

`models` in every answer is the provenance: `birefnet` is the file's sha256; `sam` is the
sha256 of its manifest lines, reproducible with
`grep -E 'vision_encoder\.|prompt_encoder' models.sha256 | sort -k2 | shasum -a 256`
(`vision_encoder_int8\.` for the int8 encoder).

## Running

```bash
./fetch-models.sh
python3.12 -m venv .venv && .venv/bin/pip install -r requirements-dev.txt
.venv/bin/python -m pytest              # unit + HTTP tests, no models needed (~10 s)
.venv/bin/python -m pytest -m slow      # real models on 2 fixtures vs the spike masks
.venv/bin/python -m v2i_studio          # serve on 127.0.0.1:4100
.venv/bin/python bench.py --repeat 3    # time every fixture through the API
```

Docker (context = this directory, after `fetch-models.sh`):

```bash
docker build -t v2i-studio .                              # native arch
docker build --platform linux/amd64 -t v2i-studio .       # the k3s node
docker run --rm --read-only --cpus 4 --memory 3584m -e STUDIO_HOST=0.0.0.0 -p 127.0.0.1:4100:4100 v2i-studio
```

The image runs as uid 10001, needs no writable path (`--read-only` works) and bakes in
the models, verified with `sha256sum -c` at build time. ONNX Runtime's Linux wheels ship
a telemetry client (to `mobile.events.data.microsoft.com`); `ORT_DISABLE_TELEMETRY=1`
is set by the package before onnxruntime is imported and in the image.

Logs are one JSON object per line on stderr (`time`, `level`, `msg`, fields): `listening`,
`models loaded`, `ready` (with load, verify and warm-up times), one `request` line per
call (status, ms, size, stage timings, iou, rescued, coverage), `stopping`, `stopped`.
SIGTERM/SIGINT: readiness goes to 503, the listener closes, the running inference
finishes, then the process exits 0 (during loading it exits at once).

## Measurements

See "Speed and memory" below.
