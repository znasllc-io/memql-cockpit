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
#   ./uninstall-mac.sh --cluster=URL|--all-homes [--purge] [--user-local]
#
# Scope: ~/.memql, the managed LaunchAgents, binary paths under the chosen
# prefix, and the standard MemQL app for that prefix. Rollback copies remain.
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
worker.yaml (the tokens). Also removes the standard MemQL app and menu helper;
custom app destinations and rollback copies are retained.

Options:
    --cluster=URL             Remove this cluster enrollment. Keep the app and
                              services if any other enrollment remains.
    --all-homes               Remove every worker enrollment and shared runtime.
                              Required unless --cluster is supplied.
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
    CLUSTER_URL=""
    ALL_HOMES="no"

    while [[ $# -gt 0 ]]; do
        case "$1" in
            --user-local) REMOVE_MODE="user-local"; shift ;;
            --purge)      PURGE="yes"; shift ;;
            --cluster=*)  CLUSTER_URL="${1#*=}"; shift ;;
            --cluster)    [[ $# -gt 1 ]] || { echo "ERROR: --cluster needs a URL" >&2; exit 2; }; CLUSTER_URL="$2"; shift 2 ;;
            --all-homes)  ALL_HOMES="yes"; shift ;;
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
    if [[ -z "$CLUSTER_URL" && "$ALL_HOMES" != yes ]] || [[ -n "$CLUSTER_URL" && "$ALL_HOMES" == yes ]]; then
        echo "ERROR: choose --cluster=URL or --all-homes" >&2
        exit 2
    fi
    if [[ -L "$HOME/.memql" ]]; then
        echo "ERROR: refusing an aliased ~/.memql directory" >&2
        exit 3
    fi
}

# Stop by service label even when a partially removed install lost its plist.
# A failed stop of a still-loaded agent is an error; never delete its executable.
function stop_agent() {
    local label="$1" target
    if ! command -v launchctl >/dev/null 2>&1; then
        echo "INFO: launchctl not found; removing service files only"
        return 0
    fi
    target="gui/$(id -u)/$1"
    if launchctl print "$target" >/dev/null 2>&1; then
        launchctl bootout "$target" >/dev/null 2>&1 || true
        if launchctl print "$target" >/dev/null 2>&1; then
            echo "ERROR: could not stop $label; runtime files retained" >&2
            return 5
        fi
    fi
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
    local label plist
    for label in "$SERVICE_LABEL_DARWIN" "$LEGACY_LABEL_DARWIN" "com.visionarys.memql-cockpit-worker" "com.znasllc.memql-cockpit-menubar"; do
        plist="${plist_dir}/${label}.plist"
        stop_agent "$label" || return $?
        remove_path_if_present "$plist"
    done
}

# Only the standard companion bundle bearing our identifier is removed.
# Custom destinations and rollback copies remain for the owner to inspect.
function remove_menu_companion() {
    local app="$HOME/Applications/MemQL Cockpit.app" identifier
    [[ -e "$app" || -L "$app" ]] || return 0
    if [[ -L "$app" || ! -d "$app" || ! -O "$app" ]]; then
        echo "WARN: leaving unexpected menu app path $app"
        record_kept "$app"
        return 0
    fi
    identifier="$(/usr/libexec/PlistBuddy -c 'Print :CFBundleIdentifier' "$app/Contents/Info.plist" 2>/dev/null || true)"
    if [[ "$identifier" != com.znasllc.memql-cockpit-menubar ]]; then
        echo "WARN: leaving app with unexpected bundle identifier at $app"
        record_kept "$app"
        return 0
    fi
    rm -rf "$app"
    record_removed "$app"
}

function remove_worker_app() {
    local app identifier
    case "$REMOVE_MODE" in system) app="/Applications/MemQL.app" ;; *) app="$HOME/Applications/MemQL.app" ;; esac
    [[ -e "$app" || -L "$app" ]] || return 0
    if [[ -L "$app" || ! -d "$app" ]]; then record_kept "$app"; return 1; fi
    identifier="$(/usr/libexec/PlistBuddy -c 'Print :CFBundleIdentifier' "$app/Contents/Info.plist" 2>/dev/null || true)"
    if [[ "$identifier" != com.znasllc.memql-worker ]]; then
        echo "WARN: leaving unrelated app at $app"; record_kept "$app"; return 1
    fi
    case "$REMOVE_MODE" in
        system)
            if ! require_sudo uninstall || ! sudo rm -rf "$app"; then record_kept "$app"; return 1; fi
            ;;
        *)
            if [[ ! -O "$app" ]]; then record_kept "$app"; return 1; fi
            rm -rf "$app"
            ;;
    esac
    record_removed "$app"
}

# Use the worker's YAML decoder and identity rules, never a text approximation
# of workers.yaml. A partially missing CLI may still have the real app worker.
function scoped_unpair() {
    local binary="$HOME/Applications/MemQL.app/Contents/MacOS/MemQL"
    [[ "$REMOVE_MODE" != system ]] || binary="/Applications/MemQL.app/Contents/MacOS/MemQL"
    [[ -x "$binary" ]] || binary="$(install_mode_dir "$REMOVE_MODE")/memql"
    if [[ ! -f "$HOME/.memql/workers.yaml" && ! -f "$HOME/.memql/worker.yaml" && ! -L "$HOME/.memql/workers.yaml" && ! -L "$HOME/.memql/worker.yaml" ]]; then
        OTHER_HOMES=0
        return 0
    fi
    if [[ ! -x "$binary" ]]; then
        echo "ERROR: cluster-scoped removal needs the installed MemQL binary to parse enrollment safely. Restore its files, or explicitly use --all-homes for complete removal." >&2
        return 4
    fi
    local result remaining plist target was_loaded=no
    result="$(mktemp)"
    if ! "$binary" worker unpair --cluster-url "$CLUSTER_URL" --dry-run --json > "$result"; then
        rm -f "$result"; return 5
    fi
    remaining="$(/usr/bin/plutil -extract remaining raw -o - "$result")" || { rm -f "$result"; return 5; }
    if [[ "$PURGE" == yes && "$remaining" -gt 0 ]]; then
        rm -f "$result"
        echo "ERROR: --purge would erase state shared with other enrollments; no enrollment was removed. Omit --purge or explicitly use --all-homes." >&2
        return 3
    fi
    plist="$HOME/Library/LaunchAgents/$SERVICE_LABEL_DARWIN.plist"
    target="gui/$(id -u)/$SERVICE_LABEL_DARWIN"
    if command -v launchctl >/dev/null 2>&1 && launchctl print "$target" >/dev/null 2>&1; then
        # Reload only a service that was running and has a retained plist.
        if [[ "$remaining" -gt 0 && ( ! -f "$plist" || -L "$plist" ) ]]; then
            rm -f "$result"; echo "ERROR: running worker has no regular service plist to reload safely" >&2; return 3
        fi
        was_loaded=yes
    fi
    stop_agent "$SERVICE_LABEL_DARWIN" || { rm -f "$result"; return 5; }
    if ! "$binary" worker unpair --cluster-url "$CLUSTER_URL" --json > "$result"; then
        rm -f "$result"
        echo "ERROR: removal did not complete; worker remains stopped so a removed token cannot reconnect. Repair enrollment before restarting." >&2
        return 5
    fi
    OTHER_HOMES="$(/usr/bin/plutil -extract remaining raw -o - "$result")" || { rm -f "$result"; return 5; }
    rm -f "$result"
    if [[ "$OTHER_HOMES" -gt 0 ]]; then
        local legacy
        for legacy in "$LEGACY_LABEL_DARWIN" "com.visionarys.memql-cockpit-worker"; do
            stop_agent "$legacy" || return $?
            remove_path_if_present "$HOME/Library/LaunchAgents/$legacy.plist"
        done
        if [[ "$was_loaded" == yes ]]; then
            launchctl bootstrap "gui/$(id -u)" "$plist" || { echo "ERROR: enrollment removed but remaining-home worker could not reload" >&2; return 5; }
        fi
        echo "SUCCESS: selected cluster enrollment removed; $OTHER_HOMES other enrollment(s) and the shared app, CLI, menu, policy and state retained."
    fi
}

function main() {
    parse_args "$@"
    local state_dir binary_rc=0
    state_dir="$(worker_state_dir_from_yaml "${HOME}/.memql/worker.yaml")"
    if [[ -n "$CLUSTER_URL" ]]; then
        scoped_unpair || exit $?
        [[ "$OTHER_HOMES" -eq 0 ]] || exit 0
    fi
    remove_launch_agent || exit $?
    remove_menu_companion
    remove_binaries_with_mode "$REMOVE_MODE" || binary_rc=$?
    remove_worker_app || binary_rc=1
    remove_worker_config
    if [[ "$PURGE" == "yes" ]]; then
        purge_worker_state "$state_dir"
    else
        report_kept_state "$state_dir"
    fi
    echo "INFO: CLI credentials, cluster settings, certificates and rollback backups are retained."
    echo "INFO: macOS privacy entries may remain in System Settings; remove them there if desired. No privacy database or grants were reset."
    print_uninstall_summary "$binary_rc"
    exit "$binary_rc"
}

main "$@"
