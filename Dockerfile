# syntax=docker/dockerfile:1.7

FROM oven/bun:1.4.2-debian AS web-builder
WORKDIR /src/web
COPY web/package.json web/bun.lock ./
RUN bun install --frozen-lockfile
COPY web/ ./
RUN bunx --bun tsc --noEmit && bunx --bun vite build

FROM nginxinc/nginx-unprivileged:1.29.4-alpine AS frontend
USER root
RUN apk update && apk upgrade --no-cache
COPY deploy/frontend-nginx.conf /etc/nginx/conf.d/default.conf
COPY --from=web-builder /src/web/dist /usr/share/nginx/html
USER 101:101

FROM golang:1.27.1-bookworm AS go-base
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ ./cmd/
COPY internal/ ./internal/
RUN mkdir -p -m 0777 /data

FROM go-base AS gateway-builder
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/gateway ./cmd/gateway

FROM go-base AS worker-builder
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/worker ./cmd/worker

FROM go-base AS auth-browser-builder
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/auth-browser ./cmd/auth-browser

FROM go-base AS dbtool-builder
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/dbtool ./cmd/dbtool

FROM gcr.io/distroless/static-debian12:nonroot AS gateway
COPY --from=gateway-builder --chown=1000:1000 /data /data
COPY --from=gateway-builder /out/gateway /gateway
USER 1000:1000
ENTRYPOINT ["/gateway"]

FROM gcr.io/distroless/static-debian12:nonroot AS worker
COPY --from=worker-builder --chown=1000:1000 /data /data
COPY --from=worker-builder /out/worker /worker
USER 1000:1000
ENTRYPOINT ["/worker"]

FROM gcr.io/distroless/static-debian12:nonroot AS dbtool
COPY --from=dbtool-builder --chown=1000:1000 /data /data
COPY --from=dbtool-builder /out/dbtool /dbtool
USER 1000:1000
ENTRYPOINT ["/dbtool"]
