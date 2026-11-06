# syntax=docker/dockerfile:1.7@sha256:a57df69d0ea827fb7266491f2813635de6f17269be881f696fbfdf2d83dda33e

FROM golang:1.27.3-bookworm@sha256:21ade014faf952365b9ecbd8dbdb87176fbacb82fac9fb132d9f2a7b8cc3f132 AS go-base
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

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab AS gateway
COPY --from=gateway-builder --chown=1000:1000 /data /data
COPY --from=gateway-builder /out/gateway /gateway
USER 1000:1000
ENTRYPOINT ["/gateway"]

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab AS worker
COPY --from=worker-builder --chown=1000:1000 /data /data
COPY --from=worker-builder /out/worker /worker
USER 1000:1000
ENTRYPOINT ["/worker"]

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab AS dbtool
COPY --from=dbtool-builder --chown=1000:1000 /data /data
COPY --from=dbtool-builder /out/dbtool /dbtool
USER 1000:1000
ENTRYPOINT ["/dbtool"]
