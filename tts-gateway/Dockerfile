FROM python:3.13.15-alpine3.24 AS builder

WORKDIR /app

COPY requirements.lock ./
RUN apk upgrade --no-cache && \
    python -m venv /opt/venv && \
    /opt/venv/bin/pip install --no-cache-dir -r requirements.lock && \
    rm -rf /opt/venv/bin/pip* \
      /opt/venv/lib/python3.13/site-packages/pip* \
      /opt/venv/lib/python3.13/site-packages/setuptools* \
      /opt/venv/lib/python3.13/site-packages/pkg_resources* \
      /opt/venv/lib/python3.13/site-packages/wheel*

FROM python:3.13.15-alpine3.24

ENV PATH="/opt/venv/bin:$PATH" \
    PYTHONUNBUFFERED=1 \
    PYTHONDONTWRITEBYTECODE=1 \
    PORT=8081

WORKDIR /app

RUN apk upgrade --no-cache && \
    rm -rf /usr/local/bin/pip* \
      /usr/local/lib/python3.13/site-packages/pip* \
      /usr/local/lib/python3.13/site-packages/setuptools* && \
    addgroup -g 1000 -S appgroup && \
    adduser -u 1000 -S -G appgroup appuser

COPY --from=builder /opt/venv /opt/venv
COPY app ./app

RUN chown -R appuser:appgroup /app /opt/venv
USER appuser

EXPOSE 8081

HEALTHCHECK --interval=10s --timeout=3s --retries=3 \
    CMD python -c "import urllib.request; urllib.request.urlopen('http://127.0.0.1:8081/health', timeout=2)"

CMD ["uvicorn", "app.main:app", "--host", "0.0.0.0", "--port", "8081"]
