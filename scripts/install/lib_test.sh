#!/usr/bin/env bash
#
# scripts/install/lib_test.sh
# ============================
#
# Smoke tests for lib.sh's pure-logic helpers: install_mode_dir,
# require_sudo's failure mode, binary_name_for (headless + computeruse
# flavours), and write_worker_yaml (fresh write, 0600 mode, capability
# list, and its --force clobber guard). The /usr/local/bin system path
# can't be exercised in CI without elevated privileges, so the
# system-mode path is checked via the require_sudo failure mode.
# Also exercises the installers' own lib.sh sourcing: the cloned-repo
# sibling path, and the curl|bash fallback that fetches lib.sh from
# MEMQL_INSTALL_RAW_BASE when no sibling exists.
# And the release-asset preflight: preflight_asset's verdicts over
# file:// fixtures (present passes, missing exits 4 naming the URL and
# the flavour/platform pair), plus both installers refusing BEFORE any
# mutation when the asset is missing and proceeding past the preflight
# to the download when it is present.
# And the --inference pass-through: setup_inference's reading of the
# setup command's exit code (0 quiet, 3 prints the interactive command
# on ONE physical line, anything else reported), that it never fails the
# install, and that both installers accept the flag and call it after
# worker.yaml and the service.
# And the uninstallers: --help and the unknown-flag exit, lib.sh
# sourcing (they join the installers' loop), and real runs against a
# throwaway HOME with launchctl / systemctl / sudo cut out of PATH --
# what --user-local removes and keeps (worker.yaml + workers.yaml),
# what --purge adds, the
# nothing-installed run, ~/.memql kept for the CLI's clusters.yaml, and
# the fence that keeps a state_dir outside ~/.memql from being deleted.
#
# Run: bash scripts/install/lib_test.sh
# Wired into CI by .github/workflows/install-scripts-lint.yml.

set -uo pipefail

# Source lib.sh; suppress its `set -uo pipefail` from killing this
# driver if a helper returns non-zero (we assert that here).
# shellcheck disable=SC1091
source "$(dirname "$0")/lib.sh"

PASS=0
FAIL=0

function fail() {
    echo "FAIL: $*" >&2
    FAIL=$((FAIL + 1))
}

function pass() {
    echo "PASS: $*"
    PASS=$((PASS + 1))
}

function expect_eq() {
    local name="$1"
    local got="$2"
    local want="$3"
    if [[ "$got" == "$want" ]]; then
        pass "$name"
    else
        fail "$name: got=$got want=$want"
    fi
}

# ---------------------------------------------------------------
# install_mode_dir
# ---------------------------------------------------------------

expect_eq "install_mode_dir system" "$(install_mode_dir system)" "/usr/local/bin"
expect_eq "install_mode_dir user-local" "$(install_mode_dir user-local)" "${HOME}/.memql/bin"

# Unknown mode prints to stderr + non-zero.
if install_mode_dir bogus >/dev/null 2>&1; then
    fail "install_mode_dir bogus accepted; should have errored"
else
    pass "install_mode_dir bogus rejected"
fi

# ---------------------------------------------------------------
# require_sudo behaviour
# ---------------------------------------------------------------

# Running as root would short-circuit; in CI / dev we're not root.
if [[ $EUID -eq 0 ]]; then
    echo "INFO: running as root -- skipping require_sudo failure-mode tests"
else
    # Force sudo to be unreachable by clearing PATH inside a subshell
    # (so `command -v sudo` and sudo itself both fail). Running the
    # subshell as the `if` condition checks its status directly.
    if (
        # shellcheck disable=SC2123  # clearing PATH is the point of this probe
        PATH=""
        require_sudo
    ) >/dev/null 2>&1; then
        fail "require_sudo passed with PATH cleared (expected fail)"
    else
        pass "require_sudo fails when sudo unreachable"
    fi
fi

# ---------------------------------------------------------------
# binary_name_for
# ---------------------------------------------------------------

_os="$(detect_os)"
_arch="$(detect_arch)"

expect_eq "binary_name_for headless" \
    "$(binary_name_for headless)" "${INSTALLED_COMMAND}-${_os}-${_arch}"
expect_eq "binary_name_for computeruse" \
    "$(binary_name_for computeruse)" "${INSTALLED_COMMAND}-computeruse-${_os}-${_arch}"

# Unknown flavour prints to stderr + non-zero.
if binary_name_for bogus >/dev/null 2>&1; then
    fail "binary_name_for bogus accepted; should have errored"
else
    pass "binary_name_for bogus rejected"
fi

# ---------------------------------------------------------------
# write_worker_yaml
# ---------------------------------------------------------------

_tmp="$(mktemp -d)"
trap 'rm -rf "$_tmp"' EXIT
_wy="${_tmp}/worker.yaml"

# file_mode prints an octal permission string portably (GNU + BSD stat).
function file_mode() {
    stat -c '%a' "$1" 2>/dev/null || stat -f '%Lp' "$1" 2>/dev/null
}

# Fresh write with default capabilities (HEADLESS only).
if write_worker_yaml "$_wy" "https://c.example" "mql_wkr_abc" "host1" "no" >/dev/null 2>&1; then
    pass "write_worker_yaml fresh write succeeds"
else
    fail "write_worker_yaml fresh write should succeed"
fi

expect_eq "write_worker_yaml renders mode 0600" "$(file_mode "$_wy")" "600"

if grep -qx '  - HEADLESS' "$_wy"; then
    pass "write_worker_yaml default lists HEADLESS capability"
else
    fail "write_worker_yaml default should list HEADLESS capability"
fi

# COMPUTERUSE appears in the concurrency block, so match the capability
# LIST line exactly (whole line) rather than a substring.
if grep -qx '  - COMPUTERUSE' "$_wy"; then
    fail "write_worker_yaml default must not list COMPUTERUSE capability"
else
    pass "write_worker_yaml default omits COMPUTERUSE capability"
fi

# Same-URL refresh without --force is allowed (token rotation).
if write_worker_yaml "$_wy" "https://c.example" "mql_wkr_abc" "host1" "no" >/dev/null 2>&1; then
    pass "write_worker_yaml refreshes the same home without --force"
else
    fail "write_worker_yaml should refresh the same cluster_url without --force"
fi

# Additive: a second cluster keeps the first home in workers.yaml.
_wy_workers="$(dirname "$_wy")/workers.yaml"
if write_worker_yaml "$_wy" "https://d.example" "mql_wkr_def" "host1" "no" "HEADLESS" >/dev/null 2>&1; then
    if grep -q 'id: c.example' "$_wy_workers" && grep -q 'id: d.example' "$_wy_workers"; then
        pass "write_worker_yaml additive upsert keeps sibling homes"
    else
        fail "write_worker_yaml should keep both c.example and d.example in workers.yaml"
        echo "---- workers.yaml ----" >&2
        cat "$_wy_workers" >&2
    fi
else
    fail "write_worker_yaml should accept a second cluster without --force"
fi

# --force replaces THAT home only (computer-use caps) and keeps siblings.
if write_worker_yaml "$_wy" "https://c.example" "mql_wkr_abc" "host1" "yes" "HEADLESS,COMPUTERUSE" >/dev/null 2>&1; then
    pass "write_worker_yaml --force replaces matched home"
else
    fail "write_worker_yaml should accept --force for the matched home"
fi

if grep -qx '  - HEADLESS' "$_wy" && grep -qx '  - COMPUTERUSE' "$_wy"; then
    pass "write_worker_yaml computeruse lists HEADLESS + COMPUTERUSE"
else
    fail "write_worker_yaml computeruse should list HEADLESS + COMPUTERUSE"
fi

if grep -q 'id: d.example' "$_wy_workers"; then
    pass "write_worker_yaml --force preserves sibling homes"
else
    fail "write_worker_yaml --force must not drop sibling homes"
fi

# Same cluster_url under a different enrolled id (pair --home-id local
# vs install host-id) refreshes WITHOUT --force and keeps the enrolled id.
_url_mismatch="$(mktemp -d)/worker.yaml"
_url_workers="$(dirname "$_url_mismatch")/workers.yaml"
mkdir -p "$(dirname "$_url_mismatch")"
cat > "$_url_workers" << 'WY'
version: 1
worker_name: host1
labels:
  os: darwin
  arch: arm64
concurrency:
  HEADLESS: 8
  COMPUTERUSE: 1
state_dir: /tmp/x
log_level: info
capabilities:
  - HEADLESS
homes:
  - id: local
    cluster_url: https://api.memql.localhost
    token: mql_wkr_local_bbbbbbbbbbbb
    enabled: true
WY
if write_worker_yaml "$_url_mismatch" "https://api.memql.localhost" "mql_wkr_local_cccccccccccc" "host1" "no" "HEADLESS" >/dev/null 2>&1; then
    if grep -q 'id: local' "$_url_workers" && grep -q 'mql_wkr_local_cccccccccccc' "$_url_workers"; then
        if grep -q 'id: api.memql.localhost' "$_url_workers"; then
            fail "write_worker_yaml must not invent a second home for the same cluster_url"
        else
            pass "write_worker_yaml same cluster_url different id refreshes without --force"
        fi
    else
        fail "write_worker_yaml should keep id local and refresh token"
        cat "$_url_workers" >&2
    fi
else
    fail "write_worker_yaml should refresh same cluster_url without --force when ids differ"
fi

# Same id + different cluster_url still requires --force.
_remap="$(mktemp -d)/worker.yaml"
_remap_workers="$(dirname "$_remap")/workers.yaml"
mkdir -p "$(dirname "$_remap")"
cat > "$_remap_workers" << 'WY'
version: 1
worker_name: host1
capabilities:
  - HEADLESS
homes:
  - id: local
    cluster_url: https://api.memql.localhost
    token: mql_wkr_local_bbbbbbbbbbbb
    enabled: true
WY
if write_worker_yaml "$_remap" "https://api.other.example" "mql_wkr_other_dddddddddddd" "host1" "no" "HEADLESS" >/dev/null 2>&1; then
    # home_id from other URL is api.other.example — additive new home, OK.
    # Remap conflict is same *id* local onto a new URL.
    pass "write_worker_yaml additive different URL under new id does not need --force"
else
    fail "write_worker_yaml should append a new home for a new cluster_url"
fi
# Force the id-conflict path: write with a URL whose host_id is "local"
# is hard; instead pre-seed id matching host and try different URL via
# manually calling with cluster that home_id_from maps... Use explicit
# seed where id equals derived host of NEW url? Better: seed id c.example
# for url https://old.example and upsert https://c.example (home_id=c.example).
_idconflict="$(mktemp -d)/worker.yaml"
_idc_workers="$(dirname "$_idconflict")/workers.yaml"
mkdir -p "$(dirname "$_idconflict")"
cat > "$_idc_workers" << 'WY'
version: 1
worker_name: host1
capabilities:
  - HEADLESS
homes:
  - id: c.example
    cluster_url: https://old.example
    token: mql_wkr_old_eeeeeeeeeeeeee
    enabled: true
WY
if write_worker_yaml "$_idconflict" "https://c.example" "mql_wkr_new_ffffffffffffff" "host1" "no" "HEADLESS" >/dev/null 2>&1; then
    fail "write_worker_yaml must require --force to remap home id onto a different cluster_url"
else
    pass "write_worker_yaml requires --force for home id cluster remap"
fi
if write_worker_yaml "$_idconflict" "https://c.example" "mql_wkr_new_ffffffffffffff" "host1" "yes" "HEADLESS" >/dev/null 2>&1; then
    if grep -q 'cluster_url: https://c.example' "$_idc_workers" && grep -q 'id: c.example' "$_idc_workers"; then
        pass "write_worker_yaml --force remaps home id onto new cluster_url"
    else
        fail "write_worker_yaml --force should remap cluster_url"
        cat "$_idc_workers" >&2
    fi
else
    fail "write_worker_yaml --force should allow home id cluster remap"
fi

# ---------------------------------------------------------------
# Version helpers (compare / parse / resolve)
# ---------------------------------------------------------------

expect_eq "normalize_semver strips v" "$(normalize_semver 'v0.12.1')" "0.12.1"
expect_eq "parse_memql_version_line" "$(parse_memql_version_line 'memql 0.12.1 (headless)')" "0.12.1"
expect_eq "compare_semver equal" "$(compare_semver '0.12.1' '0.12.1')" "0"
expect_eq "compare_semver older" "$(compare_semver '0.11.0' '0.12.1')" "-1"
expect_eq "compare_semver newer" "$(compare_semver '0.13.0' '0.12.1')" "1"

# Fake installed binary via a shim that answers --version.
_verdir="$(mktemp -d)"
printf '%s\n' '#!/bin/sh' 'echo "memql 0.12.1 (headless)"' > "$_verdir/memql-same"
chmod +x "$_verdir/memql-same"
expect_eq "read_binary_version" "$(read_binary_version "$_verdir/memql-same")" "0.12.1"

# resolve_target_version from download-base tag path
expect_eq "resolve_target_version from download-base"     "$(resolve_target_version 'https://github.com/znasllc-io/memql-cockpit/releases/download/v0.12.1')"     "0.12.1"

# Second upsert must preserve Go-tuned shared header knobs (concurrency,
# labels, worker_name, state_dir, log_level), not rebuild them from
# install defaults.
_hdr="${_tmp}/header-preserve"
mkdir -p "$_hdr"
_hdr_legacy="${_hdr}/worker.yaml"
_hdr_workers="${_hdr}/workers.yaml"
cat > "$_hdr_workers" << 'HDR'
version: 1
worker_name: tuned-name
labels:
  os: darwin
  arch: arm64
  tier: gold
concurrency:
  HEADLESS: 3
  COMPUTERUSE: 2
state_dir: /custom/state
log_level: debug
capabilities:
  - HEADLESS
homes:
  - id: c.example
    cluster_url: https://c.example
    token: mql_wkr_oldtokennnnnnn
    enabled: true
HDR
if write_worker_yaml "$_hdr_legacy" "https://e.example" "mql_wkr_newtokennnnnnn" "should-not-replace-name" "no" "HEADLESS" >/dev/null 2>&1; then
    if grep -q 'worker_name: tuned-name' "$_hdr_workers" \
        && grep -q 'tier: gold' "$_hdr_workers" \
        && grep -q 'HEADLESS: 3' "$_hdr_workers" \
        && grep -q 'COMPUTERUSE: 2' "$_hdr_workers" \
        && grep -q 'state_dir: /custom/state' "$_hdr_workers" \
        && grep -q 'log_level: debug' "$_hdr_workers" \
        && grep -q 'id: c.example' "$_hdr_workers" \
        && grep -q 'id: e.example' "$_hdr_workers"; then
        pass "write_worker_yaml upsert preserves tuned registry header fields"
    else
        fail "write_worker_yaml upsert clobbered tuned header or dropped homes"
        echo "---- workers.yaml ----" >&2
        cat "$_hdr_workers" >&2
    fi
else
    fail "write_worker_yaml should upsert beside a tuned registry"
fi

# ---------------------------------------------------------------
# install.sh restates lib.sh's names -- they must agree
# ---------------------------------------------------------------
#
# The root install.sh is fetched ALONE by `curl | sh` and has no lib.sh to
# source, so it carries its own copy of the migration names. A rename
# applied to one file and not the other produces an installer that retires
# a service nobody registered, or registers one nothing will find -- both
# silent. Reading the literals out of the script is what makes the
# duplication safe.

_install_sh="$(dirname "$0")/../../install.sh"

function install_sh_value() {
    local key="$1"
    grep -E "^${key}=" "$_install_sh" | head -1 | sed -E "s/^${key}=\"?([^\"]*)\"?.*/\\1/"
}

if [[ -f "$_install_sh" ]]; then
    expect_eq "install.sh installs the same command name" \
        "$(install_sh_value BINARY)" "$INSTALLED_COMMAND"
    expect_eq "install.sh SERVICE_LABEL_DARWIN" \
        "$(install_sh_value SERVICE_LABEL_DARWIN)" "com.znasllc.memql-worker"
    expect_eq "install.sh SERVICE_LABEL_LINUX" \
        "$(install_sh_value SERVICE_LABEL_LINUX)" "memql-worker"
    expect_eq "install.sh LEGACY_LABEL_DARWIN" \
        "$(install_sh_value LEGACY_LABEL_DARWIN)" "com.znasllc.memql-cockpit-worker"
    expect_eq "install.sh LEGACY_LABEL_LINUX" \
        "$(install_sh_value LEGACY_LABEL_LINUX)" "memql-cockpit-worker"
    expect_eq "install.sh LEGACY_BINARIES" \
        "$(install_sh_value LEGACY_BINARIES)" "$LEGACY_BINARIES"

    # lib.sh OWNS the four labels now (the uninstallers read them from
    # it), so its constants must agree with install.sh's copy too: the
    # literal expectations above pin the names, these pin the two files
    # to each other.
    expect_eq "lib.sh SERVICE_LABEL_DARWIN agrees with install.sh" \
        "$SERVICE_LABEL_DARWIN" "$(install_sh_value SERVICE_LABEL_DARWIN)"
    expect_eq "lib.sh SERVICE_LABEL_LINUX agrees with install.sh" \
        "$SERVICE_LABEL_LINUX" "$(install_sh_value SERVICE_LABEL_LINUX)"
    expect_eq "lib.sh LEGACY_LABEL_DARWIN agrees with install.sh" \
        "$LEGACY_LABEL_DARWIN" "$(install_sh_value LEGACY_LABEL_DARWIN)"
    expect_eq "lib.sh LEGACY_LABEL_LINUX agrees with install.sh" \
        "$LEGACY_LABEL_LINUX" "$(install_sh_value LEGACY_LABEL_LINUX)"

    # And the installers, which still spell the file names inline, must
    # write the service lib.sh names and retire the legacy one it
    # names -- or the uninstaller removes a file the installer never
    # wrote.
    if grep -qF "${SERVICE_LABEL_DARWIN}.plist" "$(dirname "$0")/install-mac.sh" \
        && grep -qF "${LEGACY_LABEL_DARWIN}.plist" "$(dirname "$0")/install-mac.sh"; then
        pass "install-mac.sh writes and retires the plists lib.sh names"
    else
        fail "install-mac.sh should name ${SERVICE_LABEL_DARWIN}.plist and ${LEGACY_LABEL_DARWIN}.plist"
    fi
    if grep -qF "${SERVICE_LABEL_LINUX}.service" "$(dirname "$0")/install-linux.sh" \
        && grep -qF "${LEGACY_LABEL_LINUX}.service" "$(dirname "$0")/install-linux.sh"; then
        pass "install-linux.sh writes and retires the units lib.sh names"
    else
        fail "install-linux.sh should name ${SERVICE_LABEL_LINUX}.service and ${LEGACY_LABEL_LINUX}.service"
    fi

    # The new label must not BE the old one -- a migration that renames
    # nothing would satisfy every other assertion here.
    if [[ "$(install_sh_value SERVICE_LABEL_DARWIN)" == "$(install_sh_value LEGACY_LABEL_DARWIN)" ]]; then
        fail "install.sh's darwin service label equals the legacy one; it migrates nothing"
    else
        pass "install.sh darwin service label differs from the legacy label"
    fi
else
    fail "install.sh not found at $_install_sh"
fi

# ---------------------------------------------------------------
# Installer sourcing -- cloned repo vs curl|bash (piped stdin)
# ---------------------------------------------------------------
#
# The installers source lib.sh from $(dirname "$0"), which under
# `curl ... | bash` is the operator's cwd ($0 is `bash`), so they fall
# back to fetching lib.sh from MEMQL_INSTALL_RAW_BASE. That override is
# what keeps these tests offline: the real lib.sh in this directory IS
# the fixture, reached over file://, and the failure case points at a
# path that cannot resolve. `--help` is the probe because it exits 0
# before any download / sudo / service work. The uninstallers carry
# the same source_lib and travel the same way, so they are in the loop.

_script_dir="$(cd "$(dirname "$0")" && pwd)"
_piped_cwd="${_tmp}/piped-cwd"
mkdir -p "$_piped_cwd"

for _installer in install-mac.sh install-linux.sh uninstall-mac.sh uninstall-linux.sh; do
    # Piped from a cwd holding no lib.sh, with RAW_BASE at the real
    # lib.sh: must survive sourcing and reach flag handling.
    if _out="$(cd "$_piped_cwd" && MEMQL_INSTALL_RAW_BASE="file://${_script_dir}" \
        bash -s -- --help < "${_script_dir}/${_installer}" 2>&1)" \
        && [[ "$_out" == *"Usage:"* ]]; then
        pass "$_installer piped --help sources lib.sh from RAW_BASE"
    else
        fail "$_installer piped --help should print usage and exit 0; got: $_out"
    fi

    # The cloned-repo path must NEVER fetch: with RAW_BASE poisoned,
    # running next to lib.sh still works because the sibling wins.
    if _out="$(cd "$_script_dir" && MEMQL_INSTALL_RAW_BASE="file:///nonexistent-raw-base" \
        "./${_installer}" --help 2>&1)" && [[ "$_out" == *"Usage:"* ]]; then
        pass "$_installer cloned-repo --help never consults RAW_BASE"
    else
        fail "$_installer cloned-repo --help should use the sibling lib.sh; got: $_out"
    fi

    # Piped with a broken RAW_BASE: must die with an ERROR naming the
    # lib.sh URL it tried, not a cryptic bash sourcing error.
    if _out="$(cd "$_piped_cwd" && MEMQL_INSTALL_RAW_BASE="file://${_tmp}/no-such-dir" \
        bash -s -- --help < "${_script_dir}/${_installer}" 2>&1)"; then
        fail "$_installer piped with broken RAW_BASE should fail; got: $_out"
    elif [[ "$_out" == *"ERROR"* && "$_out" == *"file://${_tmp}/no-such-dir/lib.sh"* ]]; then
        pass "$_installer piped fetch failure names the lib.sh URL"
    else
        fail "$_installer piped fetch failure should name the URL it tried; got: $_out"
    fi
done

# ---------------------------------------------------------------
# preflight_asset -- refuse a missing release asset before mutating
# ---------------------------------------------------------------
#
# The helper alone first: verdicts over file:// (present -> 0, missing
# -> 4), which is what keeps these checks offline. file:// is also why
# the helper carries its ranged-GET fallback -- HEAD support for the
# FILE protocol varies by curl build -- so passing here proves the
# fallback chain, not just `curl -I`.

_pf_assets="${_tmp}/preflight-assets"
mkdir -p "$_pf_assets"
_pf_headless="$(binary_name_for headless)"
_pf_computeruse="$(binary_name_for computeruse)"
printf 'stub-binary' > "${_pf_assets}/${_pf_headless}"

if preflight_asset "file://${_pf_assets}/${_pf_headless}" headless >/dev/null 2>&1; then
    pass "preflight_asset passes a present asset"
else
    fail "preflight_asset should pass a present asset"
fi

_out="$(preflight_asset "file://${_pf_assets}/${_pf_computeruse}" computeruse 2>&1)"
_rc=$?
expect_eq "preflight_asset missing asset returns 4 (prerequisite missing)" "$_rc" "4"

if [[ "$_out" == *"file://${_pf_assets}/${_pf_computeruse}"* ]]; then
    pass "preflight_asset refusal names the exact asset URL"
else
    fail "preflight_asset refusal should name the asset URL; got: $_out"
fi

if [[ "$_out" == *"flavour: computeruse"* && "$_out" == *"${_os}/${_arch}"* ]]; then
    pass "preflight_asset refusal names flavour + platform"
else
    fail "preflight_asset refusal should name flavour + os/arch; got: $_out"
fi

# ---------------------------------------------------------------
# Installer preflight -- refusal mutates nothing; presence passes
# ---------------------------------------------------------------
#
# Now the call site: both installers must refuse AT the preflight --
# exit 4, nothing created under HOME, no download attempted -- and a
# present asset must sail through it. --user-local + --no-service keep
# the runs sudo-free; a fresh HOME per run is what makes "nothing was
# created" assertable. The success run still fails LATER, by design:
# download_binary pins --proto '=https' and so refuses the file://
# fixture. That is fine -- the assertion here is progress PAST the
# preflight (the download attempt on the same URL), not a completed
# install.

for _installer in install-mac.sh install-linux.sh; do
    # Missing asset (--computeruse against a base that publishes
    # nothing): a clean refusal before any mutation.
    _pf_home="${_tmp}/pf-404-home-${_installer}"
    mkdir -p "$_pf_home"
    _out="$(cd "$_script_dir" && HOME="$_pf_home" "./${_installer}" \
        --token mql_wkr_test --cluster https://c.example --computeruse \
        --user-local --no-service \
        --download-base "file://${_pf_assets}-none" 2>&1)"
    _rc=$?
    expect_eq "$_installer missing asset exits 4" "$_rc" "4"

    if [[ "$_out" == *"file://${_pf_assets}-none/${_pf_computeruse}"* \
        && "$_out" == *"flavour: computeruse"* && "$_out" == *"${_os}/${_arch}"* ]]; then
        pass "$_installer refusal names the asset URL + flavour/platform"
    else
        fail "$_installer refusal should name URL + flavour/platform; got: $_out"
    fi

    if [[ ! -e "${_pf_home}/.memql" && ! -e "${_pf_home}/Library" \
        && ! -e "${_pf_home}/.config" && "$_out" != *"INFO: downloading"* ]]; then
        pass "$_installer refusal mutates nothing (no ~/.memql, no service dir, no download)"
    else
        fail "$_installer refusal left state behind or attempted a download"
    fi

    # Present asset (headless at the fixture base): the preflight
    # passes and the install proceeds to the download of the SAME URL.
    _pf_home_ok="${_tmp}/pf-ok-home-${_installer}"
    mkdir -p "$_pf_home_ok"
    _out="$(cd "$_script_dir" && HOME="$_pf_home_ok" "./${_installer}" \
        --token mql_wkr_test --cluster https://c.example \
        --user-local --no-service \
        --download-base "file://${_pf_assets}" 2>&1)"
    _rc=$?
    if [[ "$_rc" != "4" && "$_out" != *"release asset not found"* ]]; then
        pass "$_installer present asset passes preflight"
    else
        fail "$_installer present asset should pass preflight; rc=$_rc got: $_out"
    fi

    if [[ "$_out" == *"INFO: downloading file://${_pf_assets}/${_pf_headless}"* ]]; then
        pass "$_installer proceeds past preflight to the download"
    else
        fail "$_installer should reach the download after preflight; got: $_out"
    fi
done

# ---------------------------------------------------------------
# Linux registration capabilities follow the runtime display preflight.
# Load only the driver's config function, preserving its unconditional main.
# Capture its writer arguments, then render real YAML to a task-specific file.
# No HOME override, network, service call or user configuration write is needed.
_linux_write_config="$(sed -n '/^function write_config()/,/^}/p' "${_script_dir}/install-linux.sh")"

function check_linux_display_config() {
    local name="$1" wayland="$2" session_type="$3" display="$4" expected="$5" flavour="${6:-computeruse}"
    local output capabilities actual
    output="$(
        eval "$_linux_write_config"
        # Called by the evaluated driver function, beyond ShellCheck's view.
        # shellcheck disable=SC2317
        function write_worker_yaml() { printf 'CAPABILITIES=%s\n' "$6"; }
        # The evaluated write_config reads these installer variables.
        # shellcheck disable=SC2034
        FLAVOUR="$flavour" CLUSTER_URL=https://c.example TOKEN=mql_wkr_fixture NAME=fixture FORCE=no
        WAYLAND_DISPLAY="$wayland" XDG_SESSION_TYPE="$session_type" DISPLAY="$display" write_config
    )"
    capabilities="$(printf '%s\n' "$output" | sed -n 's/^CAPABILITIES=//p')"
    if [[ -z "$capabilities" ]]; then
        fail "Linux ${name} did not call the config writer"
        return
    fi
    if ! write_worker_yaml "${_tmp}/display-${name}.yaml" https://c.example mql_wkr_fixture fixture no "$capabilities" >/dev/null; then
        fail "Linux ${name} config rendering failed"
        return
    fi
    actual="$(sed -n '/^capabilities:/,$p' "${_tmp}/display-${name}.yaml" | tail -n +2 | sed 's/^  - //')"
    expect_eq "Linux ${name} rendered capabilities" "$actual" "$expected"
}

check_linux_display_config wayland-with-xwayland wayland-0 wayland :0 HEADLESS
check_linux_display_config wayland-env-wins wayland-0 x11 :0 HEADLESS
check_linux_display_config padded-session "" $' \tWaYlAnD\r\n' :0 HEADLESS
check_linux_display_config native-x11 "" x11 :0 $'HEADLESS\nCOMPUTERUSE'
check_linux_display_config display-only "" "" :0 $'HEADLESS\nCOMPUTERUSE'
check_linux_display_config no-display "" "" "" HEADLESS
check_linux_display_config session-without-display "" x11 "" HEADLESS
check_linux_display_config headless-build "" x11 :0 HEADLESS headless

# ---------------------------------------------------------------
# setup_inference -- the --inference pass-through
# ---------------------------------------------------------------
#
# A machine that paired fine and could not set up local models is still
# a working worker, so setup_inference REPORTS every failure and returns
# 0. Exit 3 is the one with its own answer -- "refused: required
# confirmation not provided", which here means a runtime install a
# scripted run may not approve -- and the answer is the interactive
# command, printed for the person who is standing at this terminal now.
# A fake binary stands in for memql: what is under test is the shell's
# reading of `$?`, not the Go program's.

_si_bin="${_tmp}/fake-memql"
_si_log="${_tmp}/fake-memql.argv"
cat > "$_si_bin" << STUB
#!/usr/bin/env bash
printf '%s\n' "\$*" >> "${_si_log}"
exit "\${FAKE_MEMQL_EXIT:-0}"
STUB
chmod +x "$_si_bin"

_out="$(FAKE_MEMQL_EXIT=0 setup_inference "$_si_bin" 2>&1)"
_rc=$?
expect_eq "setup_inference success returns 0" "$_rc" "0"
expect_eq "setup_inference runs the non-interactive setup" \
    "$(tail -1 "$_si_log")" "worker setup --inference --non-interactive"

# Exit 3: print the interactive command, and do not fail the install.
_out="$(FAKE_MEMQL_EXIT=3 setup_inference "$_si_bin" 2>&1)"
_rc=$?
expect_eq "setup_inference exit 3 still returns 0 (the install succeeded)" "$_rc" "0"

# The command MUST BE ONE PHYSICAL LINE: it is meant to be copied out of
# the terminal, and a bracketed paste of a wrapped command has already
# cost this project once. grep -qx matches the WHOLE line.
if printf '%s\n' "$_out" | grep -qx "  ${_si_bin} worker setup --inference"; then
    pass "setup_inference exit 3 prints the interactive command on one line"
else
    fail "setup_inference exit 3 should print the interactive command; got: $_out"
fi

if [[ "$_out" == *"worker setup --inference --non-interactive"* ]]; then
    fail "setup_inference exit 3 must not tell the operator to re-run the flag that refused"
else
    pass "setup_inference exit 3 drops --non-interactive from the printed command"
fi

# Any other non-zero: reported, install not failed. A below-floor
# machine (4) or a failed pull (5) is not a reason to undo a working
# pairing.
for _si_code in 4 5 1; do
    _out="$(FAKE_MEMQL_EXIT="$_si_code" setup_inference "$_si_bin" 2>&1)"
    _rc=$?
    expect_eq "setup_inference exit ${_si_code} still returns 0" "$_rc" "0"
    if [[ "$_out" == *"WARN"* && "$_out" == *"exited ${_si_code}"* \
        && "$_out" == *"paired and working"* ]]; then
        pass "setup_inference exit ${_si_code} reports without failing the install"
    else
        fail "setup_inference exit ${_si_code} should report the code and say the machine still works; got: $_out"
    fi
done

# ---------------------------------------------------------------
# Both installers accept --inference and wire it after the service
# ---------------------------------------------------------------
#
# The flag itself is checked by running the real installer: --help exits
# 0 before any download, so `--inference --help` proves the flag reached
# a case arm rather than the unknown-flag branch. The CALL SITE is
# checked by reading main(), because the end-to-end path needs a
# release asset over https and these tests stay offline.

for _installer in install-mac.sh install-linux.sh; do
    if _out="$(cd "$_script_dir" && "./${_installer}" --inference --help 2>&1)" \
        && [[ "$_out" == *"Usage:"* ]]; then
        pass "$_installer accepts --inference"
    else
        fail "$_installer should accept --inference; got: $_out"
    fi

    if [[ "$_out" == *"--inference"* ]]; then
        pass "$_installer documents --inference in its help"
    else
        fail "$_installer help should document --inference"
    fi

    # The order matters: worker.yaml first (nothing to configure
    # without it), the service next (so there is a running worker for
    # the setup's SIGHUP to reach), setup_inference last.
    _line_cfg="$(grep -nF '    write_config' "${_script_dir}/${_installer}" | head -1 | cut -d: -f1)"
    _line_inf="$(grep -nF 'setup_inference "' "${_script_dir}/${_installer}" | head -1 | cut -d: -f1)"
    if [[ -n "$_line_cfg" && -n "$_line_inf" && "$_line_inf" -gt "$_line_cfg" ]]; then
        pass "$_installer runs setup_inference after worker.yaml is written"
    else
        fail "$_installer should call setup_inference after write_config (cfg=$_line_cfg inf=$_line_inf)"
    fi

    # And only when asked: a plain install must not touch the runtime.
    if grep -qF 'INFERENCE" == "yes"' "${_script_dir}/${_installer}"; then
        pass "$_installer only sets up inference when --inference was given"
    else
        fail "$_installer should guard setup_inference behind --inference"
    fi
done

# ---------------------------------------------------------------
# Uninstallers -- flags, the missing-tool path, and what is left on disk
# ---------------------------------------------------------------
#
# Driven as the installers are: real subprocesses against a throwaway
# HOME, never this machine's ~/.memql. PATH is cut down to a directory
# holding only the utilities the scripts need, which makes launchctl
# and systemctl ABSENT in every environment this runs in (a developer's
# Linux box has systemctl, CI has it too) and keeps sudo out of reach
# -- a --user-local run must never want it. Every run is checked on the
# ARTIFACTS, which files are gone and which are still there, not on
# what the script said it did.

_nobin="${_tmp}/nobin"
mkdir -p "$_nobin"
for _tool in bash dirname basename uname rm rmdir sed head ls tr cat mktemp; do
    if _real="$(command -v "$_tool")"; then
        ln -s "$_real" "${_nobin}/${_tool}"
    else
        fail "uninstall fixture: ${_tool} not on PATH"
    fi
done
if (PATH="$_nobin"; command -v systemctl || command -v launchctl || command -v sudo) >/dev/null 2>&1; then
    fail "uninstall fixture: the reduced PATH still resolves systemctl, launchctl or sudo"
else
    pass "uninstall fixture: reduced PATH has no launchctl, systemctl or sudo"
fi

# uninstall_fixture lays down what an install leaves behind, for the
# platform under test: workers.yaml + legacy worker.yaml with tokens,
# policy.yaml, a state dir with a log, a --user-local binary with its
# symlink, and the service file (plus worker.env on linux).
function uninstall_fixture() {
    local home="$1"
    local platform="$2"
    mkdir -p "${home}/.memql/state" "${home}/.memql/bin"
    printf 'cluster_url: https://c.example\ntoken: mql_wkr_fixture\nstate_dir: %s/.memql/state\n' \
        "$home" > "${home}/.memql/worker.yaml"
    # Multi-home registry (install writes this first; legacy is the mirror).
    printf 'version: 1\nworker_name: fixture\nstate_dir: %s/.memql/state\nhomes:\n  - id: c.example\n    cluster_url: https://c.example\n    token: mql_wkr_fixture\n    enabled: true\n' \
        "$home" > "${home}/.memql/workers.yaml"
    printf 'apps:\n  allow: []\n' > "${home}/.memql/policy.yaml"
    printf 'log line\n' > "${home}/.memql/state/worker.log"
    printf '#!/bin/sh\nexit 0\n' > "${home}/.memql/bin/${_pf_headless}"
    chmod +x "${home}/.memql/bin/${_pf_headless}"
    ln -s "${home}/.memql/bin/${_pf_headless}" "${home}/.memql/bin/${INSTALLED_COMMAND}"
    case "$platform" in
        mac)
            mkdir -p "${home}/Library/LaunchAgents"
            printf '<plist/>\n' > "${home}/Library/LaunchAgents/${SERVICE_LABEL_DARWIN}.plist"
            ;;
        linux)
            mkdir -p "${home}/.config/systemd/user"
            printf '[Unit]\n' > "${home}/.config/systemd/user/${SERVICE_LABEL_LINUX}.service"
            : > "${home}/.memql/worker.env"
            # A machine `memql worker setup --inference` set up: the
            # runtime's unit, its unpacked binary, and a pulled model.
            printf '[Unit]\n' > "${home}/.config/systemd/user/${OLLAMA_LABEL_LINUX}.service"
            mkdir -p "${home}/.memql/ollama/runtime/bin" "${home}/.memql/ollama/models/blobs"
            printf '#!/bin/sh\nexit 0\n' > "${home}/.memql/ollama/runtime/bin/ollama"
            : > "${home}/.memql/ollama/models/blobs/sha256-fixture"
            ;;
    esac
}

# Ordinary Linux removal exercises a successful isolated service manager;
# explicit missing-command and failed-stop cases are tested separately below.
_uninstall_systemctl_dir="${_tmp}/uninstall-systemctl"
mkdir -p "$_uninstall_systemctl_dir"
printf '#!/bin/bash\nexit 0\n' > "${_uninstall_systemctl_dir}/systemctl"
chmod +x "${_uninstall_systemctl_dir}/systemctl"

# run_uninstaller runs one uninstaller against a HOME with the reduced
# PATH, from the script dir so the sibling lib.sh is what gets sourced.
# Output (both streams) on stdout; the caller reads $? for the code.
function run_uninstaller() {
    local script="$1"
    local home="$2"
    shift 2
    local tool_path="$_nobin"
    if [[ "$script" == uninstall-linux.sh ]]; then tool_path="${_uninstall_systemctl_dir}:$_nobin"; fi
    (cd "$_script_dir" && HOME="$home" PATH="$tool_path" bash "./${script}" "$@" 2>&1)
}

for _platform in mac linux; do
    _un="uninstall-${_platform}.sh"
    case "$_platform" in
        mac)
            _svc="Library/LaunchAgents/${SERVICE_LABEL_DARWIN}.plist"
            _tool_line="INFO: launchctl not found"
            ;;
        linux)
            _svc=".config/systemd/user/${SERVICE_LABEL_LINUX}.service"
            _tool_line="INFO: stopped and disabled"
            ;;
    esac

    # --help exits 0 with usage; an unknown flag is exit 2 (bad
    # parameter), naming the flag.
    _out="$(cd "$_script_dir" && "./${_un}" --help 2>&1)"
    _rc=$?
    expect_eq "$_un --help exits 0" "$_rc" "0"
    if [[ "$_out" == *"Usage:"* && "$_out" == *"--purge"* && "$_out" == *"--user-local"* ]]; then
        pass "$_un --help documents --purge and --user-local"
    else
        fail "$_un --help should print usage with both flags; got: $_out"
    fi
    _out="$(cd "$_script_dir" && "./${_un}" --bogus 2>&1)"
    _rc=$?
    expect_eq "$_un unknown flag exits 2" "$_rc" "2"
    if [[ "$_out" == *"ERROR: unknown flag --bogus"* ]]; then
        pass "$_un unknown flag is named"
    else
        fail "$_un should name the unknown flag; got: $_out"
    fi

    # --user-local without --purge: the token, the binary + symlink, the
    # service file (and worker.env) go; policy.yaml and the state dir
    # stay, and the flag that removes them is named.
    _uh="${_tmp}/un-home-${_platform}"
    mkdir -p "$_uh"
    uninstall_fixture "$_uh" "$_platform"
    _out="$(run_uninstaller "$_un" "$_uh" --user-local)"
    _rc=$?
    expect_eq "$_un --user-local exits 0 after platform service cleanup" "$_rc" "0"
    if [[ "$_out" == *"$_tool_line"* ]]; then
        pass "$_un reports the platform service cleanup"
    else
        fail "$_un should print '$_tool_line'; got: $_out"
    fi
    if [[ ! -e "${_uh}/.memql/worker.yaml" ]]; then
        pass "$_un --user-local removes worker.yaml (the token)"
    else
        fail "$_un --user-local left worker.yaml behind"
    fi
    if [[ ! -e "${_uh}/.memql/workers.yaml" ]]; then
        pass "$_un --user-local removes workers.yaml (multi-home registry tokens)"
    else
        fail "$_un --user-local left workers.yaml behind"
    fi
    if [[ ! -e "${_uh}/.memql/bin/${INSTALLED_COMMAND}" && ! -L "${_uh}/.memql/bin/${INSTALLED_COMMAND}" \
        && ! -e "${_uh}/.memql/bin/${_pf_headless}" ]]; then
        pass "$_un --user-local removes the binary and its symlink"
    else
        fail "$_un --user-local left the binary or its symlink: $(ls -la "${_uh}/.memql/bin" 2>&1)"
    fi
    if [[ ! -e "${_uh}/${_svc}" ]]; then
        pass "$_un --user-local removes the service file"
    else
        fail "$_un --user-local left ${_svc} behind"
    fi
    if [[ "$_platform" == "linux" ]]; then
        if [[ ! -e "${_uh}/.memql/worker.env" ]]; then
            pass "$_un --user-local removes worker.env"
        else
            fail "$_un --user-local left worker.env behind"
        fi
        # The model runtime's unit goes with the worker's; the runtime and
        # its models stay without --purge, and the summary names them.
        if [[ ! -e "${_uh}/.config/systemd/user/${OLLAMA_LABEL_LINUX}.service" ]]; then
            pass "$_un removes ${OLLAMA_LABEL_LINUX}.service"
        else
            fail "$_un left ${OLLAMA_LABEL_LINUX}.service behind"
        fi
        if [[ -f "${_uh}/.memql/ollama/runtime/bin/ollama" && "$_out" == *"kept ${_uh}/.memql/ollama"* ]]; then
            pass "$_un keeps the model runtime and its models without --purge, and says so"
        else
            fail "$_un removed ~/.memql/ollama without --purge, or did not name it; got: $_out"
        fi
    fi
    if [[ -f "${_uh}/.memql/policy.yaml" && -f "${_uh}/.memql/state/worker.log" ]]; then
        pass "$_un keeps policy.yaml and the state dir without --purge"
    else
        fail "$_un removed policy.yaml or the state dir without --purge"
    fi
    if [[ "$_out" == *"--purge"* && "$_out" == *"Fleet -> Machines"* && "$_out" == *"SUCCESS:"* ]]; then
        pass "$_un names --purge, points at MemQL OS for the revoke, and closes with SUCCESS"
    else
        fail "$_un closing block is missing --purge, the revoke sentence or SUCCESS; got: $_out"
    fi

    # --user-local --purge: policy.yaml and the state dir go too, and an
    # emptied ~/.memql is removed with them.
    _ph="${_tmp}/un-purge-home-${_platform}"
    mkdir -p "$_ph"
    uninstall_fixture "$_ph" "$_platform"
    _out="$(run_uninstaller "$_un" "$_ph" --user-local --purge)"
    _rc=$?
    expect_eq "$_un --user-local --purge exits 0" "$_rc" "0"
    if [[ ! -e "${_ph}/.memql/policy.yaml" && ! -e "${_ph}/.memql/state" ]]; then
        pass "$_un --purge removes policy.yaml and the state dir"
    else
        fail "$_un --purge left policy.yaml or the state dir: $(ls -laR "${_ph}/.memql" 2>&1)"
    fi
    if [[ "$_platform" == "linux" ]]; then
        if [[ ! -e "${_ph}/.memql/ollama" ]]; then
            pass "$_un --purge removes the model runtime and its models"
        else
            fail "$_un --purge left ~/.memql/ollama: $(ls -laR "${_ph}/.memql/ollama" 2>&1)"
        fi
    fi
    if [[ ! -e "${_ph}/.memql" ]]; then
        pass "$_un --purge removes an emptied ~/.memql"
    else
        fail "$_un --purge left ~/.memql behind: $(ls -laR "${_ph}/.memql" 2>&1)"
    fi

    # A ~/.memql that still holds the CLI's clusters.yaml is KEPT under
    # --purge, and the summary names what kept it: an uninstall of the
    # worker must not sign the person out of every cluster.
    _ch="${_tmp}/un-clusters-home-${_platform}"
    mkdir -p "$_ch"
    uninstall_fixture "$_ch" "$_platform"
    printf 'clusters: []\n' > "${_ch}/.memql/clusters.yaml"
    _out="$(run_uninstaller "$_un" "$_ch" --user-local --purge)"
    _rc=$?
    expect_eq "$_un --purge beside clusters.yaml exits 0" "$_rc" "0"
    if [[ -f "${_ch}/.memql/clusters.yaml" && "$_out" == *"clusters.yaml"* ]]; then
        pass "$_un --purge keeps clusters.yaml and says it kept ~/.memql for it"
    else
        fail "$_un --purge should keep clusters.yaml and name it; got: $_out"
    fi

    # Multi-home registry only (no legacy worker.yaml): full uninstall
    # still removes workers.yaml so tokens are not left behind.
    _mh="${_tmp}/un-multi-home-${_platform}"
    mkdir -p "${_mh}/.memql"
    printf 'version: 1\nhomes:\n  - id: c.example\n    cluster_url: https://c.example\n    token: mql_wkr_only_registry\n    enabled: true\n' \
        > "${_mh}/.memql/workers.yaml"
    _out="$(run_uninstaller "$_un" "$_mh" --user-local)"
    _rc=$?
    expect_eq "$_un with workers.yaml-only exits 0" "$_rc" "0"
    if [[ ! -e "${_mh}/.memql/workers.yaml" ]]; then
        pass "$_un removes workers.yaml when legacy worker.yaml is absent"
    else
        fail "$_un left workers.yaml behind on a registry-only machine"
    fi

    # Nothing installed: every step reports nothing to remove, exit 0,
    # and no ~/.memql is created on the way.
    _eh="${_tmp}/un-empty-home-${_platform}"
    mkdir -p "$_eh"
    _out="$(run_uninstaller "$_un" "$_eh" --user-local)"
    _rc=$?
    expect_eq "$_un on a machine with nothing installed exits 0" "$_rc" "0"
    if [[ "$_out" == *"nothing to remove"* && ! -e "${_eh}/.memql" ]]; then
        pass "$_un with nothing installed reports it and creates nothing"
    else
        fail "$_un with nothing installed should say so and leave HOME untouched; got: $_out"
    fi

    # The fence: a state_dir worker.yaml points OUTSIDE ~/.memql is never
    # deleted, purge or not. That path is operator-authored text, and
    # rm -rf on it is the one thing this script must not do.
    _fh="${_tmp}/un-fence-home-${_platform}"
    _outside="${_tmp}/un-outside-${_platform}"
    mkdir -p "$_fh" "$_outside"
    uninstall_fixture "$_fh" "$_platform"
    printf 'keep me\n' > "${_outside}/precious"
    printf 'cluster_url: https://c.example\ntoken: mql_wkr_fixture\nstate_dir: %s\n' \
        "$_outside" > "${_fh}/.memql/worker.yaml"
    _out="$(run_uninstaller "$_un" "$_fh" --user-local --purge)"
    _rc=$?
    expect_eq "$_un --purge with an outside state_dir exits 0" "$_rc" "0"
    if [[ -f "${_outside}/precious" && "$_out" == *"outside"* && "$_out" == *"$_outside"* ]]; then
        pass "$_un --purge leaves a state_dir outside ~/.memql alone and names it"
    else
        fail "$_un --purge touched or failed to name the outside state_dir; got: $_out"
    fi
    if [[ ! -e "${_fh}/.memql/policy.yaml" ]]; then
        pass "$_un --purge still removes policy.yaml when the state_dir was refused"
    else
        fail "$_un --purge should still remove policy.yaml"
    fi
done

# Default (system) mode with nothing at /usr/local/bin must not reach
# for sudo: the presence check runs first, and a machine with nothing
# there is told so rather than asked for a password to find out.
# Guarded, because a developer's machine may hold a real install at
# /usr/local/bin and this test must never remove one.
_sys_present="no"
for _n in "$INSTALLED_COMMAND" "$_pf_headless" "$_pf_computeruse" $LEGACY_BINARIES; do
    if [[ -e "/usr/local/bin/${_n}" || -L "/usr/local/bin/${_n}" ]]; then
        _sys_present="yes"
    fi
done
if [[ "$_sys_present" == "yes" ]]; then
    echo "INFO: /usr/local/bin holds a memql install; skipping the system-mode no-op check"
else
    for _platform in mac linux; do
        _un="uninstall-${_platform}.sh"
        _sh="${_tmp}/un-sys-home-${_platform}"
        mkdir -p "$_sh"
        _out="$(run_uninstaller "$_un" "$_sh")"
        _rc=$?
        expect_eq "$_un system mode with nothing installed exits 0 without sudo" "$_rc" "0"
        if [[ "$_out" == *"INFO: no ${INSTALLED_COMMAND} binary at /usr/local/bin; nothing to remove"* ]]; then
            pass "$_un system mode reports the empty prefix rather than asking for sudo"
        else
            fail "$_un system mode should report nothing at /usr/local/bin; got: $_out"
        fi
    done
fi

# Exercise the real service-stop branch without reaching this host's manager.
_native_systemctl_dir="${_tmp}/native-systemctl"
mkdir -p "$_native_systemctl_dir"
cat > "${_native_systemctl_dir}/systemctl" <<'STUB'
#!/bin/bash
printf '%s\n' "$*" >> "$MEMQL_TEST_SYSTEMCTL_LOG"
if [[ "$*" == *"disable --now memql-ollama.service"* && "${MEMQL_TEST_STOP_FAIL:-}" == yes ]]; then
    exit 1
fi
exit 0
STUB
chmod +x "${_native_systemctl_dir}/systemctl"
for _stop_failure in no yes; do
    _native_home="${_tmp}/native-stop-${_stop_failure}"
    uninstall_fixture "$_native_home" linux
    _native_log="${_tmp}/native-stop-${_stop_failure}.log"
    _out="$(cd "$_script_dir" && HOME="$_native_home" PATH="${_native_systemctl_dir}:$_nobin" MEMQL_TEST_SYSTEMCTL_LOG="$_native_log" MEMQL_TEST_STOP_FAIL="$_stop_failure" bash ./uninstall-linux.sh --user-local --purge 2>&1)"
    _rc=$?
    if [[ "$_stop_failure" == no ]]; then
        expect_eq "native uninstall exits cleanly after systemd stops" "$_rc" "0"
        if grep -qF -- '--user disable --now memql-ollama.service' "$_native_log" && [[ ! -e "${_native_home}/.memql/ollama" ]]; then
            pass "native uninstall stops the runtime and purges its files"
        else
            fail "native uninstall did not stop and purge the runtime"
        fi
    else
        if [[ "$_rc" -ne 0 && "$_out" == *PARTIAL* && -e "${_native_home}/.memql/ollama/runtime/bin/ollama" && -e "${_native_home}/.config/systemd/user/memql-ollama.service" && ! -e "${_native_home}/.memql/worker.yaml" ]]; then
            pass "failed runtime stop preserves runtime and reports partial uninstall while removing token"
        else
            fail "failed runtime stop must not purge or claim success: $_out"
        fi
    fi
done

# Missing the command does not establish that an installed service stopped.
_native_missing_home="${_tmp}/native-stop-missing"
uninstall_fixture "$_native_missing_home" linux
_out="$(cd "$_script_dir" && HOME="$_native_missing_home" PATH="$_nobin" bash ./uninstall-linux.sh --user-local --purge 2>&1)"
_rc=$?
if [[ "$_rc" -ne 0 && "$_out" == *PARTIAL* && -e "${_native_missing_home}/.memql/ollama/runtime/bin/ollama" && -e "${_native_missing_home}/.config/systemd/user/memql-ollama.service" && ! -e "${_native_missing_home}/.memql/worker.yaml" ]]; then
    pass "missing systemctl preserves runtime for safe retry and removes token"
else
    fail "missing systemctl must not purge a potentially running runtime: $_out"
fi

# ---------------------------------------------------------------
# Summary
# ---------------------------------------------------------------

echo ""
echo "===================="
echo "PASS: $PASS"
echo "FAIL: $FAIL"
echo "===================="

if [[ $FAIL -gt 0 ]]; then
    exit 1
fi
exit 0
