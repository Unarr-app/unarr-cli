#!/usr/bin/env bash
# Boot the real-Windows test VM, deploy freshly-built unarr binaries into it, and
# (optionally) run automated smoke checks. See docker-compose.yml for what this is.
#
# The point of this harness: cross-compilation proves the code BUILDS for Windows.
# It does NOT prove the console-window suppression works or that the scheduled
# task registers. Those are Windows-runtime behaviours — verify them HERE.
set -euo pipefail
cd "$(dirname "$0")"

SMOKE=0
[[ "${1:-}" == "--smoke" ]] && SMOKE=1

REPO_ROOT="$(cd ../.. && pwd)"
SHARED="./shared"
mkdir -p "$SHARED"

echo "==> Building Windows binaries (amd64)…"
( cd "$REPO_ROOT" && \
  GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -o "test/windows/shared/unarr.exe" ./cmd/unarr )
# The tray needs -H=windowsgui (GUI subsystem, no console) — mirror release/desktop.yml.
( cd "$REPO_ROOT" && \
  GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -ldflags "-H=windowsgui" \
    -o "test/windows/shared/unarr-desktop.exe" ./cmd/unarr-desktop )
echo "    unarr.exe + unarr-desktop.exe (-H=windowsgui) → $SHARED/"

# Drop the manual checklist + smoke script where the guest can reach them.
cp checklist.md "$SHARED/CHECKLIST.md"
cp smoke.ps1    "$SHARED/smoke.ps1"

echo "==> Booting Windows VM (first boot installs Windows unattended, ~10-20 min)…"
docker compose up -d

echo "==> Watch the desktop:  http://localhost:8006   (or RDP localhost:3389, user tester / unarrtest)"
echo "    The built binaries + CHECKLIST.md + smoke.ps1 are on the guest at \\\\host.lan\\Data"
echo
echo "==> On a FRESH install (down -v first), oem/install.bat runs smoke.ps1"
echo "    automatically at the end of setup and writes ./shared/smoke-result.txt."
echo "    Wait for that file to appear, then:  cat shared/smoke-result.txt"
echo

if [[ "$SMOKE" == "1" ]]; then
  echo "==> Waiting for WinRM (guest must finish install + enable WinRM first)…"
  # dockurr/windows enables WinRM once the unattended install completes.
  for i in $(seq 1 120); do
    if curl -s -m 3 -o /dev/null "http://localhost:5985/wsman"; then
      echo "    WinRM up."
      break
    fi
    sleep 15
    [[ $i == 120 ]] && { echo "    WinRM never came up — run the manual CHECKLIST.md via noVNC."; exit 1; }
  done
  echo "==> Running smoke.ps1 over WinRM (see smoke.ps1 for the assertions)…"
  # pypsrp / evil-winrm optional; simplest portable path is the guest running it
  # on first logon. We just tell the operator; automated WinRM push is a TODO the
  # manual checklist covers meanwhile.
  echo "    (Automated WinRM push not yet wired — open the VM and run 'powershell \\\\host.lan\\Data\\smoke.ps1'.)"
fi

echo "==> Done. Tear down with:  docker compose down    (keeps the disk volume)"
echo "    Full reset:            docker compose down -v (deletes Windows install)"
