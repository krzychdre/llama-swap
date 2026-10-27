#!/bin/sh
# This script installs llama-swap on Linux.
# It detects the current operating system architecture and installs the appropriate version of llama-swap.

set -eu

LLAMA_SWAP_DEFAULT_ADDRESS=${LLAMA_SWAP_DEFAULT_ADDRESS:-"127.0.0.1:8080"}

red="$( (/usr/bin/tput bold || :; /usr/bin/tput setaf 1 || :) 2>&-)"
plain="$( (/usr/bin/tput sgr0 || :) 2>&-)"

status() { echo ">>> $*" >&2; }
error() { echo "${red}ERROR:${plain} $*"; exit 1; }
warning() { echo "${red}WARNING:${plain} $*"; }

available() { command -v "$1" >/dev/null; }
require() {
    _MISSING=''
    for TOOL in "$@"; do
        if ! available "$TOOL"; then
            _MISSING="$_MISSING $TOOL"
        fi
    done

    echo "$_MISSING"
}

# accepts host:port and :port (every interface); rejects anything that could
# add a line to the unit file
validate_address() {
    _HOST=${1%:*}
    _PORT=${1##*:}
    case "$_HOST" in *[!A-Za-z0-9.:\[\]-]*) return 1 ;; esac
    case "$_PORT" in '' | *[!0-9]*) return 1 ;; esac
    [ "$_PORT" -ge 1 ] && [ "$_PORT" -le 65535 ]
}
validate_address "$LLAMA_SWAP_DEFAULT_ADDRESS" ||
    error "LLAMA_SWAP_DEFAULT_ADDRESS must be host:port, got '$LLAMA_SWAP_DEFAULT_ADDRESS'"

if available sha256sum; then
    sha256() { sha256sum "$1" | cut -d' ' -f1; }
elif available shasum; then
    sha256() { shasum -a 256 "$1" | cut -d' ' -f1; }
else
    error "This script requires sha256sum or shasum."
fi

TMPDIR=
cleanup() {
    if [ -n "$TMPDIR" ]; then
        rm -rf "$TMPDIR"
    fi
}
trap cleanup EXIT

SUDO=
if [ "$(id -u)" -ne 0 ]; then
    if ! available sudo; then
        error "This script requires superuser permissions. Please re-run as root."
    fi

    SUDO="sudo"
fi

NEEDS=$(require tee tar python3 mktemp install)
if [ -n "$NEEDS" ]; then
    status "ERROR: The following tools are required but missing:"
    for NEED in $NEEDS; do
        echo "  - $NEED"
    done
    exit 1
fi

[ "$(uname -s)" = "Linux" ] || error 'This script is intended to run on Linux only.'

ARCH=$(uname -m)
case "$ARCH" in
    x86_64) ARCH="amd64" ;;
    aarch64|arm64) ARCH="arm64" ;;
    *) error "Unsupported architecture: $ARCH" ;;
esac

IS_WSL2=false

KERN=$(uname -r)
case "$KERN" in
    *icrosoft*WSL2 | *icrosoft*wsl2) IS_WSL2=true;;
    *icrosoft) error "Microsoft WSL1 is not currently supported. Please use WSL2 with 'wsl --set-version <distro> 2'" ;;
    *) ;;
esac

download_binary() {
    TMPDIR=$(mktemp -d)
    PYTHON_SCRIPT=$(cat <<EOF
import os
import json
import sys
import urllib.request

ARCH = "${ARCH}"
TMPDIR = "${TMPDIR}"

with urllib.request.urlopen("https://api.github.com/repos/mostlygeek/llama-swap/releases/latest") as resp:
    data = json.load(resp)

version = data["tag_name"].lstrip("v")
tarball = "llama-swap_%s_linux_%s.tar.gz" % (version, ARCH)
checksums = "llama-swap_%s_checksums.txt" % version
urls = {asset["name"]: asset["browser_download_url"] for asset in data.get("assets", [])}

for name in (tarball, checksums):
    if name not in urls:
        print("ERROR: %s not found in the latest release." % name, file=sys.stderr)
        exit(1)
    print("Downloading:", urls[name], file=sys.stderr)
    urllib.request.urlretrieve(urls[name], os.path.join(TMPDIR, name))

print(tarball)
EOF
)

    ASSET_NAME=$(python3 -c "$PYTHON_SCRIPT")
    TARFILE="$TMPDIR/$ASSET_NAME"
    if [ ! -f "$TARFILE" ]; then
        error "Failed to download binary."
    fi

    EXPECTED=$(grep " $ASSET_NAME\$" "$TMPDIR"/*_checksums.txt | cut -d' ' -f1) || true
    ACTUAL=$(sha256 "$TARFILE")
    if [ -z "$EXPECTED" ] || [ "$EXPECTED" != "$ACTUAL" ]; then
        error "Checksum mismatch for $ASSET_NAME (expected '$EXPECTED', got '$ACTUAL')."
    fi

    status "Installing /usr/local/bin/llama-swap"
    tar -xzf "$TARFILE" -C "$TMPDIR" llama-swap
    $SUDO install -m 0755 "$TMPDIR/llama-swap" /usr/local/bin/llama-swap
}
download_binary

configure_systemd() {
    if ! id llama-swap >/dev/null 2>&1; then
        status "Creating llama-swap user..."
        $SUDO useradd -r -s /bin/false -U -m -d /usr/share/llama-swap llama-swap
    fi
    if getent group render >/dev/null 2>&1; then
        status "Adding llama-swap user to render group..."
        $SUDO usermod -a -G render llama-swap
    fi
    if getent group video >/dev/null 2>&1; then
        status "Adding llama-swap user to video group..."
        $SUDO usermod -a -G video llama-swap
    fi
    if getent group docker >/dev/null 2>&1; then
        status "Adding llama-swap user to docker group..."
        $SUDO usermod -a -G docker llama-swap
    fi

    status "Adding current user to llama-swap group..."
    $SUDO usermod -a -G llama-swap "$(whoami)"

    if [ ! -f "/usr/share/llama-swap/config.yaml" ]; then
        status "Creating default config.yaml..."
        cat <<EOF | $SUDO -u llama-swap tee /usr/share/llama-swap/config.yaml >/dev/null
# default 15s likely to fail for default models due to downloading models
healthCheckTimeout: 60

models:
  "qwen2.5":
    cmd: |
      docker run
        --rm
        -p \${PORT}:8080
        --name qwen2.5
      ghcr.io/ggml-org/llama.cpp:server
        -hf bartowski/Qwen2.5-0.5B-Instruct-GGUF:Q4_K_M
    cmdStop: docker stop qwen2.5

  "smollm2":
    cmd: |
      docker run
        --rm
        -p \${PORT}:8080
        --name smollm2
      ghcr.io/ggml-org/llama.cpp:server
        -hf bartowski/SmolLM2-135M-Instruct-GGUF:Q4_K_M
    cmdStop: docker stop smollm2
EOF
    fi

    status "Creating llama-swap systemd service..."
    cat <<EOF | $SUDO tee /etc/systemd/system/llama-swap.service >/dev/null
[Unit]
Description=llama-swap
After=network.target

[Service]
User=llama-swap
Group=llama-swap

# set this to match your environment
ExecStart=/usr/local/bin/llama-swap --config /usr/share/llama-swap/config.yaml --watch-config -listen ${LLAMA_SWAP_DEFAULT_ADDRESS}

Restart=on-failure
RestartSec=3
StartLimitBurst=3
StartLimitInterval=30

[Install]
WantedBy=multi-user.target
EOF
    SYSTEMCTL_RUNNING="$(systemctl is-system-running || true)"
    case $SYSTEMCTL_RUNNING in
        running|degraded)
            status "Enabling and starting llama-swap service..."
            $SUDO systemctl daemon-reload
            $SUDO systemctl enable llama-swap

            start_service() { $SUDO systemctl restart llama-swap; }
            trap 'start_service; cleanup' EXIT
            ;;
        *)
            warning "systemd is not running"
            if [ "$IS_WSL2" = true ]; then
                warning "see https://learn.microsoft.com/en-us/windows/wsl/systemd#how-to-enable-systemd to enable it"
            fi
            ;;
    esac
}

if available systemctl; then
    configure_systemd
fi

install_success() {
    status "The llama-swap API is now available at http://${LLAMA_SWAP_DEFAULT_ADDRESS}"
    status 'Customize the config file at /usr/share/llama-swap/config.yaml.'
    status 'Install complete.'
}

# WSL2 only supports GPUs via nvidia passthrough
# so check for nvidia-smi to determine if GPU is available
if [ "$IS_WSL2" = true ]; then
    if available nvidia-smi && [ -n "$(nvidia-smi | grep -o "CUDA Version: [0-9]*\.[0-9]*")" ]; then
        status "Nvidia GPU detected."
    fi
    exit 0
fi

install_success
