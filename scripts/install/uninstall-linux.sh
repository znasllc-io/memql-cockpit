#!/usr/bin/env bash
#
# scripts/install/uninstall-linux.sh
# ===================================
#
# Remove memql-worker from a Linux machine: install-linux.sh run
# backwards, in one line, because an install that is one line and an
# uninstall that is a runbook leaves the token on machines nobody
# finishes cleaning up. Stops and removes the user-systemd units -- the
# worker's and, when `memql worker setup --inference` wrote one, the
# model runtime's (memql-ollama.service) -- removes the binary and its
# symlink, removes workers.yaml and worker.yaml (the tokens) and
# worker.env; --purge takes
# policy.yaml, the state dir, the native model runtime and its models
# under ~/.memql/ollama, and an emptied ~/.memql as well. Every step says
# what it did, or that there was nothing to do, and nothing here prompts
# except sudo.
#
# Usage:
#   ./uninstall-linux.sh [--purge] [--user-local]
#
# What this never touches: anything outside ~/.memql, the three systemd
# unit files, and the binary paths under the chosen prefix. The
# machine's registration on the cluster is revoked from MemQL OS
# (Fleet -> Machines), not from here -- by the time this script could
# ask, the token that would have spoken for the machine is gone.

set -euo pipefail

# Where lib.sh can be fetched from when this script travels alone.
# Piped execution (`curl ... | bash`) makes $0 `bash`, so the sibling
# source below would look for lib.sh in the operator's cwd and die.
# Pinned to main -- the same raw base the one-liner serves this
# script from -- and overridable so lib_test.sh can point the fetch
# at a local file:// fixture and stay offline.
readonly RAW_BASE="${MEMQL_INSTALL_RAW_BASE:-https://raw.githubusercontent.com/znasllc-io/memql-cockpit/main/scripts/install}"

# Source the shared helper library. A sibling lib.sh (the cloned-repo
# case) always wins, so a checkout never gains a network dependency.
# Only the piped case fetches -- into a temp file, checked non-empty
# BEFORE sourcing, because under `set -e` a bare `source <(curl ...)`
# turns a failed fetch into a cryptic bash error naming no URL.
function source_lib() {
    local sibling
    sibling="$(dirname "$0")/lib.sh"
    if [[ -f "$sibling" ]]; then
        # shellcheck source=lib.sh
        source "$sibling"
        return 0
    fi
    local url="${RAW_BASE}/lib.sh"
    local tmp
    tmp="$(mktemp)"
    if ! curl -fsSL "$url" -o "$tmp" || [[ ! -s "$tmp" ]]; then
        rm -f "$tmp"
        echo "ERROR: failed to fetch $url" >&2
        echo "       Piped execution needs it; check network access or run from a repo clone." >&2
        exit 1
    fi
    # shellcheck disable=SC1090  # fetched at runtime; the static path is the sibling branch above
    source "$tmp"
    rm -f "$tmp"
}
source_lib

function show_help() {
    cat << EOF
Usage: $(basename "$0") [options]

Removes memql-worker from this machine: the user-systemd units (the
worker's, and the model runtime's memql-ollama.service when
\`memql worker setup --inference\` wrote one), the binary and its
symlink, and ~/.memql/worker.yaml (the token) and worker.env. Nothing
outside ~/.memql, the unit files and the binary path is touched.

Options:
    --user-local              Remove a --user-local install from
                              \$HOME/.memql/bin instead of the default
                              /usr/local/bin (which needs sudo).
    --purge                   Also remove ~/.memql/policy.yaml, the state
                              dir (logs, ledgers), the native model
                              runtime and its models under ~/.memql/ollama
                              and, once it is empty, ~/.memql itself.
                              Without it they are kept, and the script
                              says so.
    --help                    Print this help

The machine's registration on the cluster is revoked from MemQL OS
(Fleet -> Machines), not from here.
EOF
}

function parse_args() {
    REMOVE_MODE="system"  # default: the sudo-gated /usr/local/bin, as the install's
    PURGE="no"

    while [[ $# -gt 0 ]]; do
        case "$1" in
            --user-local) REMOVE_MODE="user-local"; shift ;;
            --purge)      PURGE="yes"; shift ;;
            --help|-h)    show_help; exit 0 ;;
            *)
                # 2 is "bad parameter" in the capability-script exit
                # convention this tree already uses (preflight_asset's
                # 4, cut-release.sh's table).
                echo "ERROR: unknown flag $1" >&2
                show_help
                exit 2
                ;;
        esac
    done
}

# remove_systemd_unit stops, disables and removes the user units: the
# worker's current name, its pre-rename one, and the model runtime's
# (memql-ollama.service, written by `memql worker setup --inference`
# and by nothing else -- without this line a --purge would delete the
# runtime out from under a unit still trying to restart it). The
# runtime's unit goes BEFORE the runtime directory does, for that
# reason. A failed disable (for example an unavailable user manager)
# reports a partial uninstall and keeps the unit and runtime state
# for a safe retry while the worker token is still removed. A machine
# with no systemctl on PATH gets the same preservation: an absent
# command cannot establish that the service stopped.
function remove_systemd_unit() {
    local unit_dir="${HOME}/.config/systemd/user"
    local have_systemctl="yes"
    if ! command -v systemctl >/dev/null 2>&1; then
        have_systemctl="no"
        echo "WARN: systemctl not found; installed units and runtime state will be kept because their services cannot be stopped"
    fi
    local label unit path
    local unit_rc=0
    for label in "$SERVICE_LABEL_LINUX" "$LEGACY_LABEL_LINUX" "$OLLAMA_LABEL_LINUX"; do
        unit="${label}.service"
        path="${unit_dir}/${unit}"
        if [[ ! -f "$path" ]]; then
            # The legacy unit is absent on every machine installed after
            # the rename; only the current ones are worth a line.
            if [[ "$label" != "$LEGACY_LABEL_LINUX" ]]; then
                echo "INFO: ${path} not present; no unit to stop"
            fi
            continue
        fi
        if [[ "$have_systemctl" == "no" ]]; then
            record_kept "$path (systemctl missing; retry uninstall with the user manager available)"
            unit_rc=1
            continue
        fi
        if [[ "$have_systemctl" == "yes" ]]; then
            if systemctl --user disable --now "$unit" >/dev/null 2>&1; then
                echo "INFO: stopped and disabled ${unit}"
            else
                echo "WARN: systemctl --user disable --now ${unit} failed; keeping the unit and runtime state so a running service is not purged"
                record_kept "$path (stop failed; retry uninstall when the user manager is available)"
                unit_rc=1
                continue
            fi
        fi
        rm -f "$path"
        echo "INFO: removed ${path}"
        record_removed "$path"
    done
    if [[ "$have_systemctl" == "yes" ]]; then
        systemctl --user daemon-reload >/dev/null 2>&1 || true
    fi
    return "$unit_rc"
}

function main() {
    parse_args "$@"
    # Read BEFORE the token files go: --purge deletes the directory the
    # worker actually used, and the default is only where that usually
    # is.
    local state_dir
    state_dir="$(worker_state_dir_from_yaml "${HOME}/.memql/worker.yaml")"
    # Service, binary, tokens: the install in reverse, and the order that
    # leaves the least behind if a step is interrupted -- a
    # Restart=on-failure unit still running would re-exec a binary that
    # is about to go, with a token that is about to go.
    local unit_rc=0
    remove_systemd_unit || unit_rc=$?
    # A binary that needs sudo this run cannot get is reported and
    # left; the tokens still go, because they matter more, and the exit
    # code carries the leftover.
    local binary_rc=0
    remove_binaries_with_mode "$REMOVE_MODE" || binary_rc=$?
    remove_worker_config
    remove_path_if_present "${HOME}/.memql/worker.env"
    if [[ "$PURGE" == "yes" && "$unit_rc" -eq 0 ]]; then
        purge_worker_state "$state_dir"
    else
        report_kept_state "$state_dir"
    fi
    if [[ "$unit_rc" -ne 0 ]]; then binary_rc="$unit_rc"; fi
    print_uninstall_summary "$binary_rc"
    exit "$binary_rc"
}

main "$@"
