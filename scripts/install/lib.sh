#!/usr/bin/env bash
#
# scripts/install/lib.sh
# ======================
#
# Shared function library for the worker install scripts --
# function-based helpers sourced by the OS-specific drivers
# install-mac.sh and install-linux.sh. write_worker_yaml (below) is
# the single renderer for ~/.memql/worker.yaml, so the config layout
# can never drift between the two platforms. The pure-logic helpers
# are exercised by lib_test.sh, wired into CI via
# .github/workflows/install-scripts-lint.yml.

set -uo pipefail

# Install modes:
#   system     -- /usr/local/bin (immutable; root-owned; sudo-gated).
#                 Default. A compromised user account can no longer
#                 swap the cockpit binary without privilege escalation
#                 (Wave 6 #45 hardening; #66 follow-up).
#   user-local -- ${HOME}/.memql/bin (user-writable). Opt-in via
#                 --user-local. Weaker isolation; kept for CI /
#                 restricted environments where sudo isn't available.
readonly INSTALL_PREFIX_SYSTEM="/usr/local/bin"
readonly INSTALL_PREFIX_USER="${HOME}/.memql/bin"

readonly STATE_DIR_DEFAULT="${HOME}/.memql/state"

# The installed command. ONE name for both build variants (design D4,
# znasllc-io/memql-cockpit#352): the download artifact carries the variant,
# the installed file never does. `memql --version` is what answers "which
# build is this", which only works because the path cannot.
readonly INSTALLED_COMMAND="memql"

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
# headless / computeruse flavour and the detected platform.
function binary_name_for() {
    local flavour="$1"  # "headless" or "computeruse"
    local os arch
    os="$(detect_os)"
    arch="$(detect_arch)"
    case "$flavour" in
        headless)
            echo "memql-${os}-${arch}"
            ;;
        computeruse)
            echo "memql-computeruse-${os}-${arch}"
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

# install_mode_dir prints the install destination directory for
# the given mode. Used by install_binary_with_mode + by the OS-
# specific driver scripts when they build the LaunchAgent / systemd
# unit ExecStart path.
function install_mode_dir() {
    local mode="$1"
    case "$mode" in
        system)
            echo "$INSTALL_PREFIX_SYSTEM"
            ;;
        user-local)
            echo "$INSTALL_PREFIX_USER"
            ;;
        *)
            echo "ERROR: unknown install mode '$mode' (expected: system, user-local)" >&2
            return 1
            ;;
    esac
}

# require_sudo ensures we can call `sudo` non-interactively, or
# prompts the user. Returns 0 on success, 1 if sudo isn't available
# or auth fails. Already-root processes succeed immediately. Used by
# install_binary_with_mode for the system install path so a
# /usr/local/bin write attempt fails fast with a clear message
# instead of failing in the middle of the download.
function require_sudo() {
    if [[ $EUID -eq 0 ]]; then
        return 0
    fi
    if ! command -v sudo >/dev/null 2>&1; then
        echo "ERROR: sudo required for the immutable install at $INSTALL_PREFIX_SYSTEM but is not available." >&2
        echo "       Pass --user-local to install under $INSTALL_PREFIX_USER instead." >&2
        return 1
    fi
    if sudo -n true 2>/dev/null; then
        return 0
    fi
    echo "INFO: $INSTALL_PREFIX_SYSTEM install requires sudo; you'll be prompted for your password..."
    if ! sudo -v; then
        echo "ERROR: sudo authentication failed. Pass --user-local for a passwordless install." >&2
        return 1
    fi
    return 0
}

# install_binary_with_mode downloads $url into the mode-appropriate
# directory under the binary name $binary, makes it executable, and
# symlinks $friendly_name to the downloaded file. Sets two globals
# the driver scripts read after the call:
#
#   INSTALL_BINARY_DEST     -- full path to the downloaded binary
#   INSTALL_BINARY_FRIENDLY -- full path to the friendly symlink
#                              (memql / memql-computeruse).
#
# In system mode the install path is created via `sudo install
# -m 0755 ...` so the destination is root-owned and the worker
# user can read+exec but not write -- closes the
# swap-without-privesc gap from Wave 6 #45.
function install_binary_with_mode() {
    local mode="$1"
    local url="$2"
    local binary="$3"
    local friendly_name="$4"

    local dest_dir
    dest_dir="$(install_mode_dir "$mode")"
    INSTALL_BINARY_DEST="$dest_dir/$binary"
    INSTALL_BINARY_FRIENDLY="$dest_dir/$friendly_name"

    case "$mode" in
        system)
            require_sudo
            local tmp
            tmp="$(mktemp)"
            if ! download_binary "$url" "$tmp"; then
                rm -f "$tmp"
                return 1
            fi
            sudo mkdir -p "$dest_dir"
            sudo install -m 0755 "$tmp" "$INSTALL_BINARY_DEST"
            rm -f "$tmp"
            sudo ln -sf "$INSTALL_BINARY_DEST" "$INSTALL_BINARY_FRIENDLY"
            ;;
        user-local)
            mkdir -p "$dest_dir"
            if ! download_binary "$url" "$INSTALL_BINARY_DEST"; then
                return 1
            fi
            ln -sf "$INSTALL_BINARY_DEST" "$INSTALL_BINARY_FRIENDLY"
            echo "WARN: --user-local install at $dest_dir provides weaker isolation than"
            echo "      $INSTALL_PREFIX_SYSTEM. A compromised user account can swap the"
            echo "      binary without privilege escalation. Use the default install path"
            echo "      on shared / production machines."
            ;;
        *)
            echo "ERROR: unknown install mode '$mode'" >&2
            return 1
            ;;
    esac

    # Migrate: drop the pre-rename binaries/symlinks beside the new one
    # (znasllc-io/memql#4553). Harmless when absent.
    case "$mode" in
        system)
            sudo rm -f "$dest_dir/memql-cockpit" "$dest_dir/memql-cockpit-computeruse" 2>/dev/null || true
            ;;
        *)
            rm -f "$dest_dir/memql-cockpit" "$dest_dir/memql-cockpit-computeruse" 2>/dev/null || true
            ;;
    esac
}

# write_worker_yaml renders ~/.memql/worker.yaml from the supplied
# args. It is the SINGLE source of truth for the worker.yaml layout:
# both install-mac.sh and install-linux.sh call it, so the config can
# never drift between platforms. Refuses to clobber an existing file
# unless force == "yes" (the installers' --force flag).
#
# Args:
#   $1 path          -- destination file (usually ~/.memql/worker.yaml)
#   $2 cluster_url   -- cluster edge URL
#   $3 token         -- worker token (mql_wkr_...)
#   $4 name          -- worker name
#   $5 force         -- "yes" to overwrite an existing file (default "no")
#   $6 capabilities  -- comma-separated capability list to advertise
#                       (default "HEADLESS"; pass "HEADLESS,COMPUTERUSE"
#                       for the computer-use variant). The Wayland
#                       downgrade decision stays in install-linux.sh --
#                       this helper only renders what it is handed.
#
# The concurrency block (HEADLESS: 8, COMPUTERUSE: 1) is fixed and
# platform-agnostic; os/arch labels come from detect_os/detect_arch.
function write_worker_yaml() {
    local path="$1"
    local cluster_url="$2"
    local token="$3"
    local name="$4"
    local force="${5:-no}"
    local capabilities="${6:-HEADLESS}"
    if [[ -e "$path" && "$force" != "yes" ]]; then
        echo "ERROR: $path already exists; pass --force to overwrite" >&2
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
  COMPUTERUSE: 1
state_dir: ${STATE_DIR_DEFAULT}
log_level: info
capabilities:
$(echo "$capabilities" | tr ',' '\n' | sed 's/^/  - /')
YAML
    chmod 600 "$path"
    echo "INFO: wrote $path (capabilities: ${capabilities})"
}
