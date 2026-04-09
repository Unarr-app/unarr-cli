# ---- ffprobe static binary stage ----
# Download a static ffprobe-only build (~30MB) to avoid the full ffmpeg package (~1GB).
# johnvansickle.com provides reliable static builds for amd64/arm64.
FROM alpine:3.22 AS ffprobe-dl

RUN apk add --no-cache curl xz

RUN ARCH=$(uname -m) && \
    case "$ARCH" in \
      x86_64)  SLUG="amd64" ;; \
      aarch64) SLUG="arm64" ;; \
      *) echo "Unsupported arch: $ARCH" && exit 1 ;; \
    esac && \
    curl -fsSL "https://johnvansickle.com/ffmpeg/releases/ffmpeg-release-${SLUG}-static.tar.xz" -o /tmp/ff.tar.xz && \
    tar xJ -f /tmp/ff.tar.xz --strip-components=1 -C /tmp/ && \
    mv /tmp/ffprobe /usr/local/bin/ffprobe && \
    chmod +x /usr/local/bin/ffprobe && \
    rm -rf /tmp/ff.tar.xz /tmp/ffmpeg /tmp/ffmpeg-* && \
    ffprobe -version | head -1

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

RUN apk upgrade --no-cache && \
    apk add --no-cache ca-certificates tzdata

# Non-root user (UID 1000 matches typical host user for volume permissions)
RUN addgroup -g 1000 unarr && adduser -u 1000 -G unarr -D -h /home/unarr unarr

# Default directories
RUN mkdir -p /config /downloads /data && \
    chown -R unarr:unarr /config /downloads /data

USER unarr

COPY --from=builder /unarr /usr/local/bin/unarr
COPY --from=ffprobe-dl /usr/local/bin/ffprobe /usr/local/bin/ffprobe

# Environment: point config/data to container paths
ENV UNARR_CONFIG_DIR=/config
ENV UNARR_DOWNLOAD_DIR=/downloads
ENV XDG_DATA_HOME=/data

VOLUME ["/config", "/downloads", "/data"]

ENTRYPOINT ["unarr"]
CMD ["start"]
