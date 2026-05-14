#!/usr/bin/env bash
#
# scripts/install/lib.sh
# ======================
#
# Shared function library for the worker install scripts. Mirrors
# scripts/dev/lib.sh -- function-based, sourced by the OS-specific
# drivers under scripts/install/install-{mac,linux}.sh.

set -uo pipefail

readonly INSTALL_PREFIX_DEFAULT="/usr/local/bin"
readonly CONFIG_DIR_DEFAULT="${HOME}/.memql"
readonly STATE_DIR_DEFAULT="${HOME}/.memql/state"

# Detect host os ("darwin" or "linux") and arch ("amd64" or "arm64").
function detect_os() {
    local raw
    raw="$(uname -s)"
    case "$raw" in
        Darwin) echo "darwin" ;;
        Linux)  echo "linux" ;;
        *)
            echo "ERROR: unsupported os $raw" >&2
            return 1
            ;;
    esac
}

function detect_arch() {
    local raw
    raw="$(uname -m)"
    case "$raw" in
        x86_64|amd64) echo "amd64" ;;
        arm64|aarch64) echo "arm64" ;;
        *)
            echo "ERROR: unsupported arch $raw" >&2
            return 1
            ;;
    esac
}

# Pick the appropriate cockpit binary name for the requested
# headless / gui flavour and the detected platform.
function binary_name_for() {
    local flavour="$1"  # "headless" or "gui"
    local os arch
    os="$(detect_os)"
    arch="$(detect_arch)"
    case "$flavour" in
        headless)
            echo "memql-cockpit-${os}-${arch}"
            ;;
        gui)
            echo "memql-cockpit-gui-${os}-${arch}"
            ;;
        *)
            echo "ERROR: unknown flavour $flavour" >&2
            return 1
            ;;
    esac
}

# Download the supplied URL to the supplied path, with a basic
# integrity check (HTTP 200, non-empty file).
function download_binary() {
    local url="$1"
    local dest="$2"
    if ! command -v curl >/dev/null 2>&1; then
        echo "ERROR: curl required" >&2
        return 1
    fi
    echo "INFO: downloading $url"
    if ! curl -fsSL --proto '=https' "$url" -o "$dest.partial"; then
        echo "ERROR: download failed" >&2
        rm -f "$dest.partial"
        return 1
    fi
    if [[ ! -s "$dest.partial" ]]; then
        echo "ERROR: downloaded file is empty" >&2
        rm -f "$dest.partial"
        return 1
    fi
    mv "$dest.partial" "$dest"
    chmod +x "$dest"
}

# Render ~/.memql/worker.yaml from supplied args. Refuses to clobber
# an existing file unless --force.
function write_worker_yaml() {
    local path="$1"
    local cluster_url="$2"
    local token="$3"
    local name="$4"
    local force="${5:-no}"
    if [[ -e "$path" && "$force" != "yes" ]]; then
        echo "ERROR: $path already exists; pass --force to overwrite"
        return 1
    fi
    mkdir -p "$(dirname "$path")"
    cat > "$path" << YAML
cluster_url: ${cluster_url}
token: ${token}
name: ${name}
labels:
  os: $(detect_os)
  arch: $(detect_arch)
concurrency:
  HEADLESS: 8
  GUI: 1
state_dir: ${STATE_DIR_DEFAULT}
log_level: info
capabilities:
  - HEADLESS
YAML
    chmod 600 "$path"
}
