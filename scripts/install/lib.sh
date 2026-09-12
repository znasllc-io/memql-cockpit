#!/usr/bin/env bash
#
# scripts/install/lib.sh
# ======================
#
# Shared function library for the worker install scripts --
# function-based helpers sourced by the OS-specific drivers
# install-mac.sh and install-linux.sh, and by their inverses
# uninstall-mac.sh and uninstall-linux.sh. write_worker_yaml (below) is
# the single renderer for ~/.memql/workers.yaml (+ legacy mirror), so the config layout
# can never drift between the two platforms; the uninstall helpers at
# the bottom are the single statement of what an uninstall removes,
# for the same reason. The pure-logic helpers are exercised by
# lib_test.sh, wired into CI via
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

# The pre-rename binary names the installers retire in place and the
# uninstallers remove (znasllc-io/memql#4553). The root install.sh
# carries its own copy under the same variable name -- it is fetched
# alone by `curl | sh` and has no lib.sh to source -- and lib_test.sh
# reads both files to hold the two copies together.
readonly LEGACY_BINARIES="memql-cockpit memql-cockpit-computeruse"

# The service names, current and pre-rename, on each platform. The
# installers write the current one and retire the legacy one; the
# uninstallers stop and remove both. install.sh restates these four
# too, under these names, for the reason above. Nothing in this file
# reads them -- the drivers that source it do -- hence the directive.
# shellcheck disable=SC2034  # read by the uninstallers, which source this file
readonly SERVICE_LABEL_DARWIN="com.znasllc.memql-worker" \
         LEGACY_LABEL_DARWIN="com.znasllc.memql-cockpit-worker" \
         SERVICE_LABEL_LINUX="memql-worker" \
         LEGACY_LABEL_LINUX="memql-cockpit-worker"

# The model runtime's own user unit on Linux, written by `memql worker
# setup --inference` (internal/worker/inference/stage.go, OllamaUnitName)
# rather than by an installer, and removed by the Linux uninstaller so a
# machine is not left serving models from a runtime nobody can see in
# Fleet. It has no macOS twin: there the runtime is Homebrew's service.
# shellcheck disable=SC2034  # read by uninstall-linux.sh, which sources this file
readonly OLLAMA_LABEL_LINUX="memql-ollama"

# ---------------------------------------------------------------
# Version-aware install + shared terminal UX
# ---------------------------------------------------------------
#
# VERSION is the cockpit tag (see VERSION / VERSIONING.md). Installed
# binaries answer `memql --version` as `memql X.Y.Z (headless|computeruse)`.
# --force never means "binary exists"; it only remaps workers.yaml homes
# (see write_worker_yaml). Same version → no-op skip; newer installed →
# refuse downgrade; older/missing → install/upgrade.

# memql_ascii_banner prints a compact mark + wordmark + version line.
# Inspired by brand/mark.svg (9-node graph); kept small for macOS
# Terminal and Linux TTYs alike.
function memql_ascii_banner() {
    local ver="${1:-}"
    local ver_line="MemQL Cockpit worker installer"
    if [[ -n "$ver" ]]; then
        ver_line="MemQL Cockpit worker installer  v${ver#v}"
    fi
    cat << 'BANNER'
        .  o   .
     o--+--+--+--o
        |\/|\/|
     o--+--+--+--o
        |\/|\/|
     o--+--+--+--o
        '  o   .
BANNER
    echo "  ${ver_line}"
    echo ""
}

# install_step prints a numbered human-readable step header.
function install_step() {
    local n="$1"
    local msg="$2"
    echo ""
    echo "==> [${n}] ${msg}"
}

# normalize_semver strips a leading v and any build metadata / variant
# suffix so "v0.12.1", "0.12.1", and "0.12.1 (headless)" compare equal.
function normalize_semver() {
    local raw="$1"
    raw="${raw#v}"
    raw="${raw%%[[:space:]]*}"
    raw="${raw%%+*}"
    raw="${raw%%-*}"
    echo "$raw"
}

# parse_memql_version_line extracts X.Y.Z from `memql --version` output
# (`memql 0.12.1 (headless)`). Empty on failure.
function parse_memql_version_line() {
    local line="$1"
    # Prefer the token after "memql ", else first semver-looking token.
    local ver
    ver="$(printf '%s\n' "$line" | sed -n -E 's/^[[:space:]]*memql[[:space:]]+([0-9]+(\.[0-9]+){1,3}).*/\1/p' | head -1)"
    if [[ -z "$ver" ]]; then
        ver="$(printf '%s\n' "$line" | sed -n -E 's/.*(^|[[:space:]])v?([0-9]+(\.[0-9]+){1,3}).*/\2/p' | head -1)"
    fi
    normalize_semver "$ver"
}

# read_binary_version runs `$1 --version` and parses it. Empty if the
# binary is missing or does not answer.
function read_binary_version() {
    local bin="$1"
    if [[ -z "$bin" || ! -x "$bin" ]]; then
        echo ""
        return 0
    fi
    local out
    out="$("$bin" --version 2>/dev/null | head -1)" || out=""
    parse_memql_version_line "$out"
}

# compare_semver prints -1 / 0 / 1 for a<b / a==b / a>b (numeric dotted).
# Non-numeric segments compare as 0. Empty either side → treat as 0.0.0.
function compare_semver() {
    local a b
    a="$(normalize_semver "${1:-0}")"
    b="$(normalize_semver "${2:-0}")"
    [[ -z "$a" ]] && a="0"
    [[ -z "$b" ]] && b="0"
    local IFS=.
    # shellcheck disable=SC2086
    set -- $a
    local a1="${1:-0}" a2="${2:-0}" a3="${3:-0}"
    # shellcheck disable=SC2086
    set -- $b
    local b1="${1:-0}" b2="${2:-0}" b3="${3:-0}"
    a1="${a1:-0}"; a2="${a2:-0}"; a3="${a3:-0}"
    b1="${b1:-0}"; b2="${b2:-0}"; b3="${b3:-0}"
    # Strip non-digits for bash arithmetic safety.
    a1="${a1%%[!0-9]*}"; a2="${a2%%[!0-9]*}"; a3="${a3%%[!0-9]*}"
    b1="${b1%%[!0-9]*}"; b2="${b2%%[!0-9]*}"; b3="${b3%%[!0-9]*}"
    a1="${a1:-0}"; a2="${a2:-0}"; a3="${a3:-0}"
    b1="${b1:-0}"; b2="${b2:-0}"; b3="${b3:-0}"
    if (( a1 < b1 )); then echo -1; return; fi
    if (( a1 > b1 )); then echo 1; return; fi
    if (( a2 < b2 )); then echo -1; return; fi
    if (( a2 > b2 )); then echo 1; return; fi
    if (( a3 < b3 )); then echo -1; return; fi
    if (( a3 > b3 )); then echo 1; return; fi
    echo 0
}

# resolve_target_version picks the version about to be installed.
# Order: MEMQL_INSTALL_VERSION → sibling/repo VERSION file → version
# embedded in --download-base (.../releases/download/vX.Y.Z/...) → empty
# (caller downloads and reads the binary).
function resolve_target_version() {
    local download_base="${1:-}"
    local ver=""
    if [[ -n "${MEMQL_INSTALL_VERSION:-}" ]]; then
        normalize_semver "$MEMQL_INSTALL_VERSION"
        return 0
    fi
    local here sibling_version
    here="$(cd "$(dirname "${BASH_SOURCE[0]:-$0}")" 2>/dev/null && pwd)" || here=""
    for sibling_version in \
        "${here}/../../VERSION" \
        "${here}/../VERSION" \
        "${MEMQL_INSTALL_VERSION_FILE:-}"
    do
        [[ -n "$sibling_version" && -f "$sibling_version" ]] || continue
        ver="$(tr -d '[:space:]' < "$sibling_version")"
        if [[ -n "$ver" ]]; then
            normalize_semver "$ver"
            return 0
        fi
    done
    # sed keeps this portable on macOS bash 3.2 (no BASH_REMATCH nests).
    ver="$(printf '%s' "$download_base" | sed -n -E 's#.*/download/v?([0-9]+(\.[0-9]+){1,3})(/.*)?$#\1#p')"
    if [[ -n "$ver" ]]; then
        normalize_semver "$ver"
        return 0
    fi
    echo ""
}

# home_id_from_cluster_url mirrors Go HomeIDFromURL: host only, lowercased.
function home_id_from_cluster_url() {
    local cluster_url="$1"
    local home_id
    home_id="$(echo "$cluster_url" | sed -E 's#^[a-zA-Z][a-zA-Z0-9+.-]*://##' | sed -E 's#/.*##' | sed -E 's#:[0-9]+$##' | tr '[:upper:]' '[:lower:]')"
    if [[ -z "$home_id" ]]; then
        home_id="default"
    fi
    echo "$home_id"
}

# same_cluster_url mirrors Go sameClusterURL (trim, case-fold, trailing /).
function same_cluster_url() {
    local a b
    a="$(echo "$1" | sed -E 's/[[:space:]]+$//; s/^[[:space:]]+//; s#/+$##')"
    b="$(echo "$2" | sed -E 's/[[:space:]]+$//; s/^[[:space:]]+//; s#/+$##')"
    local al bl
    al="$(printf '%s' "$a" | tr '[:upper:]' '[:lower:]')"
    bl="$(printf '%s' "$b" | tr '[:upper:]' '[:lower:]')"
    if [[ "$al" == "$bl" ]]; then
        return 0
    fi
    local ha hb
    ha="$(home_id_from_cluster_url "$a")"
    hb="$(home_id_from_cluster_url "$b")"
    [[ "$ha" != "default" && "$ha" == "$hb" ]]
}

# find_home_id_by_cluster_url prints the id of a home whose cluster_url
# matches, or empty. Used so install (URL-host id) and pair (--home-id
# local) refresh the same enrollment without --force.
function find_home_id_by_cluster_url() {
    local workers_path="$1"
    local cluster_url="$2"
    [[ -f "$workers_path" ]] || { echo ""; return 0; }
    # BSD awk runs END even after exit — guard with found=1.
    awk -v want="$cluster_url" '
        function norm(s) {
            gsub(/^[[:space:]]+|[[:space:]]+$/, "", s)
            sub(/\/+$/, "", s)
            return tolower(s)
        }
        BEGIN { wantn = norm(want); in_homes=0; cur=""; curl=""; found=0 }
        /^homes:[[:space:]]*$/ { in_homes=1; next }
        !in_homes { next }
        /^[[:space:]]*-[[:space:]]*id:[[:space:]]*/ {
            if (cur != "" && curl != "" && norm(curl) == wantn) { print cur; found=1; exit }
            cur=$0; sub(/^[[:space:]]*-[[:space:]]*id:[[:space:]]*/, "", cur)
            curl=""
            next
        }
        /^[[:space:]]*cluster_url:[[:space:]]*/ {
            curl=$0; sub(/^[[:space:]]*cluster_url:[[:space:]]*/, "", curl)
            next
        }
        END { if (!found && cur != "" && curl != "" && norm(curl) == wantn) print cur }
    ' "$workers_path" | head -1
}

# find_home_cluster_url_by_id prints cluster_url for a given home id.
function find_home_cluster_url_by_id() {
    local workers_path="$1"
    local home_id="$2"
    [[ -f "$workers_path" ]] || { echo ""; return 0; }
    awk -v want="$home_id" '
        BEGIN { in_homes=0; cur=""; curl=""; found=0 }
        /^homes:[[:space:]]*$/ { in_homes=1; next }
        !in_homes { next }
        /^[[:space:]]*-[[:space:]]*id:[[:space:]]*/ {
            if (cur == want && curl != "") { print curl; found=1; exit }
            cur=$0; sub(/^[[:space:]]*-[[:space:]]*id:[[:space:]]*/, "", cur)
            curl=""
            next
        }
        /^[[:space:]]*cluster_url:[[:space:]]*/ {
            curl=$0; sub(/^[[:space:]]*cluster_url:[[:space:]]*/, "", curl)
            next
        }
        END { if (!found && cur == want && curl != "") print curl }
    ' "$workers_path" | head -1
}

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

# Pick the DOWNLOAD artifact name for the requested headless /
# computeruse flavour and the detected platform.
#
# The artifact carries the variant AND the platform; the installed file is
# always plain $INSTALLED_COMMAND. Both names derive from that one
# constant, so a rename cannot move the download without moving the
# install. These strings must also agree with the Makefile's dist targets
# and release.yml's upload list -- TestReleaseArtifactNamesAgree holds
# those three together.
function binary_name_for() {
    local flavour="$1"  # "headless" or "computeruse"
    local os arch
    os="$(detect_os)"
    arch="$(detect_arch)"
    case "$flavour" in
        headless)
            echo "${INSTALLED_COMMAND}-${os}-${arch}"
            ;;
        computeruse)
            echo "${INSTALLED_COMMAND}-computeruse-${os}-${arch}"
            ;;
        *)
            echo "ERROR: unknown flavour $flavour" >&2
            return 1
            ;;
    esac
}

# preflight_asset probes the resolved release-asset URL BEFORE anything
# is mutated -- before the sudo prompt, the binary install, the
# worker.yaml write and the LaunchAgent / systemd load -- so a flavour
# whose asset the release never published (the --computeruse case,
# znasllc-io/memql-cockpit#374) is a clean refusal up front instead of a
# bare curl 404 after the operator has already typed their password.
#
# The probe is a HEAD after redirects (`curl -fsIL`; GitHub release
# download URLs answer HEAD). When HEAD fails, a one-byte ranged GET
# retries the same question -- some curl builds mishandle HEAD on
# non-HTTP protocols (file:// among them, which is how lib_test.sh
# exercises this offline), and GitHub's missing-asset answer to HEAD is
# a connection drop rather than a clean 404, so the second opinion costs
# one byte and removes a false refusal either way. Deliberately NO
# --proto '=https' here: the probe writes nothing (-o /dev/null), so the
# https-only enforcement stays on download_binary, the call that
# produces the bytes that get executed.
#
# On refusal the message names the exact URL and the flavour/platform
# pair, and returns 4 -- prerequisite missing, the same capability-script
# exit convention cut-release.sh uses (2 bad param, 3 refused, 4
# prerequisite missing). The installers run under `set -e`, so the
# return surfaces as the process exit code.
function preflight_asset() {
    local url="$1"
    local flavour="$2"  # "headless" or "computeruse", for the message
    if ! command -v curl >/dev/null 2>&1; then
        echo "ERROR: curl required" >&2
        return 4
    fi
    echo "INFO: checking release asset $url"
    if curl -fsIL -o /dev/null "$url"; then
        return 0
    fi
    if curl -fsL -r 0-0 -o /dev/null "$url"; then
        return 0
    fi
    local os arch
    os="$(detect_os)"
    arch="$(detect_arch)"
    echo "ERROR: release asset not found (or unreachable): $url" >&2
    echo "       (flavour: ${flavour}, platform: ${os}/${arch})" >&2
    echo "       The default download base is the LATEST release" >&2
    echo "       (releases/latest/download), which may not publish an asset for" >&2
    echo "       this flavour. Check the release's published assets, or pass" >&2
    echo "       --download-base to point at a release that ships it." >&2
    echo "       Nothing was installed or modified." >&2
    return 4
}

# Download the supplied URL to the supplied path, with a basic
# integrity check (HTTP 200, non-empty file).
function download_binary() {
    local url="$1"
    local dest="$2"
    if ! command -v curl >/dev/null 2>&1; then
        echo "ERROR: curl required for the download step." >&2
        echo "       Install curl, or pass --download-base file:///... for an offline asset." >&2
        return 1
    fi
    echo "INFO: downloading $url"
    # Progress bar when stdout is a TTY; silent -sS for CI / pipes.
    local curl_flags=(-fL --proto '=https')
    if [[ -t 1 ]]; then
        curl_flags+=(--progress-bar)
    else
        curl_flags+=(-sS)
    fi
    if ! curl "${curl_flags[@]}" "$url" -o "$dest.partial"; then
        echo "ERROR: download failed for $url" >&2
        echo "       Check network access, or pass --download-base to a release that" >&2
        echo "       publishes this asset. Nothing was installed from this URL." >&2
        rm -f "$dest.partial"
        return 1
    fi
    if [[ ! -s "$dest.partial" ]]; then
        echo "ERROR: downloaded file is empty ($url)" >&2
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
# instead of failing in the middle of the download, and by
# remove_binaries_with_mode for the same directory on the way out.
#
# $1 is the action being gated, "install" (the default) or
# "uninstall", and it changes the WORDING only: the way out of a
# missing sudo is --user-local in both cases, but telling a person who
# is removing a worker to "install under ~/.memql/bin instead" sends
# them the wrong way.
function require_sudo() {
    local action="${1:-install}"
    local need="the immutable install at $INSTALL_PREFIX_SYSTEM"
    local way_out="install under $INSTALL_PREFIX_USER"
    if [[ "$action" == "uninstall" ]]; then
        need="removing the immutable install at $INSTALL_PREFIX_SYSTEM"
        way_out="remove a --user-local install from $INSTALL_PREFIX_USER"
    fi
    if [[ $EUID -eq 0 ]]; then
        return 0
    fi
    if ! command -v sudo >/dev/null 2>&1; then
        echo "ERROR: sudo required for ${need} but is not available." >&2
        echo "       Pass --user-local to ${way_out} instead." >&2
        return 1
    fi
    if sudo -n true 2>/dev/null; then
        return 0
    fi
    echo "INFO: $INSTALL_PREFIX_SYSTEM ${action} requires sudo; you'll be prompted for your password..."
    if ! sudo -v; then
        echo "ERROR: sudo authentication failed. Pass --user-local for a passwordless ${action}." >&2
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
    # Optional 5th arg: target version (empty → resolve / read after download).
    local target_ver="${5:-}"

    local dest_dir
    dest_dir="$(install_mode_dir "$mode")"
    INSTALL_BINARY_DEST="$dest_dir/$binary"
    INSTALL_BINARY_FRIENDLY="$dest_dir/$friendly_name"
    INSTALL_BINARY_ACTION=""   # skip | upgrade | fresh | refuse
    INSTALL_BINARY_BEFORE=""
    INSTALL_BINARY_AFTER=""

    local installed_ver=""
    if [[ -x "$INSTALL_BINARY_FRIENDLY" ]]; then
        installed_ver="$(read_binary_version "$INSTALL_BINARY_FRIENDLY")"
    elif [[ -x "$INSTALL_BINARY_DEST" ]]; then
        installed_ver="$(read_binary_version "$INSTALL_BINARY_DEST")"
    fi
    INSTALL_BINARY_BEFORE="$installed_ver"

    if [[ -z "$target_ver" ]]; then
        target_ver="$(resolve_target_version "")"
    fi

    # Same version already installed → no-op (no re-download, no --force).
    if [[ -n "$installed_ver" && -n "$target_ver" ]]; then
        local cmp
        cmp="$(compare_semver "$installed_ver" "$target_ver")"
        if [[ "$cmp" == "0" ]]; then
            echo "INFO: already at v${installed_ver}; skipping binary download"
            INSTALL_BINARY_ACTION="skip"
            INSTALL_BINARY_AFTER="$installed_ver"
            return 0
        fi
        if [[ "$cmp" == "1" ]]; then
            echo "ERROR: installed memql v${installed_ver} is newer than v${target_ver} about to install." >&2
            echo "       Refusing to downgrade. Install a newer release, or remove the" >&2
            echo "       existing binary first (see uninstall-mac.sh / uninstall-linux.sh)." >&2
            echo "       --force does not override this (it only remaps workers.yaml homes)." >&2
            INSTALL_BINARY_ACTION="refuse"
            return 3
        fi
        echo "INFO: upgrading memql v${installed_ver} → v${target_ver}"
        INSTALL_BINARY_ACTION="upgrade"
    elif [[ -n "$installed_ver" && -z "$target_ver" ]]; then
        # Unknown target: download to temp, compare, then decide.
        :
        INSTALL_BINARY_ACTION="upgrade"
        echo "INFO: installed memql v${installed_ver}; checking downloaded binary version"
    else
        INSTALL_BINARY_ACTION="fresh"
        if [[ -n "$target_ver" ]]; then
            echo "INFO: fresh install → v${target_ver}"
        else
            echo "INFO: fresh install (version resolved after download)"
        fi
    fi

    case "$mode" in
        system)
            require_sudo
            local tmp
            tmp="$(mktemp)"
            if ! download_binary "$url" "$tmp"; then
                rm -f "$tmp"
                return 1
            fi
            local dl_ver
            dl_ver="$(read_binary_version "$tmp")"
            if [[ -z "$target_ver" && -n "$dl_ver" ]]; then
                target_ver="$dl_ver"
            fi
            if [[ -n "$installed_ver" && -n "$dl_ver" ]]; then
                local cmp2
                cmp2="$(compare_semver "$installed_ver" "$dl_ver")"
                if [[ "$cmp2" == "0" ]]; then
                    echo "INFO: already at v${installed_ver}; downloaded asset matches — leaving binary in place"
                    rm -f "$tmp"
                    INSTALL_BINARY_ACTION="skip"
                    INSTALL_BINARY_AFTER="$installed_ver"
                    return 0
                fi
                if [[ "$cmp2" == "1" ]]; then
                    echo "ERROR: installed memql v${installed_ver} is newer than downloaded v${dl_ver}." >&2
                    echo "       Refusing to downgrade. --force does not override this." >&2
                    rm -f "$tmp"
                    INSTALL_BINARY_ACTION="refuse"
                    return 3
                fi
                echo "INFO: upgrading memql v${installed_ver} → v${dl_ver}"
                INSTALL_BINARY_ACTION="upgrade"
            elif [[ -z "$installed_ver" && -n "$dl_ver" ]]; then
                echo "INFO: fresh install → v${dl_ver}"
                INSTALL_BINARY_ACTION="fresh"
            fi
            sudo mkdir -p "$dest_dir"
            sudo install -m 0755 "$tmp" "$INSTALL_BINARY_DEST"
            rm -f "$tmp"
            sudo ln -sf "$INSTALL_BINARY_DEST" "$INSTALL_BINARY_FRIENDLY"
            ;;
        user-local)
            mkdir -p "$dest_dir"
            local tmp
            tmp="$(mktemp)"
            if ! download_binary "$url" "$tmp"; then
                rm -f "$tmp"
                return 1
            fi
            local dl_ver
            dl_ver="$(read_binary_version "$tmp")"
            if [[ -z "$target_ver" && -n "$dl_ver" ]]; then
                target_ver="$dl_ver"
            fi
            if [[ -n "$installed_ver" && -n "$dl_ver" ]]; then
                local cmp2
                cmp2="$(compare_semver "$installed_ver" "$dl_ver")"
                if [[ "$cmp2" == "0" ]]; then
                    echo "INFO: already at v${installed_ver}; downloaded asset matches — leaving binary in place"
                    rm -f "$tmp"
                    INSTALL_BINARY_ACTION="skip"
                    INSTALL_BINARY_AFTER="$installed_ver"
                    return 0
                fi
                if [[ "$cmp2" == "1" ]]; then
                    echo "ERROR: installed memql v${installed_ver} is newer than downloaded v${dl_ver}." >&2
                    echo "       Refusing to downgrade. --force does not override this." >&2
                    rm -f "$tmp"
                    INSTALL_BINARY_ACTION="refuse"
                    return 3
                fi
                echo "INFO: upgrading memql v${installed_ver} → v${dl_ver}"
                INSTALL_BINARY_ACTION="upgrade"
            elif [[ -z "$installed_ver" && -n "$dl_ver" ]]; then
                echo "INFO: fresh install → v${dl_ver}"
                INSTALL_BINARY_ACTION="fresh"
            fi
            mv "$tmp" "$INSTALL_BINARY_DEST"
            chmod +x "$INSTALL_BINARY_DEST"
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

    INSTALL_BINARY_AFTER="$(read_binary_version "$INSTALL_BINARY_FRIENDLY")"
    if [[ -z "$INSTALL_BINARY_AFTER" ]]; then
        INSTALL_BINARY_AFTER="$target_ver"
    fi
    if [[ -n "$INSTALL_BINARY_BEFORE" && -n "$INSTALL_BINARY_AFTER" && "$INSTALL_BINARY_ACTION" != "skip" ]]; then
        echo "INFO: binary ${INSTALL_BINARY_BEFORE} → ${INSTALL_BINARY_AFTER}"
    elif [[ -z "$INSTALL_BINARY_BEFORE" && -n "$INSTALL_BINARY_AFTER" ]]; then
        echo "INFO: binary fresh install → v${INSTALL_BINARY_AFTER}"
    fi

    # Migrate: drop the pre-rename binaries/symlinks beside the new one
    # (znasllc-io/memql#4553). Harmless when absent. The names come from
    # LEGACY_BINARIES so the uninstaller removes exactly the set this
    # retires.
    local legacy
    for legacy in $LEGACY_BINARIES; do
        case "$mode" in
            system)
                sudo rm -f "$dest_dir/$legacy" 2>/dev/null || true
                ;;
            *)
                rm -f "$dest_dir/$legacy" 2>/dev/null || true
                ;;
        esac
    done
}

function linux_worker_capabilities() {
    local flavour="$1" wayland_display="$2" session_type="$3" display="$4"
    session_type="${session_type#"${session_type%%[![:space:]]*}"}"
    session_type="${session_type%"${session_type##*[![:space:]]}"}"
    if [[ "$flavour" != computeruse || -n "$wayland_display" || -z "$display" ]]; then
        echo HEADLESS
        return
    fi
    case "$session_type" in
        [Ww][Aa][Yy][Ll][Aa][Nn][Dd]) echo HEADLESS ;;
        *) echo HEADLESS,COMPUTERUSE ;;
    esac
}

# write_worker_yaml upserts one home into ~/.memql/workers.yaml and
# mirrors that home into the legacy worker.yaml path. ADDITIVE: a
# second install against a different cluster keeps the first home.
#
# --force (force == "yes") is ONLY for remapping a home id onto a
# different cluster_url (cluster identity change). It is NOT required
# for: same cluster_url refresh (even when the suggested id differs
# from the enrolled id, e.g. install host-id vs `pair --home-id local`),
# same-id token rotation, or "binary already exists".
#
# Args:
#   $1 path          -- legacy worker.yaml path (usually ~/.memql/worker.yaml)
#   $2 cluster_url   -- cluster edge URL
#   $3 token         -- worker token (mql_wkr_...)
#   $4 name          -- worker name
#   $5 force         -- "yes" to remap matched home id → new URL (default "no")
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
    local dir workers_path home_id match_id existing_url
    dir="$(dirname "$path")"
    workers_path="${dir}/workers.yaml"
    # Default id from URL host (Go HomeIDFromURL). May be remapped below
    # when an existing home already holds this cluster_url under a
    # different id (e.g. pair --home-id local vs install host id).
    home_id="$(home_id_from_cluster_url "$cluster_url")"
    match_id="$home_id"

    mkdir -p "$dir"

    # Match semantics (aligned with Go UpsertHome):
    #   1. Same cluster_url → refresh that home in place; preserve its id.
    #      No --force. (Fixes install host-id vs pair --home-id local.)
    #   2. Same home id + same/empty URL → refresh token; no --force.
    #   3. Same home id + DIFFERENT cluster_url → --force required
    #      (cluster identity remapping). Siblings are always kept.
    # --force never means "binary exists" and never wipes sibling homes.
    if [[ -e "$workers_path" ]]; then
        local by_url
        by_url="$(find_home_id_by_cluster_url "$workers_path" "$cluster_url")"
        by_url="$(printf '%s' "$by_url" | tr -d '\r' | head -1)"
        if [[ -n "$by_url" ]]; then
            match_id="$by_url"
            echo "INFO: refreshing existing home ${match_id} for ${cluster_url} (token upsert; --force not required)"
        else
            existing_url="$(find_home_cluster_url_by_id "$workers_path" "$home_id")"
            if [[ -n "$existing_url" ]]; then
                if same_cluster_url "$existing_url" "$cluster_url"; then
                    match_id="$home_id"
                elif [[ "$force" != "yes" ]]; then
                    echo "ERROR: $workers_path already has home id '${home_id}' for ${existing_url}." >&2
                    echo "       Pass --force to remap that home to ${cluster_url} (siblings are preserved)." >&2
                    echo "       Same cluster_url under another id refreshes without --force." >&2
                    return 1
                else
                    match_id="$home_id"
                    echo "INFO: --force remapping home ${match_id}: ${existing_url} → ${cluster_url}"
                fi
            fi
        fi
    elif [[ -e "$path" && "$force" != "yes" ]]; then
        # Legacy-only machine: promote via rewrite rather than refuse —
        # the Go loader migrates on first run too. Still require --force
        # only when we would destroy the legacy token for a *different*
        # URL; same URL refresh is fine.
        local legacy_url
        legacy_url="$(grep -E '^cluster_url:' "$path" | head -1 | sed -E 's/^cluster_url:[[:space:]]*//')"
        if [[ -n "$legacy_url" ]] && ! same_cluster_url "$legacy_url" "$cluster_url"; then
            echo "ERROR: $path already exists for ${legacy_url}; pass --force to replace that home (siblings are preserved in workers.yaml)" >&2
            return 1
        fi
    fi

    # Rebuild workers.yaml: keep sibling homes, upsert this one.
    # Preserve existing registry HEADER fields (worker_name, labels,
    # concurrency, state_dir, log_level) when present so a second
    # install/pair does not clobber Go-tuned shared knobs. Capabilities
    # still come from this install (computer-use may widen them), matching
    # Go UpsertHome when Capabilities is non-empty. Fresh files get defaults.
    local tmp
    tmp="$(mktemp)"
    local out_name out_state out_log
    out_name="$name"
    out_state="$STATE_DIR_DEFAULT"
    out_log="info"
    local labels_block concurrency_block
    labels_block="$(printf 'labels:\n  os: %s\n  arch: %s\n' "$(detect_os)" "$(detect_arch)")"
    concurrency_block="$(printf 'concurrency:\n  HEADLESS: 8\n  COMPUTERUSE: 1\n')"
    if [[ -e "$workers_path" ]]; then
        local existing
        existing="$(sed -n -E 's/^worker_name:[[:space:]]*//p' "$workers_path" | head -1)"
        [[ -n "$existing" ]] && out_name="$existing"
        existing="$(sed -n -E 's/^state_dir:[[:space:]]*"?([^"#]*[^"#[:space:]])"?[[:space:]]*(#.*)?$/\1/p' "$workers_path" | head -1)"
        [[ -n "$existing" ]] && out_state="$existing"
        existing="$(sed -n -E 's/^log_level:[[:space:]]*//p' "$workers_path" | head -1)"
        [[ -n "$existing" ]] && out_log="$existing"
        existing="$(awk '
            /^labels:[[:space:]]*$/ {grab=1; print; next}
            grab && /^[^[:space:]#]/ {exit}
            grab {print}
        ' "$workers_path")"
        [[ -n "$existing" ]] && labels_block="$existing"
        existing="$(awk '
            /^concurrency:[[:space:]]*$/ {grab=1; print; next}
            grab && /^[^[:space:]#]/ {exit}
            grab {print}
        ' "$workers_path")"
        [[ -n "$existing" ]] && concurrency_block="$existing"
    fi
    {
        echo "version: 1"
        echo "worker_name: ${out_name}"
        printf '%s\n' "$labels_block"
        printf '%s\n' "$concurrency_block"
        echo "state_dir: ${out_state}"
        echo "log_level: ${out_log}"
        echo "capabilities:"
        echo "$capabilities" | tr ',' '\n' | sed 's/^/  - /'
        echo "homes:"
        if [[ -e "$workers_path" ]]; then
            # Emit sibling home blocks; skip the matched id (URL or id).
            awk -v keep_id="$match_id" '
                BEGIN { in_homes=0; skip=0; buf="" }
                /^homes:[[:space:]]*$/ { in_homes=1; next }
                !in_homes { next }
                /^[[:space:]]*-[[:space:]]*id:[[:space:]]*/ {
                    if (buf != "" && !skip) printf "%s", buf
                    buf = $0 "\n"
                    id=$0; sub(/^[[:space:]]*-[[:space:]]*id:[[:space:]]*/, "", id)
                    skip = (id == keep_id) ? 1 : 0
                    next
                }
                in_homes {
                    if (buf == "") next
                    buf = buf $0 "\n"
                }
                END { if (buf != "" && !skip) printf "%s", buf }
            ' "$workers_path"
        elif [[ -e "$path" ]]; then
            # Promote legacy single-home when present and URL differs.
            local leg_url leg_token leg_id
            leg_url="$(grep -E '^cluster_url:' "$path" | head -1 | sed -E 's/^cluster_url:[[:space:]]*//')"
            leg_token="$(grep -E '^token:' "$path" | head -1 | sed -E 's/^token:[[:space:]]*//')"
            if [[ -n "$leg_url" ]] && ! same_cluster_url "$leg_url" "$cluster_url" && [[ -n "$leg_token" ]]; then
                leg_id="$(home_id_from_cluster_url "$leg_url")"
                echo "  - id: ${leg_id}"
                echo "    cluster_url: ${leg_url}"
                echo "    token: ${leg_token}"
                echo "    enabled: true"
            fi
        fi
        echo "  - id: ${match_id}"
        echo "    cluster_url: ${cluster_url}"
        echo "    token: ${token}"
        echo "    enabled: true"
    } > "$tmp"
    mv "$tmp" "$workers_path"
    chmod 600 "$workers_path"
    echo "INFO: upserted home ${match_id} in $workers_path (capabilities: ${capabilities})"

    # Legacy mirror for older tooling / mid-transition LaunchAgents.
    # state_dir is namespaced per home (homes/<id>) to match ConfigForHome
    # so a single-file reader does not share registration with siblings.
    cat > "$path" << YAML
cluster_url: ${cluster_url}
token: ${token}
name: ${out_name}
labels:
  os: $(detect_os)
  arch: $(detect_arch)
concurrency:
  HEADLESS: 8
  COMPUTERUSE: 1
state_dir: ${out_state}/homes/${match_id}
log_level: ${out_log}
capabilities:
$(echo "$capabilities" | tr ',' '\n' | sed 's/^/  - /')
YAML
    chmod 600 "$path"
    echo "INFO: mirrored legacy $path"
}

function setup_inference() {
    local binary="$1"
    echo ""
    echo "INFO: setting this machine up to serve local models"
    local rc=0
    "$binary" worker setup --inference --non-interactive || rc=$?
    case "$rc" in
        0)
            return 0
            ;;
        3)
            echo ""
            echo "INFO: the model runtime is not installed here, and a scripted run"
            echo "      is not allowed to approve installing it. Run this yourself,"
            echo "      in this same terminal:"
            echo ""
            echo "  ${binary} worker setup --inference"
            echo ""
            ;;
        *)
            echo "WARN: '${binary} worker setup --inference' exited ${rc}." >&2
            echo "      This machine is paired and working; it is not offering local" >&2
            echo "      models. Run the command above without --non-interactive to" >&2
            echo "      read why." >&2
            ;;
    esac
    return 0
}

# ---------------------------------------------------------------
# Uninstall helpers -- shared by uninstall-mac.sh and uninstall-linux.sh
# ---------------------------------------------------------------
#
# The uninstallers are the installers run backwards, and everything
# that is the same on both platforms lives here so the two cannot
# disagree about what "uninstalled" means: which binaries go, which
# files under ~/.memql go always, which go only under --purge, and
# the one fence every recursive delete is checked against. The
# service half (launchctl against systemctl) stays in the drivers, as
# the install half does.
#
# Two ledgers, appended to by every helper that removes or keeps
# something and printed by print_uninstall_summary. Newline-joined
# strings rather than arrays: macOS ships bash 3.2, where expanding an
# EMPTY array under `set -u` is an unbound-variable error, and an
# empty ledger is the common case for half of these helpers.
UNINSTALL_REMOVED=""
UNINSTALL_KEPT=""

function record_removed() {
    UNINSTALL_REMOVED="${UNINSTALL_REMOVED}  $1"$'\n'
}

function record_kept() {
    UNINSTALL_KEPT="${UNINSTALL_KEPT}  $1"$'\n'
}

# under_memql_home answers whether $1 lies inside ${HOME}/.memql, the
# ONLY tree the uninstallers delete recursively. It is strict about
# shape on purpose -- absolute, no `..` segment -- because a state_dir
# read out of worker config is operator-authored text, and the one thing
# an uninstaller must never do is `rm -rf` wherever a file it did not
# write points.
function under_memql_home() {
    local path="$1"
    local fence="${HOME}/.memql"
    [[ "$path" == /* ]] || return 1
    [[ "$path" != *"/../"* && "$path" != *"/.." ]] || return 1
    [[ "$path" == "$fence" || "$path" == "$fence"/* ]]
}

# remove_path_if_present deletes ONE file or symlink and says so, or
# says there was nothing there. Never recursive -- a directory handed
# to it is reported and left -- so the drivers can name paths outside
# the ~/.memql fence (the LaunchAgent plist, the systemd unit) without
# the fence being all that stands between them and a tree.
function remove_path_if_present() {
    local path="$1"
    if [[ -d "$path" && ! -L "$path" ]]; then
        echo "WARN: $path is a directory; not removed by this step"
        return 0
    fi
    if [[ ! -e "$path" && ! -L "$path" ]]; then
        echo "INFO: $path not present; nothing to remove"
        return 0
    fi
    rm -f "$path"
    echo "INFO: removed $path"
    record_removed "$path"
}

# remove_worker_config deletes the worker token files on a full
# uninstall: the multi-home registry (~/.memql/workers.yaml) and the
# legacy single-home mirror (~/.memql/worker.yaml). Both hold cluster
# tokens, so both always go -- not only under --purge. clusters.yaml
# and credentials/ stay: they belong to `memql cluster`, not the worker.
function remove_worker_config() {
    remove_path_if_present "${HOME}/.memql/workers.yaml"
    remove_path_if_present "${HOME}/.memql/worker.yaml"
}

# remove_tree_if_present deletes a directory recursively, inside the
# ~/.memql fence and nowhere else. Outside it the directory is KEPT
# and reported with its path, so the person can decide. That is not
# an error: the uninstall still did everything it was allowed to.
function remove_tree_if_present() {
    local dir="$1"
    if [[ ! -e "$dir" && ! -L "$dir" ]]; then
        echo "INFO: $dir not present; nothing to remove"
        return 0
    fi
    if ! under_memql_home "$dir"; then
        echo "WARN: $dir is outside ${HOME}/.memql; not touched. Delete it by hand if you want it gone."
        record_kept "$dir (outside ${HOME}/.memql; not touched)"
        return 0
    fi
    rm -rf "$dir"
    echo "INFO: removed $dir"
    record_removed "$dir"
}

# worker_state_dir_from_yaml prints the state_dir worker config names,
# or the default when the file or the key is absent. Prefers the path
# handed in (usually legacy worker.yaml), then the multi-home registry
# workers.yaml beside it -- install always mirrors both, but a machine
# that only has the registry still has a purge target. The drivers call
# it BEFORE the token files are removed: --purge has to delete the
# directory the worker actually used, and write_worker_yaml's default
# is only where that usually is. A leading `~/` is expanded the way
# the shell would have; anything else reaches the fence as written.
function worker_state_dir_from_yaml() {
    local path="$1"
    local dir=""
    local try
    for try in "$path" "$(dirname "$path")/workers.yaml"; do
        if [[ -f "$try" ]]; then
            dir="$(sed -n -E 's/^state_dir:[[:space:]]*"?([^"#]*[^"#[:space:]])"?[[:space:]]*(#.*)?$/\1/p' "$try" | head -1)"
            [[ -n "$dir" ]] && break
        fi
    done
    case "$dir" in
        "")   dir="$STATE_DIR_DEFAULT" ;;
        \~/*) dir="${HOME}/${dir#\~/}" ;;
    esac
    echo "$dir"
}

# remove_binaries_with_mode is install_binary_with_mode's inverse: from
# the mode's directory it deletes the installed command, the two
# download-named binaries beside it (headless and computer-use, for
# this platform) and the pre-rename names. Every name derives from the
# constants the install used, so a rename moves both or neither.
#
# Scoped to that ONE directory on purpose, as install.sh's
# remove_legacy_binaries is: a PATH-wide sweep would delete a memql
# this installer never placed. Sudo is asked for only once something
# is actually there to remove -- a machine with nothing at
# /usr/local/bin must not raise a password prompt to find that out.
# When sudo is needed and cannot be had, the paths are printed for the
# person to delete by hand and the function returns 4 (prerequisite
# missing, the capability-script convention preflight_asset uses); the
# drivers carry on to the token, which matters more than the binary,
# and exit with that code at the end.
function remove_binaries_with_mode() {
    local mode="$1"
    local dest_dir
    dest_dir="$(install_mode_dir "$mode")" || return 1
    local headless computeruse
    headless="$(binary_name_for headless)" || return 1
    computeruse="$(binary_name_for computeruse)" || return 1
    local names="${INSTALLED_COMMAND} ${headless} ${computeruse} ${LEGACY_BINARIES}"

    local name path present="no"
    for name in $names; do
        path="${dest_dir}/${name}"
        if [[ -e "$path" || -L "$path" ]]; then
            present="yes"
        fi
    done
    if [[ "$present" == "no" ]]; then
        echo "INFO: no ${INSTALLED_COMMAND} binary at ${dest_dir}; nothing to remove"
        return 0
    fi

    local privileged="no"
    if [[ "$mode" == "system" && $EUID -ne 0 ]]; then
        if ! require_sudo uninstall; then
            echo "ERROR: cannot remove from ${dest_dir} without sudo. Delete these by hand:" >&2
            for name in $names; do
                path="${dest_dir}/${name}"
                if [[ -e "$path" || -L "$path" ]]; then
                    echo "       $path" >&2
                    record_kept "$path (needs sudo; delete it by hand)"
                fi
            done
            return 4
        fi
        privileged="yes"
    fi

    # The command's symlink goes first (it is first in the list), so no
    # moment leaves a `memql` pointing at a file already gone.
    for name in $names; do
        path="${dest_dir}/${name}"
        [[ -e "$path" || -L "$path" ]] || continue
        if [[ "$privileged" == "yes" ]]; then
            sudo rm -f "$path"
        else
            rm -f "$path"
        fi
        echo "INFO: removed $path"
        record_removed "$path"
    done
    # The user-local bin dir was the installer's to create, so it is
    # taken away again when nothing is left in it. /usr/local/bin is
    # nobody's to remove, and rmdir refuses a non-empty directory.
    if [[ "$mode" == "user-local" ]]; then
        rmdir "$dest_dir" 2>/dev/null || true
    fi
}

# purge_worker_state is what --purge adds: policy.yaml (the owner's
# apps.allow / models.allow / backup.roots -- kept by default because
# it is authored, not generated), the state dir (logs, ledgers, the
# recorded registration id), the consent socket, and then ~/.memql
# itself once nothing is left in it. The CLI's clusters.yaml and
# credentials/ are never on this list: they belong to `memql cluster`,
# not to the worker, and an uninstall of the worker that signed the
# person out of every cluster would be a second thing nobody asked
# for. When they are there ~/.memql stays, and the summary says what
# kept it.
function purge_worker_state() {
    local state_dir="$1"
    remove_path_if_present "${HOME}/.memql/policy.yaml"
    remove_tree_if_present "$state_dir"
    remove_path_if_present "${HOME}/.memql/worker.sock"
    # The native model runtime and its models (Linux): gigabytes under
    # the fence, kept without --purge because the models were pulled on
    # purpose and cost hours to pull again.
    remove_tree_if_present "${HOME}/.memql/ollama"
    remove_memql_home_if_empty
}

# report_kept_state is the no-purge counterpart: it names what stays
# and the flag that removes it, so a token-less ~/.memql is never left
# behind unexplained.
function report_kept_state() {
    local state_dir="$1"
    local path
    for path in "${HOME}/.memql/policy.yaml" "$state_dir" "${HOME}/.memql/ollama"; do
        if [[ -e "$path" ]]; then
            echo "INFO: kept $path (re-run with --purge to remove it)"
            record_kept "$path (--purge removes it)"
        fi
    done
}

# remove_memql_home_if_empty takes ~/.memql away only when it is
# empty -- rmdir, never rm -rf, so whatever is still in there decides.
# What kept it is named, because "kept ~/.memql" alone reads as a
# purge that did not work.
function remove_memql_home_if_empty() {
    local dir="${HOME}/.memql"
    [[ -d "$dir" ]] || return 0
    if rmdir "$dir" 2>/dev/null; then
        echo "INFO: removed $dir (empty)"
        record_removed "$dir"
        return 0
    fi
    # A glob walk rather than `ls`: no external tool, and a name with
    # a space or a newline in it is listed rather than split. The three
    # patterns are the visible entries, the dotfiles, and the `..x`
    # names the second pattern cannot reach; an unmatched pattern stays
    # literal and fails the existence test.
    local left="" entry
    for entry in "$dir"/* "$dir"/.[!.]* "$dir"/..?*; do
        if [[ -e "$entry" || -L "$entry" ]]; then
            left="${left}${entry##*/} "
        fi
    done
    echo "INFO: kept $dir; it still holds: ${left}"
    record_kept "$dir (still holds: ${left})"
}

# print_uninstall_summary is the closing block, the uninstall's
# counterpart to the installers' SUCCESS block: what went, what stayed
# and why, and the one thing this script cannot do. The registration
# row lives on the cluster, and the token that could have spoken for
# this machine has just been deleted -- Fleet -> Machines in MemQL OS
# is where a machine is revoked, and a worker retrying with a dead
# token is the reason to revoke there first. $1 is the binary step's
# return code: non-zero means something is still on disk, and the
# heading says so rather than claiming success over a leftover.
function print_uninstall_summary() {
    local rc="${1:-0}"
    local heading="SUCCESS: memql-worker uninstalled."
    if [[ "$rc" -ne 0 ]]; then
        heading="PARTIAL: memql-worker uninstalled, with leftovers (see Kept)."
    fi
    echo ""
    echo "================================================================"
    echo "$heading"
    echo ""
    echo "Removed:"
    if [[ -n "$UNINSTALL_REMOVED" ]]; then
        printf '%s' "$UNINSTALL_REMOVED"
    else
        echo "  (nothing)"
    fi
    echo ""
    echo "Kept:"
    if [[ -n "$UNINSTALL_KEPT" ]]; then
        printf '%s' "$UNINSTALL_KEPT"
    else
        echo "  (nothing)"
    fi
    # A memql still resolving on PATH after this is one this script did
    # not install -- the system copy after a --user-local run, or a
    # `go install` -- and saying so beats a person typing `memql` and
    # concluding the uninstall did nothing.
    local other
    if other="$(command -v "$INSTALLED_COMMAND" 2>/dev/null)" && [[ -n "$other" ]]; then
        echo ""
        echo "NOTE: a ${INSTALLED_COMMAND} is still on your PATH at ${other}."
        echo "      This script did not put it there and has not touched it."
    fi
    echo ""
    echo "This machine's registration on the cluster is revoked from MemQL OS"
    echo "(Fleet -> Machines), not from here."
    echo "================================================================"
}
