# ---- Build stage ----
FROM golang:1.25-alpine AS builder

RUN apk add --no-cache git ca-certificates

WORKDIR /src

# Copy go.mod/go.sum first for layer caching
COPY go.mod go.sum ./
RUN go mod download

# Copy source
COPY . .

ARG VERSION=dev
RUN CGO_ENABLED=0 go build -ldflags="-s -w -X github.com/torrentclaw/unarr/internal/cmd.Version=${VERSION}" -trimpath -o /unarr ./cmd/unarr/

# ---- Runtime stage ----
FROM alpine:3.21

RUN apk add --no-cache ca-certificates tzdata

# Non-root user (UID 1000 matches typical host user for volume permissions)
RUN addgroup -g 1000 unarr && adduser -u 1000 -G unarr -D -h /home/unarr unarr

# Default directories
RUN mkdir -p /config /downloads /data && \
    chown -R unarr:unarr /config /downloads /data

USER unarr

COPY --from=builder /unarr /usr/local/bin/unarr

# Environment: point config/data to container paths
ENV UNARR_CONFIG_DIR=/config
ENV UNARR_DOWNLOAD_DIR=/downloads
ENV XDG_DATA_HOME=/data

VOLUME ["/config", "/downloads", "/data"]

ENTRYPOINT ["unarr"]
CMD ["start"]
