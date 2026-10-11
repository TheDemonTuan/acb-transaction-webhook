# Piper/ONNX and lameenc publish glibc manylinux wheels, including ARM64.
FROM python:3.13.15-slim-bookworm AS builder
WORKDIR /app
COPY requirements.lock ./
RUN python -m venv /opt/venv && \
    /opt/venv/bin/pip install --only-binary=:all: --no-cache-dir --require-hashes -r requirements.lock && \
    rm -rf /opt/venv/bin/pip /opt/venv/bin/pip3 /opt/venv/bin/pip3.13 /opt/venv/lib/python3.13/site-packages/pip /opt/venv/lib/python3.13/site-packages/pip-*.dist-info

FROM python:3.13.15-slim-bookworm
ENV PATH="/opt/venv/bin:$PATH" \
    PYTHONUNBUFFERED=1 \
    PYTHONDONTWRITEBYTECODE=1 \
    OMP_NUM_THREADS=1 \
    OPENBLAS_NUM_THREADS=1 \
    MKL_NUM_THREADS=1 \
    PORT=8081 \
    TTS_VOICEPACK_ROOT=/app/assets/voicepacks
WORKDIR /app
RUN apt-get update && \
    apt-get install -y --no-install-recommends libgomp1 libstdc++6 && \
    rm -rf /var/lib/apt/lists/* /usr/local/bin/pip /usr/local/bin/pip3 /usr/local/bin/pip3.13 /usr/local/lib/python3.13/site-packages/pip /usr/local/lib/python3.13/site-packages/pip-*.dist-info && \
    groupadd --gid 1000 appgroup && useradd --uid 1000 --gid appgroup --no-create-home appuser
COPY --from=builder /opt/venv /opt/venv
COPY app ./app
COPY assets ./assets
COPY licenses ./licenses
COPY Dockerfile requirements.txt requirements.lock requirements-dev.txt requirements-dev.lock /app/licenses/application/
COPY scripts /app/licenses/application/scripts
# Both inference inputs and corresponding dependency sources ship read-only.
RUN chown -R root:root /app /opt/venv && chmod -R a-w /app /opt/venv
USER appuser
EXPOSE 8081
HEALTHCHECK --interval=10s --timeout=3s --retries=3 --start-period=30s \
    CMD python -c "import json,urllib.request; d=json.load(urllib.request.urlopen('http://127.0.0.1:8081/health',timeout=2)); assert d['piper']['available']"
CMD ["uvicorn", "app.main:app", "--host", "0.0.0.0", "--port", "8081", "--workers", "1"]
