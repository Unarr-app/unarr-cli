# ---- Build stage ----
# Pin the builder to the host's native arch and cross-compile (CGO is off, so
# Go cross-compiles trivially). During multi-arch buildx this keeps `go build`
# at native speed instead of compiling under QEMU emulation for the foreign arch.
FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS builder

RUN apk add --no-cache git ca-certificates

WORKDIR /src

# Copy go.mod/go.sum first for layer caching
COPY go.mod go.sum ./
RUN go mod download

# Copy source
COPY . .

ARG VERSION=dev
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -ldflags="-s -w -X github.com/torrentclaw/unarr/internal/cmd.Version=${VERSION}" -trimpath -o /unarr ./cmd/unarr/

# ---- Runtime stage ----
# glibc base (not Alpine/musl). NVIDIA's userspace — nvidia-smi and the
# libnvidia-encode / libcuda libs that `--gpus all` injects, plus the static
# BtbN ffmpeg that links nvenc — are all glibc ELF. On musl they fail with
# "no such file or directory" (missing glibc loader), so HW transcode is
# impossible on Alpine. bookworm-slim is the smallest base that runs the full
# NVIDIA stack while still falling back to software libx264 when no GPU is
# passed in.
FROM debian:bookworm-slim

# par2  → repair corrupted Usenet segments (without it a single bad segment
#         silently corrupts the output).
# 7z    → archive extractor for RAR/7z-packed downloads (p7zip-full also reads
#         RAR5, so unrar — unavailable as a free Debian package — isn't needed).
# tzdata/ca-certificates → TLS + correct local time for schedules/logs.
# libvulkan1 → the Vulkan loader (libvulkan.so.1). ffmpeg's libplacebo filter
#         (GPU HDR→SDR tonemap) loads Vulkan dynamically through it; without the
#         loader the filter can't reach a GPU even when the NVIDIA driver mounts
#         its ICD. ~150 KB. The agent only USES libplacebo after a functional
#         probe (FFmpegSupportsLibplacebo) succeeds AND a real HW encoder is
#         present, so this is inert on hosts without a working Vulkan GPU.
#
#         NOTE: in this container libplacebo's Vulkan probe ALWAYS fails and the
#         agent falls back to the CPU zscale tonemap chain — by design, not a
#         bug. The nvidia Vulkan ICD is libGLX_nvidia.so.0, whose GL backend
#         (libnvidia-glcore) references glibc malloc hooks removed in glibc 2.34
#         (__malloc_hook/__free_hook/...) and the Xorg symbol ErrorF; on a
#         headless modern-glibc base (debian or ubuntu) those go unresolved so
#         vkCreateInstance returns VK_ERROR_INCOMPATIBLE_DRIVER. We deliberately
#         do NOT chase it (would need `graphics` cap + X11 libs + a 1.4 loader
#         AND a desktop-class glibc/Xorg — fragile, distro+driver coupled). The
#         loader stays so that on the RARE host where Vulkan does come up the
#         probe can use it. nvenc/nvdec (CUDA, not Vulkan) work regardless.
#         GPU HDR tonemap is a bare-metal-binary feature, not a container one.
RUN apt-get update && \
    apt-get install -y --no-install-recommends \
      ca-certificates tzdata wget xz-utils par2 p7zip-full libvulkan1 && \
    rm -rf /var/lib/apt/lists/*

# Arch for the bundled deps below is taken from `dpkg --print-architecture` (the
# real arch of THIS runtime stage), NOT the TARGETARCH build-arg. A baked
# `ARG TARGETARCH=amd64` default used to shadow buildx's per-leg value in this
# stage, so even the published arm64 image bundled an amd64 cloudflared/ffmpeg
# while the unarr binary was native arm64 → "exec format error" when the daemon
# spawned cloudflared → funnel never came up → TV/Stremio connect failed
# ("Failed to get add-on manifest"). dpkg reads the emulated base image's arch,
# so it is correct under buildx cross-builds AND a plain `docker build`.

# Static GPL ffmpeg + ffprobe with nvenc compiled in (BtbN builds). nvenc is
# linked but the actual libnvidia-encode.so is dlopen'd at runtime from the
# host driver that `--gpus all` exposes — so the same binary does HW transcode
# when a GPU is present and falls back to libx264 when it isn't. Placed in
# /usr/local/bin so ResolveFFmpeg picks them up off PATH ahead of any distro
# ffmpeg. arm64 has no nvenc but the build still serves software transcode.
RUN ARCH="$(dpkg --print-architecture)" && \
    case "$ARCH" in \
      amd64) FF_ARCH=linux64 ;; \
      arm64) FF_ARCH=linuxarm64 ;; \
      *)     echo "unsupported arch=$ARCH" >&2; exit 1 ;; \
    esac && \
    wget -4 --tries=3 --timeout=30 -qO /tmp/ffmpeg.tar.xz "https://github.com/BtbN/FFmpeg-Builds/releases/download/latest/ffmpeg-master-latest-${FF_ARCH}-gpl.tar.xz" && \
    mkdir -p /tmp/ff && tar -xJf /tmp/ffmpeg.tar.xz -C /tmp/ff --strip-components=1 && \
    cp /tmp/ff/bin/ffmpeg /tmp/ff/bin/ffprobe /usr/local/bin/ && \
    chmod +x /usr/local/bin/ffmpeg /usr/local/bin/ffprobe && \
    rm -rf /tmp/ffmpeg.tar.xz /tmp/ff

# Bundle cloudflared so `unarr funnel on` (default: on, see config defaults)
# Just Works on a headless container with no first-run network round-trip.
RUN ARCH="$(dpkg --print-architecture)" && \
    case "$ARCH" in \
      amd64)  CF_ARCH=amd64 ;; \
      arm64)  CF_ARCH=arm64 ;; \
      armhf)  CF_ARCH=armhf ;; \
      *)      echo "unsupported arch=$ARCH" >&2; exit 1 ;; \
    esac && \
    wget -4 --tries=3 --timeout=30 -qO /usr/local/bin/cloudflared "https://github.com/cloudflare/cloudflared/releases/latest/download/cloudflared-linux-$CF_ARCH" && \
    chmod +x /usr/local/bin/cloudflared

# Non-root user (UID 1000 matches typical host user for volume permissions)
RUN groupadd -g 1000 unarr && useradd -u 1000 -g 1000 -m -d /home/unarr unarr

# Default directories
RUN mkdir -p /config /downloads /data && \
    chown -R unarr:unarr /config /downloads /data

USER unarr

COPY --from=builder /unarr /usr/local/bin/unarr

# Environment: point config/data to container paths
ENV UNARR_CONFIG_DIR=/config
ENV UNARR_DOWNLOAD_DIR=/downloads
ENV XDG_DATA_HOME=/data

# Mark this as a container install so the agent reports isDocker=true to the web
# (which then shows a `docker pull` command instead of the in-app update button —
# the binary self-update refuses to run in Docker). Covers podman/containerd too,
# which don't create /.dockerenv. See internal/agent/RunningInDocker.
ENV UNARR_DOCKER=1

# NVIDIA passthrough defaults. `--gpus all` alone only grants the "utility" +
# "compute" capabilities; nvenc needs "video", and "graphics" makes the runtime
# mount the NVIDIA Vulkan ICD (nvidia_icd.json — the load-bearing piece — plus
# GLX/EGL libs) so ffmpeg's libplacebo filter (GPU HDR tonemap, paired with
# libvulkan1 above) can create a Vulkan device. "compute" alone does NOT mount
# the ICD. Baking these here means a plain `docker run --gpus all` (or the compose
# device reservation) lights up HW transcode + GPU tonemap with zero extra flags.
# Harmless when no GPU is attached.
ENV NVIDIA_VISIBLE_DEVICES=all
ENV NVIDIA_DRIVER_CAPABILITIES=video,compute,utility,graphics

VOLUME ["/config", "/downloads", "/data"]

ENTRYPOINT ["unarr"]
CMD ["start"]
