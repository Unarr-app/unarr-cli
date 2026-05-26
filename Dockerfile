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
FROM alpine:3.22

# Use Alpine's native musl ffmpeg + ffprobe instead of the johnvansickle /
# BtbN static glibc builds — those need a glibc shim on Alpine and the
# vector-math symbols the GPL builds reference are not satisfiable by
# gcompat. Alpine ships ffmpeg ~7.x which is fine for the HLS transcoding
# pipeline (libx264 + libfdk-aac alternatives included).
RUN apk upgrade --no-cache && \
    apk add --no-cache ca-certificates tzdata ffmpeg wget

# Bundle cloudflared so `unarr funnel on` (default: on, see config defaults)
# Just Works on a headless container with no first-run network round-trip.
# TARGETARCH is set automatically by Docker buildx during cross-builds.
ARG TARGETARCH=amd64
RUN case "$TARGETARCH" in \
      amd64) CF_ARCH=amd64 ;; \
      arm64) CF_ARCH=arm64 ;; \
      arm)   CF_ARCH=armhf ;; \
      *)     echo "unsupported TARGETARCH=$TARGETARCH" >&2; exit 1 ;; \
    esac && \
    wget -qO /usr/local/bin/cloudflared "https://github.com/cloudflare/cloudflared/releases/latest/download/cloudflared-linux-$CF_ARCH" && \
    chmod +x /usr/local/bin/cloudflared

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
