"""Re-export a BiRefNet .pth to ONNX with the native ONNX DeformConv op (opset 19).

The rembg/BiRefNet-release ONNX files emulate torchvision's deform_conv2d with
GatherND/Mul chains (deform_conv2d_onnx_exporter), which materialise
[1, 64, k*k, H/4, W/4] tensors: 784 MB each for the 7x7 branch at 1024 px, several
alive at once -> 6.5 GB peak. ONNX Runtime has a CPU DeformConv kernel, so emitting
the real op keeps the same weights and maths without those tensors.

usage: export_deformconv.py REPO_DIR WEIGHTS.pth OUT.onnx [--size 1024]
"""

import argparse
import os
import sys

import torch
from torch.onnx import symbolic_helper as sh

ap = argparse.ArgumentParser()
ap.add_argument("repo")
ap.add_argument("weights")
ap.add_argument("out")
ap.add_argument("--size", type=int, default=1024)
a = ap.parse_args()

sys.path.insert(0, a.repo)
os.chdir(a.repo)
from models.birefnet import BiRefNet  # noqa: E402
from utils import check_state_dict  # noqa: E402


@sh.parse_args("v", "v", "v", "v", "v", "i", "i", "i", "i", "i", "i", "i", "i", "b")
def deform_conv2d_sym(
    g, inp, weight, offset, mask, bias, stride_h, stride_w, pad_h, pad_w, dil_h, dil_w, n_weight_grps, n_offset_grps, use_mask
):
    kh, kw = sh._get_tensor_sizes(weight)[2:]
    ins = [inp, weight, offset, bias] + ([mask] if use_mask else [])
    return g.op(
        "DeformConv",
        *ins,
        dilations_i=[dil_h, dil_w],
        group_i=n_weight_grps,
        kernel_shape_i=[kh, kw],
        offset_group_i=n_offset_grps,
        pads_i=[pad_h, pad_w, pad_h, pad_w],
        strides_i=[stride_h, stride_w],
    )


torch.onnx.register_custom_op_symbolic("torchvision::deform_conv2d", deform_conv2d_sym, 19)

net = BiRefNet(bb_pretrained=False)
sd = check_state_dict(torch.load(a.weights, map_location="cpu", weights_only=True))
missing, unexpected = net.load_state_dict(sd, strict=True), None
net.eval()
x = torch.randn(1, 3, a.size, a.size)
with torch.no_grad():
    torch.onnx.export(
        net,
        x,
        a.out,
        opset_version=19,
        input_names=["input_image"],
        output_names=["output_image"],
        dynamo=False,
        do_constant_folding=True,
    )
print("exported", a.out, os.path.getsize(a.out) // 2**20, "MB")
