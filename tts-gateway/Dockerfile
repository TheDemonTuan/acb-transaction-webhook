# Piper/ONNX and lameenc publish glibc manylinux wheels, including ARM64.
FROM python:3.13.15-slim-trixie AS builder
WORKDIR /app
COPY requirements.lock ./
RUN python -m venv /opt/venv && \
    /opt/venv/bin/pip install --only-binary=:all: --no-cache-dir --require-hashes -r requirements.lock && \
    rm -rf /opt/venv/bin/pip /opt/venv/bin/pip3 /opt/venv/bin/pip3.13 /opt/venv/lib/python3.13/site-packages/pip /opt/venv/lib/python3.13/site-packages/pip-*.dist-info

FROM python:3.13.15-slim-trixie
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
    apt-get upgrade -y && \
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
# No mounts, login, systemd, Perl or terminal UI run in this sidecar.
# Purge their actual files and dpkg records, not scanner metadata alone.
# Dependency overrides are limited to the immutable application-only image;
# the constrained ARM smoke must prove HTTP, inference and cancellation.
RUN dpkg --purge --force-depends --force-remove-essential \
    bsdutils util-linux mount perl-base ncurses-base ncurses-bin \
    libncursesw6 libtinfo6 libacl1 libblkid1 liblastlog2-2 libmount1 \
    libsmartcols1 libsystemd0 libudev1 libuuid1
# Remove the protected login package files and clear its dpkg status entry
RUN rm -f /bin/login /usr/bin/login /etc/pam.d/login /etc/securetty && \
    python3 -c "from pathlib import Path; p = Path('/var/lib/dpkg/status'); text = p.read_text(encoding='utf-8', errors='ignore'); text = '\n\n'.join(b for b in text.split('\n\n') if not b.startswith('Package: login\n') and not b.startswith('Package: login ')); p.write_text(text, encoding='utf-8')"
USER appuser
EXPOSE 8081
HEALTHCHECK --interval=10s --timeout=3s --retries=3 --start-period=30s \
    CMD python -c "import json,urllib.request; d=json.load(urllib.request.urlopen('http://127.0.0.1:8081/health',timeout=2)); assert d['piper']['available']"
CMD ["uvicorn", "app.main:app", "--host", "0.0.0.0", "--port", "8081", "--workers", "1"]
