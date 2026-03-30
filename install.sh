#!/bin/sh
# unarr — cross-platform installer (Linux / macOS)
# Usage: curl -fsSL https://get.unarr.com/install.sh | sh
#    or: curl -fsSL https://raw.githubusercontent.com/torrentclaw/unarr/main/install.sh | sh
#
# Options (env vars):
#   INSTALL_DIR=/usr/local/bin  — where to place the binary (default: /usr/local/bin or ~/.local/bin)
#   VERSION=0.5.0               — specific version (default: latest)
#   METHOD=binary|docker        — force install method (default: auto-detect)
set -e

REPO="torrentclaw/unarr"
BINARY="unarr"

# ---- Colors (only if terminal) ----
if [ -t 1 ]; then
    BOLD="\033[1m"
    GREEN="\033[32m"
    YELLOW="\033[33m"
    RED="\033[31m"
    CYAN="\033[36m"
    RESET="\033[0m"
else
    BOLD="" GREEN="" YELLOW="" RED="" CYAN="" RESET=""
fi

info()  { printf "${CYAN}→${RESET} %s\n" "$1"; }
ok()    { printf "${GREEN}✓${RESET} %s\n" "$1"; }
warn()  { printf "${YELLOW}!${RESET} %s\n" "$1"; }
error() { printf "${RED}✗${RESET} %s\n" "$1" >&2; exit 1; }

# ---- Detect OS ----
detect_os() {
    OS="$(uname -s | tr '[:upper:]' '[:lower:]')"
    case "$OS" in
        linux*)  OS="linux" ;;
        darwin*) OS="darwin" ;;
        mingw*|msys*|cygwin*) OS="windows" ;;
        *)       error "Unsupported OS: $OS. Use install.ps1 for Windows." ;;
    esac
}

# ---- Detect architecture ----
detect_arch() {
    ARCH="$(uname -m)"
    case "$ARCH" in
        x86_64|amd64)   ARCH="amd64" ;;
        aarch64|arm64)  ARCH="arm64" ;;
        *)              error "Unsupported architecture: $ARCH" ;;
    esac
}

# ---- Detect if we can write to /usr/local/bin ----
detect_install_dir() {
    if [ -n "$INSTALL_DIR" ]; then
        return
    fi

    if [ -w "/usr/local/bin" ]; then
        INSTALL_DIR="/usr/local/bin"
    elif [ -d "$HOME/.local/bin" ]; then
        INSTALL_DIR="$HOME/.local/bin"
    else
        mkdir -p "$HOME/.local/bin"
        INSTALL_DIR="$HOME/.local/bin"
    fi
}

# ---- Check if command exists ----
has() {
    command -v "$1" >/dev/null 2>&1
}

# ---- HTTP download (curl or wget) ----
download() {
    url="$1"
    output="$2"
    if has curl; then
        curl -fsSL -o "$output" "$url"
    elif has wget; then
        wget -qO "$output" "$url"
    else
        error "Neither curl nor wget found. Install one and retry."
    fi
}

# ---- Fetch text (for API) ----
fetch() {
    url="$1"
    if has curl; then
        curl -fsSL "$url"
    elif has wget; then
        wget -qO- "$url"
    fi
}

# ---- Get latest version from GitHub ----
get_latest_version() {
    if [ -n "$VERSION" ]; then
        return
    fi

    info "Checking latest version..."

    # Try GitHub API first
    response=$(fetch "https://api.github.com/repos/$REPO/releases/latest" 2>/dev/null || true)

    if [ -n "$response" ]; then
        # Parse tag_name from JSON (works without jq)
        VERSION=$(echo "$response" | grep '"tag_name"' | head -1 | sed 's/.*"tag_name"[[:space:]]*:[[:space:]]*"v\{0,1\}\([^"]*\)".*/\1/')
    fi

    if [ -z "$VERSION" ]; then
        # Fallback: follow redirect from /releases/latest
        if has curl; then
            VERSION=$(curl -fsSI "https://github.com/$REPO/releases/latest" 2>/dev/null | grep -i '^location:' | sed 's|.*/v\{0,1\}||' | tr -d '\r\n')
        fi
    fi

    if [ -z "$VERSION" ]; then
        error "Could not determine latest version. Set VERSION=x.y.z and retry."
    fi
}

# ---- Install via binary ----
install_binary() {
    get_latest_version

    # Strip leading 'v' if present
    VERSION="${VERSION#v}"

    archive="${BINARY}_${VERSION}_${OS}_${ARCH}.tar.gz"
    url="https://github.com/$REPO/releases/download/v${VERSION}/${archive}"

    info "Downloading $BINARY v$VERSION for $OS/$ARCH..."

    tmpdir=$(mktemp -d)

    download "$url" "$tmpdir/$archive"

    info "Extracting..."
    tar -xzf "$tmpdir/$archive" -C "$tmpdir"

    # Find binary (may be at root or in a subdir)
    bin_path=$(find "$tmpdir" -name "$BINARY" -type f | head -1)
    if [ -z "$bin_path" ]; then
        rm -rf "$tmpdir"
        error "Binary not found in archive"
    fi

    chmod +x "$bin_path"

    # Install
    if [ -w "$INSTALL_DIR" ]; then
        mv "$bin_path" "$INSTALL_DIR/$BINARY"
    else
        info "Requires sudo to install to $INSTALL_DIR"
        sudo mv "$bin_path" "$INSTALL_DIR/$BINARY"
    fi

    rm -rf "$tmpdir"

    ok "Installed $BINARY v$VERSION to $INSTALL_DIR/$BINARY"

    # Check PATH
    case ":$PATH:" in
        *":$INSTALL_DIR:"*) ;;
        *)
            warn "$INSTALL_DIR is not in your PATH."
            printf "    Add it with: ${BOLD}export PATH=\"%s:\$PATH\"${RESET}\n" "$INSTALL_DIR"
            ;;
    esac
}

# ---- Install via Docker ----
install_docker() {
    if ! has docker; then
        error "Docker not found. Install Docker first: https://docs.docker.com/get-docker/"
    fi

    info "Pulling torrentclaw/unarr:latest..."
    docker pull torrentclaw/unarr:latest 2>/dev/null || {
        info "Image not on Docker Hub yet, building from source..."
        tmpdir=$(mktemp -d)

        if has git; then
            git clone --depth 1 "https://github.com/$REPO.git" "$tmpdir/unarr"
            docker build -t torrentclaw/unarr:latest "$tmpdir/unarr"
            rm -rf "$tmpdir"
        else
            rm -rf "$tmpdir"
            error "git not found. Install git or pull the image manually."
        fi
    }

    ok "Docker image ready: torrentclaw/unarr:latest"

    printf "\n${BOLD}Quick start:${RESET}\n"
    cat <<'DOCKER_USAGE'

  # 1. Create config directory
  mkdir -p ~/.config/unarr

  # 2. Run setup (interactive)
  docker run -it --rm \
    -v ~/.config/unarr:/config \
    torrentclaw/unarr init

  # 3. Start daemon
  docker run -d --name unarr \
    --restart unless-stopped \
    --network host \
    --read-only \
    --memory 512m \
    -v ~/.config/unarr:/config \
    -v ~/Media:/downloads \
    torrentclaw/unarr

  # Or use the provided docker-compose.yml:
  # curl -fsSL https://raw.githubusercontent.com/torrentclaw/unarr/main/docker-compose.yml > docker-compose.yml
  # docker compose up -d

DOCKER_USAGE
}

# ---- Uninstall ----
uninstall() {
    info "Uninstalling $BINARY..."

    # Remove binary
    for dir in /usr/local/bin "$HOME/.local/bin" /usr/bin; do
        if [ -f "$dir/$BINARY" ]; then
            if [ -w "$dir" ]; then
                rm -f "$dir/$BINARY"
            else
                sudo rm -f "$dir/$BINARY"
            fi
            ok "Removed $dir/$BINARY"
        fi
    done

    # Remove Docker
    if has docker; then
        docker rm -f unarr 2>/dev/null && ok "Removed Docker container 'unarr'"
        docker rmi torrentclaw/unarr:latest 2>/dev/null && ok "Removed Docker image"
    fi

    ok "Uninstalled. Config remains at ~/.config/unarr/ (delete manually if desired)."
    exit 0
}

# ---- Interactive menu ----
interactive_menu() {
    printf "\n"
    printf "  ${BOLD}unarr Installer${RESET}\n"
    printf "  ────────────────────────\n"
    printf "\n"
    printf "  Detected: ${CYAN}$OS/$ARCH${RESET}\n"
    printf "\n"
    printf "  Install method:\n"
    printf "\n"
    printf "    ${BOLD}1)${RESET} Binary — standalone executable, no dependencies\n"
    printf "    ${BOLD}2)${RESET} Docker — sandboxed, isolated filesystem access ${GREEN}(recommended)${RESET}\n"

    if [ "$OS" = "darwin" ] || has brew; then
        printf "    ${BOLD}3)${RESET} Homebrew — brew install torrentclaw/tap/unarr\n"
    fi

    printf "    ${BOLD}u)${RESET} Uninstall\n"
    printf "\n"
    printf "  Choice [1/2"
    if [ "$OS" = "darwin" ] || has brew; then printf "/3"; fi
    printf "]: "

    read -r choice

    case "$choice" in
        1)      METHOD="binary" ;;
        2)      METHOD="docker" ;;
        3)
            if [ "$OS" = "darwin" ] || has brew; then
                METHOD="brew"
            else
                error "Invalid choice"
            fi
            ;;
        u|U)    uninstall ;;
        *)      error "Invalid choice: $choice" ;;
    esac
}

# ---- Main ----
main() {
    detect_os
    detect_arch
    detect_install_dir

    # Non-interactive if METHOD is set
    if [ -z "$METHOD" ]; then
        # If piped (non-interactive), default to binary
        if [ ! -t 0 ]; then
            METHOD="binary"
        else
            interactive_menu
        fi
    fi

    printf "\n"

    case "$METHOD" in
        binary)
            install_binary
            printf "\n  Run ${BOLD}unarr init${RESET} to get started.\n\n"
            ;;
        docker)
            install_docker
            ;;
        brew)
            if ! has brew; then
                error "Homebrew not found. Install it first: https://brew.sh"
            fi
            info "Installing via Homebrew..."
            brew install torrentclaw/tap/unarr
            ok "Installed via Homebrew"
            printf "\n  Run ${BOLD}unarr init${RESET} to get started.\n\n"
            ;;
        *)
            error "Unknown method: $METHOD"
            ;;
    esac
}

main "$@"
