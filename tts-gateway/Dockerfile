FROM python:3.13.15-alpine3.24

ENV PYTHONUNBUFFERED=1 \
    PYTHONDONTWRITEBYTECODE=1 \
    PORT=8081

WORKDIR /app

# Upgrade the base image security baseline before installing dependencies.
RUN apk upgrade --no-cache

# Copy dependencies
COPY requirements.txt requirements.lock ./
RUN pip install --no-cache-dir -r requirements.lock

# Create unprivileged user
RUN addgroup -g 1000 -S appgroup && \
    adduser -u 1000 -S -G appgroup appuser

COPY app ./app

RUN chown -R appuser:appgroup /app
USER appuser

EXPOSE 8081

HEALTHCHECK --interval=10s --timeout=3s --retries=3 \
    CMD python -c "import urllib.request; urllib.request.urlopen('http://127.0.0.1:8081/health', timeout=2)"

CMD ["uvicorn", "app.main:app", "--host", "0.0.0.0", "--port", "8081"]
