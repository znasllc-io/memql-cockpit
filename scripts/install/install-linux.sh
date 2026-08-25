#!/usr/bin/env bash
#
# scripts/install/install-linux.sh
# =================================
#
# Install memql-worker on Linux. Downloads the appropriate
# binary, drops a user-systemd service, and (optionally) generates
# a worker.yaml from a token supplied on the command line.

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
    --name <name>             Worker name (default: hostname -s)
    --computeruse                     Install the computer-use variant. Wayland only
                              registers HEADLESS; X11 enables the COMPUTERUSE capability.
    --user-local              Install under \$HOME/.memql/bin instead of
                              /usr/local/bin. Use when sudo isn't
                              available (CI, restricted environments).
                              Provides weaker isolation -- a compromised
                              user account can swap the binary without
                              privilege escalation.
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
    # The computer-use variant advertises COMPUTERUSE, but only on X11:
    # RobotGo can't drive Wayland, so a Wayland session downgrades to
    # HEADLESS-only. This platform-specific decision stays here; the
    # shared renderer (write_worker_yaml) only emits what it is handed
    # and honors --force.
    local capabilities="HEADLESS"
    if [[ "$FLAVOUR" == "computeruse" ]]; then
        if [[ -n "${WAYLAND_DISPLAY:-}" && -z "${DISPLAY:-}" ]]; then
            echo "INFO: Wayland detected; registering HEADLESS only (X11 required for COMPUTERUSE)"
        else
            capabilities="HEADLESS,COMPUTERUSE"
        fi
    fi
    write_worker_yaml "$path" "$CLUSTER_URL" "$TOKEN" "$NAME" "$FORCE" "$capabilities"
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
    # Migrate: retire the pre-rename unit so an upgraded machine never
    # runs two workers (znasllc-io/memql#4553).
    if [[ -f "$unit_dir/memql-cockpit-worker.service" ]]; then
        systemctl --user disable --now memql-cockpit-worker.service >/dev/null 2>&1 || true
        rm -f "$unit_dir/memql-cockpit-worker.service"
        systemctl --user daemon-reload >/dev/null 2>&1 || true
        echo "INFO: removed legacy memql-cockpit-worker.service unit"
    fi
    local unit="${unit_dir}/memql-worker.service"
    local env_file="${HOME}/.memql/worker.env"
    # Create the env file as 0600 even when empty so an operator
    # who later wants to inject MEMQL_WORKER_TOKEN via the env-file
    # path drops it into a file that already has owner-only mode.
    # The unit references EnvironmentFile= with a leading `-` so
    # systemd ignores the file when it's missing.
    if [[ ! -f "$env_file" ]]; then
        umask 077 && touch "$env_file"
    fi
    chmod 0600 "$env_file" 2>/dev/null || true
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

# Environment file for the worker token + cluster URL. Marked with
# leading '-' so systemd doesn't fail if the file is missing; the
# worker also reads MEMQL_WORKER_TOKEN from ~/.memql/worker.yaml.
# Use this file (chmod 0600) instead of inline Environment= so
# the token is NOT visible in 'systemctl show' output.
EnvironmentFile=-${env_file}

# Hardening directives. The headless worker doesn't need write
# access to the system tree or other users' home dirs; tighten
# accordingly. The computer-use build (build tag computeruse) needs a
# more permissive ProtectHome to drive the user's desktop, but
# the headless install path here is the default.
ProtectSystem=strict
ProtectHome=read-only
ReadWritePaths=${HOME}/.memql
NoNewPrivileges=yes
PrivateTmp=yes
RestrictSUIDSGID=yes
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectControlGroups=yes
LockPersonality=yes
MemoryDenyWriteExecute=yes
RestrictRealtime=yes
RestrictNamespaces=yes
SystemCallArchitectures=native

[Install]
WantedBy=default.target
UNIT
    echo "INFO: wrote $unit"
    systemctl --user daemon-reload
    systemctl --user enable memql-worker.service
    systemctl --user restart memql-worker.service
    echo "INFO: enabled and started memql-worker.service"
}

function main() {
    parse_args "$@"
    mkdir -p "${HOME}/.memql/state"
    install_binary
    write_config
    install_systemd_unit

    cat << EOF

================================================================
SUCCESS: memql-worker installed.

Binary:    ${INSTALLED_BINARY}
Config:    ${HOME}/.memql/worker.yaml
Logs:      ${HOME}/.memql/state/worker.log

To check the status:

  systemctl --user status memql-worker.service

To stop it:

  systemctl --user stop memql-worker.service
================================================================
EOF
}

main "$@"
