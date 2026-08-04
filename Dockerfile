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
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -ldflags="-s -w -X github.com/Unarr-app/unarr-cli/internal/cmd.Version=${VERSION}" -trimpath -o /unarr ./cmd/unarr/

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
# libdrm2/libva2/libva-drm2 → VAAPI/QSV userspace the bundled BtbN ffmpeg
#         dlopen()s for Intel QuickSync (h264_qsv/hevc_qsv). Without libdrm.so.2
#         the qsv encoder aborts on the very first frame ("libdrm.so.2: cannot
#         open shared object file" → `load_library: Assertion 0` → core dump), so
#         an Intel-GPU host (renderD128) never produces a segment and web
#         playback hangs forever on init.mp4. All three exist on arm64 too and
#         are inert there, so they live on the common line; the Intel-only driver
#         + oneVPL runtime are installed in an amd64-guarded block below.
RUN apt-get update && \
    apt-get install -y --no-install-recommends \
      ca-certificates tzdata wget xz-utils par2 p7zip-full libvulkan1 \
      libdrm2 libva2 libva-drm2 gosu && \
    rm -rf /var/lib/apt/lists/*

# Intel QuickSync (QSV) runtime — amd64 only. The oneVPL dispatcher (libvpl2) +
# the Gen GPU runtime (libmfx-gen1.2) are what the bundled ffmpeg loads for
# h264_qsv/hevc_qsv; intel-media-va-driver-non-free is the iHD VAAPI driver
# covering Gen9+ incl the Alder Lake-N SoCs that UGREEN / Intel NAS boxes ship.
# These packages have no arm64 candidate (Intel x86 media stack), so installing
# them on the common line would break the arm64 buildx leg — guard by arch. iHD
# lives in non-free, absent from bookworm-slim's default sources, so enable the
# component first. Inert on NVIDIA hosts (which use nvenc, not QSV).
# The non-free stanza reuses the base image's deb822 keyring (Signed-By); a plain
# one-line `deb ...` list for the same suite trips apt's "Conflicting values set
# for option Signed-By" on modern bookworm-slim (which ships /etc/apt/sources.list.d
# /debian.sources with the keyring pinned), so it must be deb822 with the same key.
RUN if [ "$(dpkg --print-architecture)" = "amd64" ]; then \
      printf 'Types: deb\nURIs: http://deb.debian.org/debian\nSuites: bookworm\nComponents: non-free non-free-firmware\nSigned-By: /usr/share/keyrings/debian-archive-keyring.gpg\n' \
        > /etc/apt/sources.list.d/non-free.sources && \
      apt-get update && \
      apt-get install -y --no-install-recommends \
        intel-media-va-driver-non-free libmfx-gen1.2 libvpl2 && \
      rm -rf /var/lib/apt/lists/*; \
    fi

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

# The container starts as root ONLY to chown the mounts, then execs as
# PUID:PGID (default 1000:1000 — identical to the old `USER unarr`). Baking the
# uid instead made every NAS fail: Synology uids start at 1024, unRAID uses
# 99:100, so the bind-mounted /config and /downloads were unwritable and the
# agent died on its first write. See docker-entrypoint.sh.
COPY docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh
RUN chmod +x /usr/local/bin/docker-entrypoint.sh

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

# Select the iHD VAAPI driver for Intel QuickSync (installed on amd64 above).
# Without this libva may probe the wrong/absent driver on some hosts; iHD is the
# modern Intel driver (Gen9+). Harmless on NVIDIA/arm64 hosts where no libva
# device is used (QSV simply isn't selected). Pass /dev/dri via `--device`.
ENV LIBVA_DRIVER_NAME=iHD

VOLUME ["/config", "/downloads", "/data"]

# Without this, a container whose daemon has died reports "running" forever:
# the entrypoint process is still alive, and nothing else looks at whether the
# thing it supervises is. Docker, Portainer, Swarm, k8s and `compose --wait`
# all read this and nothing else.
#
# --quick is the load-bearing flag. It runs ONLY the local checks — config,
# download dir, disk, daemon liveness — and makes NO network call. A
# healthcheck that reaches the API marks the container unhealthy on every
# transient blip, Docker restarts it, and one ISP hiccup becomes a restart loop
# across the fleet. Warnings never produce a non-zero exit for the same reason:
# Docker's only response to "unhealthy" is to restart, and a missing par2 is
# not a reason to restart anything.
#
# start-period is generous because the first boot registers the agent and scans
# the library, and neither is instant on a NAS.
HEALTHCHECK --interval=60s --timeout=15s --start-period=90s --retries=3 \
  CMD unarr doctor --quick --json > /dev/null || exit 1

ENTRYPOINT ["/usr/local/bin/docker-entrypoint.sh"]
# `up` is `start` plus one extra ability: if UNARR_AUTHKEY is set it redeems the
# key first. With CMD=start, `docker run -e UNARR_AUTHKEY=… unarr/cli` ignored the
# key and died with "no API key configured" — the container has no tty for the
# wizard, so that was the end of the road. `up` with no key and a stored
# credential behaves exactly like `start`.
CMD ["up"]
