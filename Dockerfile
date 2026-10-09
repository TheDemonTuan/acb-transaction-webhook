# syntax=docker/dockerfile:1.7

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
