#!/usr/bin/env bash
#
# scripts/install/uninstall-mac.sh
# ================================
#
# Remove memql-worker from a macOS machine: install-mac.sh run
# backwards, in one line, because an install that is one line and an
# uninstall that is a runbook leaves the token on machines nobody
# finishes cleaning up. Unloads and removes the LaunchAgent, removes
# the binary and its symlink, removes workers.yaml and worker.yaml
# (the tokens); --purge
# takes policy.yaml, the state dir and an emptied ~/.memql as well.
# Every step says what it did, or that there was nothing to do, and
# nothing here prompts except sudo.
#
# Usage:
#   ./uninstall-mac.sh [--purge] [--user-local]
#
# What this never touches: anything outside ~/.memql, the two
# LaunchAgent plists, and the binary paths under the chosen prefix.
# The machine's registration on the cluster is revoked from MemQL OS
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

Removes memql-worker from this machine: the LaunchAgent, the binary
and its symlink, and ~/.memql/workers.yaml plus the legacy
worker.yaml (the tokens). Nothing outside ~/.memql, the plist files
and the binary path is touched.

Options:
    --user-local              Remove a --user-local install from
                              \$HOME/.memql/bin instead of the default
                              /usr/local/bin (which needs sudo).
    --purge                   Also remove ~/.memql/policy.yaml, the state
                              dir (logs, ledgers) and, once it is empty,
                              ~/.memql itself. Without it they are kept,
                              and the script says so.
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

# remove_launch_agent unloads and removes the LaunchAgent, the current
# label and the pre-rename one. `launchctl unload` is what
# install-mac.sh's own restart uses, so an agent it could load this can
# unload; a failed unload (not loaded in this session) is a WARN and
# the plist still goes, because a KeepAlive plist left on disk is a
# worker that comes back at the next login. A machine with no launchctl
# on PATH -- nothing macOS ships, but lib_test.sh runs this script on
# Linux -- is told, and gets the file removal only.
function remove_launch_agent() {
    local plist_dir="${HOME}/Library/LaunchAgents"
    local have_launchctl="yes"
    if ! command -v launchctl >/dev/null 2>&1; then
        have_launchctl="no"
        echo "INFO: launchctl not found; not unloading the LaunchAgent (the plist files are still removed)"
    fi
    local label plist
    for label in "$SERVICE_LABEL_DARWIN" "$LEGACY_LABEL_DARWIN"; do
        plist="${plist_dir}/${label}.plist"
        if [[ ! -f "$plist" ]]; then
            # The legacy plist is absent on every machine installed
            # after the rename; only the current one is worth a line.
            if [[ "$label" == "$SERVICE_LABEL_DARWIN" ]]; then
                echo "INFO: ${plist} not present; no LaunchAgent to stop"
            fi
            continue
        fi
        if [[ "$have_launchctl" == "yes" ]]; then
            if launchctl unload "$plist" >/dev/null 2>&1; then
                echo "INFO: unloaded the ${label} LaunchAgent"
            else
                echo "WARN: launchctl unload ${plist} failed (not loaded?); removing the plist anyway"
            fi
        fi
        rm -f "$plist"
        echo "INFO: removed ${plist}"
        record_removed "$plist"
    done
}

function main() {
    parse_args "$@"
    # Read BEFORE the token files go: --purge deletes the directory the
    # worker actually used, and the default is only where that usually
    # is.
    local state_dir
    state_dir="$(worker_state_dir_from_yaml "${HOME}/.memql/worker.yaml")"
    # Service, binary, tokens: the install in reverse, and the order that
    # leaves the least behind if a step is interrupted -- a KeepAlive
    # agent still running would re-exec a binary that is about to go,
    # with a token that is about to go.
    remove_launch_agent
    # A binary that needs sudo this run cannot get is reported and
    # left; the tokens still go, because they matter more, and the exit
    # code carries the leftover.
    local binary_rc=0
    remove_binaries_with_mode "$REMOVE_MODE" || binary_rc=$?
    remove_worker_config
    if [[ "$PURGE" == "yes" ]]; then
        purge_worker_state "$state_dir"
    else
        report_kept_state "$state_dir"
    fi
    print_uninstall_summary "$binary_rc"
    exit "$binary_rc"
}

main "$@"
