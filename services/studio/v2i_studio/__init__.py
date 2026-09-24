"""v2i studio sidecar: car cut-out masks (BiRefNet-lite + SAM 2.1 rescue union) over HTTP."""

import os

# ONNX Runtime's Linux wheels ship a telemetry client that posts to
# mobile.events.data.microsoft.com; the sidecar must not talk to the network. The switch is
# read when onnxruntime is imported, so it is set before any module imports it.
os.environ.setdefault("ORT_DISABLE_TELEMETRY", "1")
