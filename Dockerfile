# syntax=docker/dockerfile:1.7

FROM oven/bun:1.4.2-debian AS web-builder
WORKDIR /src/web
COPY web/package.json web/bun.lock ./
RUN bun install --frozen-lockfile
COPY web/ ./
RUN bunx --bun tsc --noEmit && bunx --bun vite build

FROM nginxinc/nginx-unprivileged:1.29.4-alpine AS frontend
COPY deploy/frontend-nginx.conf /etc/nginx/conf.d/default.conf
COPY --from=web-builder /src/web/dist /usr/share/nginx/html
USER 101:101

FROM golang:1.27.1-bookworm AS go-builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ ./cmd/
COPY internal/ ./internal/
RUN mkdir -p -m 0777 /data
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/gateway ./cmd/gateway && \
    CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/worker ./cmd/worker && \
    CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/auth-browser ./cmd/auth-browser && \
    CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/dbtool ./cmd/dbtool

FROM gcr.io/distroless/static-debian12:nonroot AS gateway
COPY --from=go-builder --chown=1000:1000 /data /data
COPY --from=go-builder /out/gateway /gateway
USER 1000:1000
ENTRYPOINT ["/gateway"]

FROM gcr.io/distroless/static-debian12:nonroot AS worker
COPY --from=go-builder --chown=1000:1000 /data /data
COPY --from=go-builder /out/worker /worker
USER 1000:1000
ENTRYPOINT ["/worker"]

FROM gcr.io/distroless/static-debian12:nonroot AS dbtool
COPY --from=go-builder --chown=1000:1000 /data /data
COPY --from=go-builder /out/dbtool /dbtool
USER 1000:1000
ENTRYPOINT ["/dbtool"]
