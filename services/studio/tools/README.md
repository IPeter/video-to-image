# services/studio/tools

One-off and measurement tools; none of them is part of the image.

| Tool | What it does |
| --- | --- |
| `export_deformconv.py` | Re-exports BiRefNet-general-lite from its MIT `.pth` to ONNX with the native `DeformConv` op (needs torch) |
| `threads_bench.py` | Times the runner per intra-op thread split (sequential / parallel, int8, spinning) and records peak memory, one process per configuration |
| `compare_masks.py` | Compares two mask sets written by `../bench.py --save` (IoU, gate decisions, magenta cut-out overlays) |

## Reproducing `birefnet-general-lite-dc.onnx`

The sidecar does not use rembg's or BiRefNet's published ONNX files: they emulate
torchvision's `deform_conv2d` with GatherND/Mul chains that materialise
`[1, 64, k·k, 256, 256]` tensors (784 MB each for the 7×7 branch at 1024 px), which
peaks at about 6.5 GB and is OOM-killed in the 4 GiB worker. ONNX Runtime has a CPU
`DeformConv` kernel (opset 19), so `export_deformconv.py` emits the real op: same weights
and maths, peak 2.0-2.3 GB, 3-5 s per image (research spike, `spike-result.json` →
`rembg`). The export is a one-off; the result goes into `../models.sha256` and is
copied into the image by `../fetch-models.sh` (`STUDIO_BIREFNET_URL` when there is no
local copy).

Inputs (MIT licence, code and weights):

- code: <https://github.com/ZhengPeng7/BiRefNet> at commit
  `ebcc0bc8ec7fe919cec829f2dea656b3078acddc` (2026-09-02)
- weights: release `v1`, `BiRefNet-general-bb_swin_v1_tiny-epoch_232.pth`
  (177,791,685 bytes, sha256 `6a1e050c6ec2697e5ed268455df544782b023acf8643ab771250979094875ab1`)

```bash
python3.12 -m venv /tmp/birefnet-export && . /tmp/birefnet-export/bin/activate
pip install torch torchvision timm kornia einops onnx   # the spike used torch 2.14
git clone https://github.com/ZhengPeng7/BiRefNet /tmp/BiRefNet
git -C /tmp/BiRefNet checkout ebcc0bc8ec7fe919cec829f2dea656b3078acddc
# The lite model is the Swin-T backbone: in config.py pick 'swin_v1_t' in the self.bb list
# (index 6; the default, index 3, is Swin-L). The script loads the weights strictly, so a
# wrong backbone fails loudly.
sed -i.bak "s/        \]\[3\]\n        self.freeze_bb/X/" /tmp/BiRefNet/config.py   # or edit by hand
curl -fLO https://github.com/ZhengPeng7/BiRefNet/releases/download/v1/BiRefNet-general-bb_swin_v1_tiny-epoch_232.pth
python export_deformconv.py /tmp/BiRefNet BiRefNet-general-bb_swin_v1_tiny-epoch_232.pth birefnet-general-lite-dc.onnx
```

A new export is not necessarily byte-identical to the current one (the torch version
changes constant folding and node order). Before switching, run both through the
sidecar and compare:

```bash
python ../bench.py --save /tmp/masks-old --repeat 1              # current model
# swap the file, update models.sha256, restart the sidecar
python ../bench.py --save /tmp/masks-new --repeat 1
python compare_masks.py /tmp/masks-old /tmp/masks-new --fixtures <fixtures> --out /tmp/cmp
```

The spike's check of its own export against rembg's original ONNX: mean |diff| 0.09/255,
0.04 % of pixels off by more than 32 (all in soft tyre-shadow areas).
