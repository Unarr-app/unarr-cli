# ---- Build stage ----
# Build context must be the parent directory containing both torrentclaw-cli/
# and go-client/. Use: docker build -f torrentclaw-cli/Dockerfile .
# Or use the provided docker-build.sh script.
FROM golang:1.24-alpine AS builder

RUN apk add --no-cache git ca-certificates

# Copy go-client dependency (local replace in go.mod -> ../go-client)
WORKDIR /deps
COPY go-client/ /deps/go-client/

# Copy go.mod/go.sum first for layer caching
WORKDIR /src
COPY torrentclaw-cli/go.mod torrentclaw-cli/go.sum ./
RUN go mod edit -replace github.com/torrentclaw/go-client=/deps/go-client
RUN go mod download

# Copy source (changes here won't invalidate mod cache)
COPY torrentclaw-cli/ .
RUN go mod edit -replace github.com/torrentclaw/go-client=/deps/go-client

RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /unarr ./cmd/unarr/

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
