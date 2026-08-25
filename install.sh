#!/bin/sh
set -eu

# memQL Cockpit one-line installer (headless binary; macOS + Linux, no Windows).
#
#   curl -fsSL https://raw.githubusercontent.com/znasllc-io/memql-cockpit/main/install.sh | sh
#
# Pin a version, or choose an install dir:
#   curl -fsSL .../install.sh | MEMQL_COCKPIT_VERSION=v0.9.0 sh
#   curl -fsSL .../install.sh | BIN_DIR=/usr/local/bin sh
#
# The computer-use variant (screenshot/mouse/keyboard) needs native tooling and
# is built from source -- see the README (`make cockpit-computeruse` or
# `go install -tags computeruse ...`). This installer fetches the headless build.

REPO="znasllc-io/memql-cockpit"
BINARY="memql"

log() { printf '%s\n' "$*" >&2; }
die() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }
need() { command -v "$1" >/dev/null 2>&1 || die "$1 is required but not installed"; }

detect_os() {
    case "$(uname -s)" in
        Darwin) echo darwin ;;
        Linux)  echo linux ;;
        *) die "unsupported OS '$(uname -s)' (macOS + Linux only)" ;;
    esac
}

detect_arch() {
    case "$(uname -m)" in
        x86_64|amd64)  echo amd64 ;;
        arm64|aarch64) echo arm64 ;;
        *) die "unsupported architecture '$(uname -m)'" ;;
    esac
}

resolve_version() {
    if [ -n "${MEMQL_COCKPIT_VERSION:-}" ]; then
        echo "$MEMQL_COCKPIT_VERSION"; return
    fi
    # Latest published release tag.
    curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" 2>/dev/null \
        | grep '"tag_name"' | head -1 \
        | sed -E 's/.*"tag_name": *"([^"]+)".*/\1/'
}

verify_checksum() {
    # $1=dir $2=asset $3=sumsfile
    ( cd "$1" && grep " $2\$" "$3" | \
        if command -v sha256sum >/dev/null 2>&1; then sha256sum -c -; else shasum -a 256 -c -; fi ) >/dev/null 2>&1
}

main() {
    need curl; need tar
    os="$(detect_os)"; arch="$(detect_arch)"
    tag="$(resolve_version)"
    [ -n "$tag" ] || die "could not resolve a release version (set MEMQL_COCKPIT_VERSION)"
    ver="${tag#v}"

    asset="${BINARY}-${ver}-${os}-${arch}.tar.gz"
    sums="${BINARY}-${ver}-SHA256SUMS"
    base="https://github.com/${REPO}/releases/download/${tag}"

    tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"' EXIT INT TERM

    log "==> memQL Cockpit ${tag} (${os}/${arch})"
    log "==> downloading ${asset}"
    if curl -fsSL --proto '=https' "${base}/${asset}" -o "${tmp}/${asset}" 2>/dev/null; then
        # Current format: a versioned tar.gz (make dist / release.yml) + SHA256SUMS.
        if curl -fsSL --proto '=https' "${base}/${sums}" -o "${tmp}/${sums}" 2>/dev/null; then
            log "==> verifying checksum"
            verify_checksum "$tmp" "$asset" "$sums" || die "checksum verification failed"
        else
            log "    (no ${sums} published for ${tag}; skipping checksum verify)"
        fi
        tar -xzf "${tmp}/${asset}" -C "$tmp"
        [ -f "${tmp}/${BINARY}" ] || die "archive did not contain ${BINARY}"
    else
        # Legacy format: a raw per-platform binary (memql-<os>-<arch>).
        legacy="${BINARY}-${os}-${arch}"
        log "    (no tar.gz; trying legacy raw binary ${legacy})"
        curl -fsSL --proto '=https' "${base}/${legacy}" -o "${tmp}/${BINARY}" \
            || die "no release asset for ${os}/${arch} in ${tag}"
        chmod 0755 "${tmp}/${BINARY}"
    fi

    bindir="${BIN_DIR:-${PREFIX:-$HOME/.local}/bin}"
    mkdir -p "$bindir"
    if ! install -m 0755 "${tmp}/${BINARY}" "${bindir}/${BINARY}" 2>/dev/null; then
        cp "${tmp}/${BINARY}" "${bindir}/${BINARY}" && chmod 0755 "${bindir}/${BINARY}"
    fi

    # Migrate: drop the pre-rename binaries if this machine had them.
    rm -f "${bindir}/memql-cockpit" "${bindir}/memql-cockpit-computeruse" 2>/dev/null || true

    log ""
    log "installed ${BINARY} ${tag} -> ${bindir}/${BINARY}"
    case ":${PATH}:" in
        *":${bindir}:"*) : ;;
        *) log "NOTE: ${bindir} is not on your PATH -- add it, e.g. export PATH=\"${bindir}:\$PATH\"" ;;
    esac
    log "next: ${BINARY} --version"
}

main "$@"
