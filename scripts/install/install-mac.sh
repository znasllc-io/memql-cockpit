#!/usr/bin/env bash
#
# scripts/install/install-mac.sh
# ===============================
#
# Install memql-worker on macOS. Downloads the appropriate
# binary, drops a LaunchAgent at ~/Library/LaunchAgents/, and
# (optionally) generates a worker.yaml from a token supplied on the
# command line.
#
# Usage:
#   ./install-mac.sh --token mql_wkr_<...> --cluster https://app.copresent.ai
#
# Flags:
#   --token <token>     Worker token (mql_wkr_<...>). Required.
#   --cluster <url>     Cluster URL. Required.
#   --name <name>       Worker name (default: hostname).
#   --computeruse               Install the computer-use variant (memql-computeruse).
#   --download-base <u> Base URL for binary downloads.
#   --force             Overwrite existing worker.yaml.
#   --no-service        Skip LaunchAgent installation.

set -euo pipefail

# shellcheck source=lib.sh
source "$(dirname "$0")/lib.sh"

readonly DEFAULT_DOWNLOAD_BASE="https://github.com/znasllc-io/memql-cockpit/releases/latest/download"

function show_help() {
    cat << EOF
Usage: $(basename "$0") --token <token> --cluster <url> [options]

Required:
    --token <token>           Worker token (mql_wkr_<...>)
    --cluster <url>           Cluster URL (e.g. https://app.copresent.ai)

Options:
    --name <name>             Worker name (default: hostname)
    --computeruse                     Install the computer-use variant (memql-computeruse)
    --user-local              Install under \$HOME/.memql/bin instead of
                              /usr/local/bin. Use when sudo isn't
                              available (CI, restricted environments).
                              Provides weaker isolation -- a compromised
                              user account can swap the binary without
                              privilege escalation.
    --download-base <url>     Override binary download base URL
    --force                   Overwrite existing worker.yaml
    --no-service              Skip LaunchAgent installation
    --help                    Print this help
EOF
}

function parse_args() {
    TOKEN=""
    CLUSTER_URL=""
    NAME="$(hostname -s)"
    FLAVOUR="headless"
    DOWNLOAD_BASE="$DEFAULT_DOWNLOAD_BASE"
    FORCE="no"
    INSTALL_SERVICE="yes"
    INSTALL_MODE="system"  # default: sudo-gated /usr/local/bin (#66)

    while [[ $# -gt 0 ]]; do
        case "$1" in
            --token)         TOKEN="$2"; shift 2 ;;
            --cluster)       CLUSTER_URL="$2"; shift 2 ;;
            --name)          NAME="$2"; shift 2 ;;
            --computeruse)           FLAVOUR="computeruse"; shift ;;
            --user-local)    INSTALL_MODE="user-local"; shift ;;
            --download-base) DOWNLOAD_BASE="$2"; shift 2 ;;
            --force)         FORCE="yes"; shift ;;
            --no-service)    INSTALL_SERVICE="no"; shift ;;
            --help|-h)       show_help; exit 0 ;;
            *)
                echo "ERROR: unknown flag $1" >&2
                show_help
                exit 1
                ;;
        esac
    done

    if [[ -z "$TOKEN" || -z "$CLUSTER_URL" ]]; then
        echo "ERROR: --token and --cluster are required" >&2
        show_help
        exit 1
    fi
    if [[ ! "$TOKEN" =~ ^mql_wkr_ ]]; then
        echo "ERROR: token must start with mql_wkr_" >&2
        exit 1
    fi
}

function install_binary() {
    local binary
    binary="$(binary_name_for "$FLAVOUR")"
    local url="${DOWNLOAD_BASE}/${binary}"
    # ONE installed command for BOTH variants (design D4). The DOWNLOAD
    # artifact carries the variant (memql-computeruse-<os>-<arch>); the
    # installed file never does. Installing the computer-use build as
    # `memql-computeruse` would reinstate the second command name D4
    # retires -- and it is the name the docs, the service unit's
    # ExecStart, and `memql --version`'s variant line all assume.
    install_binary_with_mode "$INSTALL_MODE" "$url" "$binary" "$INSTALLED_COMMAND"
    INSTALLED_BINARY="$INSTALL_BINARY_FRIENDLY"
}

function write_config() {
    local path="${HOME}/.memql/worker.yaml"
    # The computer-use variant additionally advertises COMPUTERUSE.
    # macOS has no Wayland gate, so the flavour alone decides the set.
    local capabilities="HEADLESS"
    if [[ "$FLAVOUR" == "computeruse" ]]; then
        capabilities="HEADLESS,COMPUTERUSE"
    fi
    write_worker_yaml "$path" "$CLUSTER_URL" "$TOKEN" "$NAME" "$FORCE" "$capabilities"
}

function install_launch_agent() {
    if [[ "$INSTALL_SERVICE" != "yes" ]]; then
        echo "INFO: --no-service set; skipping LaunchAgent installation"
        return 0
    fi
    local plist_dir="${HOME}/Library/LaunchAgents"
    local plist="${plist_dir}/com.znasllc.memql-worker.plist"
    mkdir -p "$plist_dir"
    # Migrate: retire the pre-rename LaunchAgent so an upgraded machine
    # never runs two workers (znasllc-io/memql#4553).
    local legacy="${plist_dir}/com.znasllc.memql-cockpit-worker.plist"
    if [[ -f "$legacy" ]]; then
        launchctl unload "$legacy" >/dev/null 2>&1 || true
        rm -f "$legacy"
        echo "INFO: removed legacy com.znasllc.memql-cockpit-worker LaunchAgent"
    fi
    cat > "$plist" << PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>com.znasllc.memql-worker</string>
    <key>ProgramArguments</key>
    <array>
        <string>${INSTALLED_BINARY}</string>
        <string>worker</string>
        <string>run</string>
    </array>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
    <key>StandardOutPath</key>
    <string>${HOME}/.memql/state/worker.log</string>
    <key>StandardErrorPath</key>
    <string>${HOME}/.memql/state/worker.log</string>
    <key>EnvironmentVariables</key>
    <dict>
        <key>HOME</key>
        <string>${HOME}</string>
    </dict>
</dict>
</plist>
PLIST
    chmod 644 "$plist"
    echo "INFO: wrote $plist"
    launchctl unload "$plist" >/dev/null 2>&1 || true
    launchctl load "$plist"
    echo "INFO: launched memql-worker LaunchAgent"
}

function main() {
    parse_args "$@"
    install_binary
    write_config
    install_launch_agent

    cat << EOF

================================================================
SUCCESS: memql-worker installed.

Binary:    ${INSTALLED_BINARY}
Config:    ${HOME}/.memql/worker.yaml
Logs:      ${HOME}/.memql/state/worker.log

The worker is running as a LaunchAgent and will reconnect on
boot. To check the status:

  launchctl list | grep memql-worker

To stop it:

  launchctl unload ~/Library/LaunchAgents/com.znasllc.memql-worker.plist
================================================================
EOF
}

main "$@"
