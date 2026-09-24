# syntax=docker/dockerfile:1.7
# v2i-studio: the segmentation sidecar of the worker pod (BiRefNet-lite + SAM 2.1 rescue
# union on ONNX Runtime, CPU). Build context: services/studio, after ./fetch-models.sh.
#   services/studio/fetch-models.sh
#   docker build -t v2i-studio services/studio                        # native arch
#   docker build --platform linux/amd64 -t v2i-studio services/studio # the k3s node
# The models (about 510 MB) are baked in and verified against models.sha256 at build time.
# Runs as a non-root user and needs no writable path (read-only root filesystem is fine).

FROM python:3.12-slim AS deps
ENV PIP_NO_CACHE_DIR=1 PIP_DISABLE_PIP_VERSION_CHECK=1
COPY requirements.txt /tmp/requirements.txt
# Binary wheels only: no compiler in either stage.
RUN python -m venv /opt/venv \
 && /opt/venv/bin/pip install --only-binary=:all: -r /tmp/requirements.txt \
 && /opt/venv/bin/pip uninstall -y pip \
 && find /opt/venv -name tests -type d -prune -exec rm -rf {} + \
 && python -m compileall -q /opt/venv/lib

FROM python:3.12-slim
RUN useradd --system --uid 10001 --home-dir /nonexistent --shell /usr/sbin/nologin studio
COPY --from=deps /opt/venv /opt/venv
COPY models.sha256 /app/models.sha256
COPY models/ /models/
RUN cd /models && sha256sum -c /app/models.sha256 && rm -f /models/.gitignore
COPY v2i_studio /app/v2i_studio
RUN python -m compileall -q /app
# MALLOC_ARENA_MAX: two inference threads would otherwise get their own glibc arenas and
# keep freed activation memory mapped (higher peak in the 3.5 GiB container).
ENV PATH=/opt/venv/bin:$PATH \
    PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1 \
    PYTHONPATH=/app \
    MALLOC_ARENA_MAX=2 \
    ORT_DISABLE_TELEMETRY=1 \
    STUDIO_HOST=127.0.0.1 \
    STUDIO_PORT=4100 \
    STUDIO_MODELS_DIR=/models \
    STUDIO_MODELS_SHA256=/app/models.sha256
WORKDIR /app
USER 10001:10001
EXPOSE 4100
ENTRYPOINT ["python", "-m", "v2i_studio"]
