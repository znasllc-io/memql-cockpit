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

# Refuses to clobber an existing file without force.
if write_worker_yaml "$_wy" "https://c.example" "mql_wkr_abc" "host1" "no" >/dev/null 2>&1; then
    fail "write_worker_yaml clobbered an existing file without --force"
else
    pass "write_worker_yaml refuses to clobber without --force"
fi

# Overwrites with force=yes and the computer-use capability set.
if write_worker_yaml "$_wy" "https://c.example" "mql_wkr_abc" "host1" "yes" "HEADLESS,COMPUTERUSE" >/dev/null 2>&1; then
    pass "write_worker_yaml overwrites with --force"
else
    fail "write_worker_yaml should overwrite with --force"
fi

if grep -qx '  - HEADLESS' "$_wy" && grep -qx '  - COMPUTERUSE' "$_wy"; then
    pass "write_worker_yaml computeruse lists HEADLESS + COMPUTERUSE"
else
    fail "write_worker_yaml computeruse should list HEADLESS + COMPUTERUSE"
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
# before any download / sudo / service work.

_script_dir="$(cd "$(dirname "$0")" && pwd)"
_piped_cwd="${_tmp}/piped-cwd"
mkdir -p "$_piped_cwd"

for _installer in install-mac.sh install-linux.sh; do
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
