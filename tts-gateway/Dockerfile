# Piper/ONNX and lameenc publish glibc manylinux wheels, including ARM64.
FROM python:3.13.16-slim-trixie@sha256:cf50a4f6ac4f026458b08d5222023d700ac806410dff5e9ca8728fa44f2b850f AS builder
WORKDIR /app
COPY requirements.lock ./
COPY scripts/fetch_vieneu_assets.py ./scripts/
COPY assets/models/vieneu/provenance.json ./assets/models/vieneu/
RUN python -m venv /opt/venv && \
    /opt/venv/bin/pip install --only-binary=:all: --no-cache-dir --require-hashes -r requirements.lock && \
    /opt/venv/bin/python ./scripts/fetch_vieneu_assets.py --output /app/assets/models/vieneu && \
    /opt/venv/bin/python ./scripts/fetch_vieneu_assets.py --output /app/assets/models/vieneu --verify-only && \
    rm -rf /opt/venv/bin/pip /opt/venv/bin/pip3 /opt/venv/bin/pip3.13 /opt/venv/lib/python3.13/site-packages/pip /opt/venv/lib/python3.13/site-packages/pip-*.dist-info

FROM python:3.13.16-slim-trixie@sha256:cf50a4f6ac4f026458b08d5222023d700ac806410dff5e9ca8728fa44f2b850f
ENV PATH="/opt/venv/bin:$PATH" \
    PYTHONUNBUFFERED=1 \
    PYTHONDONTWRITEBYTECODE=1 \
    OMP_NUM_THREADS=1 \
    OPENBLAS_NUM_THREADS=1 \
    MKL_NUM_THREADS=1 \
    PORT=8081 \
    TTS_VOICEPACK_ROOT=/app/assets/voicepacks \
    HF_HOME=/app/assets/models/vieneu/hf \
    HF_HUB_OFFLINE=1 \
    HF_HUB_DISABLE_TELEMETRY=1
WORKDIR /app
RUN apt-get update && \
    apt-get upgrade -y && \
    apt-get install -y --no-install-recommends libgomp1 libstdc++6 && \
    rm -rf /var/lib/apt/lists/* /usr/local/bin/pip /usr/local/bin/pip3 /usr/local/bin/pip3.13 /usr/local/lib/python3.13/site-packages/pip /usr/local/lib/python3.13/site-packages/pip-*.dist-info && \
    groupadd --gid 1000 appgroup && useradd --uid 1000 --gid appgroup --no-create-home appuser
COPY --from=builder /opt/venv /opt/venv
COPY --from=builder /app/assets/models/vieneu ./assets/models/vieneu
COPY app ./app
COPY assets ./assets
COPY licenses ./licenses
COPY Dockerfile requirements.txt requirements.lock requirements-dev.txt requirements-dev.lock /app/licenses/application/
COPY scripts /app/licenses/application/scripts
# Both inference inputs and corresponding dependency sources ship read-only.
RUN chown -R root:root /app /opt/venv && chmod -R a-w /app /opt/venv
# No mounts, login, systemd, Perl or terminal UI run in this sidecar.
# Purge their actual files and dpkg records, not scanner metadata alone.
# Dependency overrides are limited to the immutable application-only image;
# the constrained ARM smoke must prove HTTP, inference and cancellation.
RUN dpkg --purge --force-depends --force-remove-essential --force-remove-protected \
    login bsdutils util-linux mount perl-base ncurses-base ncurses-bin \
    libncursesw6 libtinfo6 libacl1 libblkid1 liblastlog2-2 libmount1 \
    libsmartcols1 libsystemd0 libudev1 libuuid1
USER appuser
EXPOSE 8081
HEALTHCHECK --interval=30s --timeout=3s --retries=3 --start-period=120s --start-interval=5s \
    CMD python -c "import json,http.client; c=http.client.HTTPConnection('127.0.0.1',8081,timeout=2); c.request('GET','/health'); r=c.getresponse(); assert r.status==200; d=json.loads(r.read()); c.close(); assert d.get('vieneu', {}).get('available') and d.get('piper', {}).get('available'), 'Required providers unavailable'"
CMD ["uvicorn", "app.main:app", "--host", "0.0.0.0", "--port", "8081", "--workers", "1"]
