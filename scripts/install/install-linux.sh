#!/usr/bin/env bash
#
# scripts/install/install-linux.sh
# =================================
#
# Install memql-cockpit-worker on Linux. Downloads the appropriate
# binary, drops a user-systemd service, and (optionally) generates
# a worker.yaml from a token supplied on the command line.

set -euo pipefail

# shellcheck source=lib.sh
source "$(dirname "$0")/lib.sh"

readonly DEFAULT_DOWNLOAD_BASE="https://app.copresent.ai/admin/workers/install"

function show_help() {
    cat << EOF
Usage: $(basename "$0") --token <token> --cluster <url> [options]

Required:
    --token <token>           Worker token (mql_wkr_<...>)
    --cluster <url>           Cluster URL (e.g. https://app.copresent.ai)

Options:
    --name <name>             Worker name (default: hostname -s)
    --gui                     Install the GUI variant. Wayland only
                              registers HEADLESS; X11 enables GUI.
    --download-base <url>     Override binary download base URL
    --force                   Overwrite existing worker.yaml
    --no-service              Skip systemd unit installation
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

    while [[ $# -gt 0 ]]; do
        case "$1" in
            --token)         TOKEN="$2"; shift 2 ;;
            --cluster)       CLUSTER_URL="$2"; shift 2 ;;
            --name)          NAME="$2"; shift 2 ;;
            --gui)           FLAVOUR="gui"; shift ;;
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
    local dest_dir="${HOME}/.memql/bin"
    mkdir -p "$dest_dir"
    local dest="${dest_dir}/${binary}"
    download_binary "$url" "$dest"

    local friendly
    if [[ "$FLAVOUR" == "gui" ]]; then
        friendly="${dest_dir}/memql-cockpit-gui"
    else
        friendly="${dest_dir}/memql-cockpit"
    fi
    ln -sf "$dest" "$friendly"
    INSTALLED_BINARY="$friendly"
}

function write_config() {
    local path="${HOME}/.memql/worker.yaml"
    local capabilities="HEADLESS"
    if [[ "$FLAVOUR" == "gui" ]]; then
        if [[ -n "${WAYLAND_DISPLAY:-}" && -z "${DISPLAY:-}" ]]; then
            echo "INFO: Wayland detected; registering HEADLESS only (X11 required for GUI)"
        else
            capabilities="HEADLESS,GUI"
        fi
    fi

    cat > "$path" << YAML
cluster_url: ${CLUSTER_URL}
token: ${TOKEN}
name: ${NAME}
labels:
  os: linux
  arch: $(detect_arch)
concurrency:
  HEADLESS: 8
  GUI: 1
state_dir: ${HOME}/.memql/state
log_level: info
capabilities:
$(echo "$capabilities" | tr ',' '\n' | sed 's/^/  - /')
YAML
    chmod 600 "$path"
    echo "INFO: wrote $path (capabilities: $capabilities)"
}

function install_systemd_unit() {
    if [[ "$INSTALL_SERVICE" != "yes" ]]; then
        echo "INFO: --no-service set; skipping systemd installation"
        return 0
    fi
    if ! command -v systemctl >/dev/null 2>&1; then
        echo "WARN: systemctl not found; skipping service installation"
        return 0
    fi
    local unit_dir="${HOME}/.config/systemd/user"
    mkdir -p "$unit_dir"
    local unit="${unit_dir}/memql-cockpit-worker.service"
    cat > "$unit" << UNIT
[Unit]
Description=memQL Cockpit Worker
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=${INSTALLED_BINARY} worker run
Restart=on-failure
RestartSec=5
StandardOutput=append:${HOME}/.memql/state/worker.log
StandardError=append:${HOME}/.memql/state/worker.log

[Install]
WantedBy=default.target
UNIT
    echo "INFO: wrote $unit"
    systemctl --user daemon-reload
    systemctl --user enable memql-cockpit-worker.service
    systemctl --user restart memql-cockpit-worker.service
    echo "INFO: enabled and started memql-cockpit-worker.service"
}

function main() {
    parse_args "$@"
    mkdir -p "${HOME}/.memql/state"
    install_binary
    write_config
    install_systemd_unit

    cat << EOF

================================================================
SUCCESS: memql-cockpit-worker installed.

Binary:    ${INSTALLED_BINARY}
Config:    ${HOME}/.memql/worker.yaml
Logs:      ${HOME}/.memql/state/worker.log

To check the status:

  systemctl --user status memql-cockpit-worker.service

To stop it:

  systemctl --user stop memql-cockpit-worker.service
================================================================
EOF
}

main "$@"
