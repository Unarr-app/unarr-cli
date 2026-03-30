# unarr — Windows installer (PowerShell 5.1+)
# Usage: irm https://get.unarr.com/install.ps1 | iex
#    or: irm https://raw.githubusercontent.com/torrentclaw/unarr/main/install.ps1 | iex
#
# Options (env vars):
#   $env:INSTALL_DIR = "C:\path"  — where to place the binary
#   $env:VERSION = "0.5.0"        — specific version
#   $env:METHOD = "binary|docker" — force install method

param(
    [string]$Method,
    [string]$Version,
    [string]$InstallDir
)

$ErrorActionPreference = "Stop"

$Repo = "torrentclaw/unarr"
$Binary = "unarr.exe"

# ---- Helpers ----
function Write-Info  { param($msg) Write-Host "→ $msg" -ForegroundColor Cyan }
function Write-Ok    { param($msg) Write-Host "✓ $msg" -ForegroundColor Green }
function Write-Warn  { param($msg) Write-Host "! $msg" -ForegroundColor Yellow }
function Write-Err   { param($msg) Write-Error "✗ $msg"; throw "Installation failed: $msg" }

# ---- Detect architecture ----
function Get-Arch {
    $arch = [System.Runtime.InteropServices.RuntimeInformation]::OSArchitecture
    switch ($arch) {
        "X64"   { return "amd64" }
        "Arm64" { return "arm64" }
        default {
            # Fallback for older PowerShell
            if ($env:PROCESSOR_ARCHITECTURE -eq "AMD64") { return "amd64" }
            if ($env:PROCESSOR_ARCHITECTURE -eq "ARM64") { return "arm64" }
            Write-Err "Unsupported architecture: $arch"
        }
    }
}

# ---- Detect install directory ----
function Get-InstallDir {
    if ($InstallDir)           { return $InstallDir }
    if ($env:INSTALL_DIR)      { return $env:INSTALL_DIR }

    # Default: %LOCALAPPDATA%\Programs\unarr
    $dir = Join-Path $env:LOCALAPPDATA "Programs\unarr"
    if (-not (Test-Path $dir)) {
        New-Item -ItemType Directory -Path $dir -Force | Out-Null
    }
    return $dir
}

# ---- Get latest version ----
function Get-LatestVersion {
    if ($Version)          { return $Version.TrimStart("v") }
    if ($env:VERSION)      { return $env:VERSION.TrimStart("v") }

    Write-Info "Checking latest version..."
    try {
        $release = Invoke-RestMethod "https://api.github.com/repos/$Repo/releases/latest"
        return $release.tag_name.TrimStart("v")
    } catch {
        # Fallback: follow redirect
        try {
            $response = Invoke-WebRequest "https://github.com/$Repo/releases/latest" -MaximumRedirection 0 -ErrorAction SilentlyContinue
            $location = $response.Headers.Location
            if ($location -match "/v?(\d+\.\d+\.\d+)") {
                return $Matches[1]
            }
        } catch {}
    }
    Write-Err "Could not determine latest version. Set `$env:VERSION='x.y.z' and retry."
}

# ---- Add to PATH ----
function Add-ToPath {
    param($dir)
    $currentPath = [Environment]::GetEnvironmentVariable("PATH", "User")
    if ($currentPath -split ";" -contains $dir) { return }

    Write-Info "Adding $dir to user PATH..."
    [Environment]::SetEnvironmentVariable("PATH", "$currentPath;$dir", "User")
    $env:PATH = "$env:PATH;$dir"
    Write-Ok "Added to PATH (restart terminal for full effect)"
}

# ---- Install binary ----
function Install-Binary {
    $ver = Get-LatestVersion
    $arch = Get-Arch
    $dir = Get-InstallDir

    $archive = "unarr_${ver}_windows_${arch}.zip"
    $url = "https://github.com/$Repo/releases/download/v${ver}/$archive"

    Write-Info "Downloading unarr v$ver for windows/$arch..."

    $tmpDir = Join-Path $env:TEMP "unarr-install-$(Get-Random)"
    New-Item -ItemType Directory -Path $tmpDir -Force | Out-Null

    try {
        $zipPath = Join-Path $tmpDir $archive
        Invoke-WebRequest -Uri $url -OutFile $zipPath -UseBasicParsing

        Write-Info "Extracting..."
        Expand-Archive -Path $zipPath -DestinationPath $tmpDir -Force

        # Find binary
        $binPath = Get-ChildItem -Path $tmpDir -Recurse -Filter "unarr.exe" | Select-Object -First 1
        if (-not $binPath) {
            Write-Err "Binary not found in archive"
        }

        Copy-Item $binPath.FullName (Join-Path $dir $Binary) -Force
        Write-Ok "Installed unarr v$ver to $dir\$Binary"

        Add-ToPath $dir
    } finally {
        Remove-Item -Recurse -Force $tmpDir -ErrorAction SilentlyContinue
    }
}

# ---- Install Docker ----
function Install-Docker {
    $dockerCmd = Get-Command docker -ErrorAction SilentlyContinue
    if (-not $dockerCmd) {
        Write-Err "Docker not found. Install Docker Desktop: https://docs.docker.com/desktop/install/windows/"
    }

    Write-Info "Pulling torrentclaw/unarr:latest..."
    try {
        docker pull torrentclaw/unarr:latest 2>$null
    } catch {
        Write-Info "Image not on Docker Hub, building from source..."
        $gitCmd = Get-Command git -ErrorAction SilentlyContinue
        if (-not $gitCmd) {
            Write-Err "git not found. Install git or pull the image manually."
        }
        $tmpDir = Join-Path $env:TEMP "unarr-build-$(Get-Random)"
        git clone --depth 1 "https://github.com/$Repo.git" $tmpDir
        docker build -t torrentclaw/unarr:latest $tmpDir
        Remove-Item -Recurse -Force $tmpDir -ErrorAction SilentlyContinue
    }

    Write-Ok "Docker image ready: torrentclaw/unarr:latest"

    Write-Host ""
    Write-Host "Quick start:" -ForegroundColor White
    Write-Host ""
    Write-Host "  # 1. Create config directory"
    Write-Host "  mkdir `$env:APPDATA\unarr"
    Write-Host ""
    Write-Host "  # 2. Run setup (interactive)"
    Write-Host "  docker run -it --rm -v `$env:APPDATA\unarr:/config torrentclaw/unarr init"
    Write-Host ""
    Write-Host "  # 3. Start daemon"
    Write-Host "  docker run -d --name unarr --restart unless-stopped ``"
    Write-Host "    --read-only --memory 512m ``"
    Write-Host "    -v `$env:APPDATA\unarr:/config ``"
    Write-Host "    -v `$HOME\Media:/downloads ``"
    Write-Host "    torrentclaw/unarr"
    Write-Host ""
}

# ---- Uninstall ----
function Uninstall-Unarr {
    Write-Info "Uninstalling unarr..."

    # Remove binary
    $dir = Get-InstallDir
    $binPath = Join-Path $dir $Binary
    if (Test-Path $binPath) {
        Remove-Item $binPath -Force
        Write-Ok "Removed $binPath"
    }

    # Clean empty install dir
    if ((Test-Path $dir) -and -not (Get-ChildItem $dir)) {
        Remove-Item $dir -Force
    }

    # Remove Docker
    $dockerCmd = Get-Command docker -ErrorAction SilentlyContinue
    if ($dockerCmd) {
        docker rm -f unarr 2>$null | Out-Null
        docker rmi torrentclaw/unarr:latest 2>$null | Out-Null
        Write-Ok "Removed Docker container and image"
    }

    Write-Ok "Uninstalled. Config remains at $env:APPDATA\unarr\ (delete manually if desired)."
    exit
}

# ---- Interactive menu ----
function Show-Menu {
    Write-Host ""
    Write-Host "  unarr Installer" -ForegroundColor White
    Write-Host "  ────────────────────────"
    Write-Host ""
    Write-Host "  Detected: " -NoNewline
    Write-Host "windows/$(Get-Arch)" -ForegroundColor Cyan
    Write-Host ""
    Write-Host "  Install method:"
    Write-Host ""
    Write-Host "    1) " -NoNewline -ForegroundColor White
    Write-Host "Binary — standalone .exe, no dependencies"
    Write-Host "    2) " -NoNewline -ForegroundColor White
    Write-Host "Docker — sandboxed, isolated filesystem access " -NoNewline
    Write-Host "(recommended)" -ForegroundColor Green
    Write-Host "    u) " -NoNewline -ForegroundColor White
    Write-Host "Uninstall"
    Write-Host ""

    $choice = Read-Host "  Choice [1/2]"

    switch ($choice) {
        "1" { return "binary" }
        "2" { return "docker" }
        "u" { Uninstall-Unarr }
        "U" { Uninstall-Unarr }
        default { Write-Err "Invalid choice: $choice" }
    }
}

# ---- Main ----
function Main {
    # Resolve method
    $m = if ($Method)      { $Method }
         elseif ($env:METHOD) { $env:METHOD }
         else                 { Show-Menu }

    Write-Host ""

    switch ($m) {
        "binary" {
            Install-Binary
            Write-Host ""
            Write-Host "  Run " -NoNewline
            Write-Host "unarr init" -ForegroundColor White -NoNewline
            Write-Host " to get started."
            Write-Host ""
        }
        "docker" {
            Install-Docker
        }
        default {
            Write-Err "Unknown method: $m"
        }
    }
}

Main
