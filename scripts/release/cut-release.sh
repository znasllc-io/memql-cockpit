#!/bin/bash
set -euo pipefail

# cut-release.sh -- recommend the next semver from the commits since the last
# tag (conventional-commit prefixes; NO AI), then optionally cut a tag-driven
# release. Cutting bumps the VERSION file + the main.go `version` constant,
# commits, tags vX.Y.Z, pushes, and creates a GitHub Release -- which triggers
# .github/workflows/release.yml to build + upload the per-platform binaries.
#
# Usage:
#   make release                         recommend only (safe; shows next version + why)
#   make release ARGS="--cut"            cut the RECOMMENDED version (asks to confirm)
#   make release ARGS="--cut --yes"      cut it non-interactively
#   make release ARGS="--bump=minor --cut"      force a minor bump
#   make release ARGS="--version=1.2.3 --cut"   force an explicit version
#
# Recommendation rules (highest wins across the range):
#   major  <- a commit subject like `feat!:` / `fix(x)!:`, or `BREAKING CHANGE` in a body
#   minor  <- a `feat:` / `feat(scope):` subject
#   patch  <- fix/perf/refactor/docs/chore/... or anything else

#=============================================================================
# CONFIGURATION
#=============================================================================

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
VERSION_FILE="$REPO_ROOT/VERSION"
MAIN_GO="$REPO_ROOT/cmd/memql-cockpit/main.go"

#=============================================================================
# FUNCTIONS
#=============================================================================

function log() { echo "$*" >&2; }
function err() { echo "ERROR: $*" >&2; }

function parse_arguments() {
    DO_CUT=false; ASSUME_YES=false; FORCE_BUMP=""; FORCE_VERSION=""
    while [[ $# -gt 0 ]]; do
        case "$1" in
            --cut)        DO_CUT=true; shift ;;
            --yes|-y)     ASSUME_YES=true; shift ;;
            --bump=*)     FORCE_BUMP="${1#*=}"; shift ;;
            --version=*)  FORCE_VERSION="${1#*=}"; shift ;;
            --dry-run)    DO_CUT=false; shift ;;
            -h|--help)    grep '^#' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'; exit 0 ;;
            *)            err "unknown arg: $1"; exit 2 ;;
        esac
    done
}

function last_version() {
    # Latest v-prefixed semver tag, else the VERSION file, else 0.0.0.
    local t
    t="$(git -C "$REPO_ROOT" tag --sort=-v:refname 2>/dev/null | grep -E '^v[0-9]+\.[0-9]+\.[0-9]+$' | head -1 || true)"
    if [ -n "$t" ]; then echo "${t#v}"; return; fi
    cat "$VERSION_FILE" 2>/dev/null || echo "0.0.0"
}

function classify_commits() {
    # Reads commits since the base ref; prints "major", "minor", or "patch".
    local base="$1" level="patch" subjects bodies
    subjects="$(git -C "$REPO_ROOT" log "${base}..HEAD" --format='%s' 2>/dev/null || true)"
    bodies="$(git -C "$REPO_ROOT" log "${base}..HEAD" --format='%b' 2>/dev/null || true)"
    if echo "$subjects" | grep -qE '^[a-z]+(\([^)]+\))?!:' || echo "$bodies" | grep -q 'BREAKING CHANGE'; then
        level="major"
    elif echo "$subjects" | grep -qE '^feat(\([^)]+\))?:'; then
        level="minor"
    fi
    echo "$level"
}

function bump() {
    # $1=current X.Y.Z  $2=level  ->  next X.Y.Z
    local IFS='.'; read -r ma mi pa <<< "$1"
    case "$2" in
        major) echo "$((ma+1)).0.0" ;;
        minor) echo "${ma}.$((mi+1)).0" ;;
        patch) echo "${ma}.${mi}.$((pa+1))" ;;
        *) err "bad bump level: $2"; exit 2 ;;
    esac
}

function show_range() {
    local base="$1" total drivers
    total="$(git -C "$REPO_ROOT" rev-list --count "${base}..HEAD" 2>/dev/null || echo 0)"
    log ""
    log "Commits since v${CUR}: ${total}"
    drivers="$(git -C "$REPO_ROOT" log "${base}..HEAD" --format='%s' 2>/dev/null \
        | perl -ne 'chomp; if(/^[a-z]+(\([^)]+\))?!:/){print "  [MAJOR] $_\n"} elsif(/^feat(\([^)]+\))?:/){print "  [minor] $_\n"}' \
        | head -25 || true)"
    if [ -n "$drivers" ]; then
        log "Version-driving commits (feat / breaking):"; echo "$drivers" >&2
    else
        log "  (no feat/breaking commits since the last tag -> patch)"
    fi
}

function check_cut_preconditions() {
    if [ -n "$(git -C "$REPO_ROOT" status --porcelain)" ]; then
        err "working tree is dirty -- commit or stash before cutting a release"; exit 3
    fi
    local br; br="$(git -C "$REPO_ROOT" rev-parse --abbrev-ref HEAD)"
    if [ "$br" != "main" ]; then
        err "on branch '$br', not 'main' -- releases are cut from main"; exit 3
    fi
    command -v gh >/dev/null 2>&1 || { err "gh CLI required to create the release"; exit 4; }
}

function perform_cut() {
    local next="$1" tag="v$1"
    if git -C "$REPO_ROOT" rev-parse "$tag" >/dev/null 2>&1; then
        err "tag $tag already exists"; exit 3
    fi
    log "==> bumping VERSION + main.go to $next"
    echo "$next" > "$VERSION_FILE"
    perl -pi -e "s/^const version = \".*\"/const version = \"$next\"/" "$MAIN_GO"
    git -C "$REPO_ROOT" add "$VERSION_FILE" "$MAIN_GO"
    git -C "$REPO_ROOT" commit -q -m "release: $tag"
    git -C "$REPO_ROOT" tag -a "$tag" -m "$tag"
    log "==> pushing main + $tag"
    git -C "$REPO_ROOT" push origin main
    git -C "$REPO_ROOT" push origin "$tag"
    log "==> creating GitHub Release $tag (triggers release.yml build+upload)"
    gh release create "$tag" --repo "$(gh repo view --json nameWithOwner -q .nameWithOwner)" \
        --title "$tag" --generate-notes --target main
    log ""
    log "DONE: $tag cut. Watch the binaries build: gh run watch --repo \$(gh repo view --json nameWithOwner -q .nameWithOwner)"
}

function main() {
    parse_arguments "$@"
    CUR="$(last_version)"
    local base="v${CUR}"
    git -C "$REPO_ROOT" rev-parse "$base" >/dev/null 2>&1 || base="$(git -C "$REPO_ROOT" rev-list --max-parents=0 HEAD | tail -1)"  # no tag yet -> from root

    local level next
    if [ -n "$FORCE_VERSION" ]; then
        next="$FORCE_VERSION"; level="explicit"
    else
        level="${FORCE_BUMP:-$(classify_commits "$base")}"
        next="$(bump "$CUR" "$level")"
    fi

    show_range "$base"
    log ""
    log "Current:     v${CUR}"
    log "Recommended: v${next}   (${level} bump)"

    if [ "$DO_CUT" != true ]; then
        log ""
        log "(recommend-only) To cut it:  make release ARGS=\"--cut\""
        log "  override:  make release ARGS=\"--bump=minor --cut\"  |  ARGS=\"--version=X.Y.Z --cut\""
        exit 0
    fi

    check_cut_preconditions
    if [ "$ASSUME_YES" != true ]; then
        printf 'Cut release v%s now? [y/N] ' "$next" >&2
        read -r reply </dev/tty || reply=""
        case "$reply" in y|Y|yes) ;; *) log "aborted."; exit 0 ;; esac
    fi
    perform_cut "$next"
}

#=============================================================================
# ENTRY POINT
#=============================================================================

main "$@"
