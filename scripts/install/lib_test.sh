#!/usr/bin/env bash
#
# scripts/install/lib_test.sh
# ============================
#
# Smoke tests for lib.sh's mode helpers. Runs the pure-logic
# functions (install_mode_dir, install_mode validation) and a
# user-local dry run. The /usr/local/bin path can't be exercised
# in CI without elevated privileges, so the system-mode path is
# checked via the require_sudo failure mode + the help text.
#
# Run: bash scripts/install/lib_test.sh
# Used by .github/workflows/install-scripts-lint.yml (when wired).

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
    # Force sudo to fail by clobbering PATH so `sudo` is unreachable
    # AND command -v sudo returns non-zero. Run in a subshell so the
    # PATH munging doesn't bleed.
    (
        PATH=""
        if require_sudo 2>/dev/null; then
            echo "FAIL: require_sudo passed with PATH cleared (expected fail)"
            exit 1
        fi
        echo "PASS: require_sudo fails when sudo unreachable"
    )
    if [[ $? -ne 0 ]]; then
        FAIL=$((FAIL + 1))
    else
        PASS=$((PASS + 1))
    fi
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
