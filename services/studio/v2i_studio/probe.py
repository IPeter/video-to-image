"""Kubernetes exec probe: the sidecar listens on 127.0.0.1 only, so kubelet's httpGet
(which targets the pod IP) cannot reach it, and the image has no curl.

    python -m v2i_studio.probe            # GET /readyz → exit 0 on 200, else 1
    python -m v2i_studio.probe /healthz

Host and port come from STUDIO_HOST/STUDIO_PORT like the server (0.0.0.0 → 127.0.0.1).
"""

import http.client
import os
import sys


def main(argv):
    path = argv[1] if len(argv) > 1 else "/readyz"
    host = os.environ.get("STUDIO_HOST", "") or "127.0.0.1"
    if host in ("0.0.0.0", "::"):
        host = "127.0.0.1"
    port = int(os.environ.get("STUDIO_PORT", "") or 4100)
    try:
        c = http.client.HTTPConnection(host, port, timeout=3)
        c.request("GET", path)
        status = c.getresponse().status
    except OSError as e:
        print(f"{path}: {e}", file=sys.stderr)
        return 1
    if status != 200:
        print(f"{path}: HTTP {status}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
