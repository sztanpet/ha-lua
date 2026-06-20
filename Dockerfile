ARG BUILD_FROM=ghcr.io/home-assistant/base-debian:latest

# CI builds each arch natively on its own runner (amd64 on ubuntu-24.04,
# aarch64 on ubuntu-24.04-arm), so a plain `go build` (no GOARCH) already
# produces the right binary for that arch. Don't map TARGETARCH to GOARCH: the
# HA arch names (aarch64) are not Go's (arm64), and the naive mapping ships a
# broken image.
FROM golang:1.26-bookworm AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build \
    -trimpath -ldflags="-s -w" \
    -o /usr/local/bin/ha-lua ./cmd/ha-lua

FROM ${BUILD_FROM}
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates \
 && rm -rf /var/lib/apt/lists/*
COPY --from=builder /usr/local/bin/ha-lua /usr/local/bin/ha-lua
COPY run.sh /run.sh
RUN chmod a+x /run.sh
CMD ["/run.sh"]
