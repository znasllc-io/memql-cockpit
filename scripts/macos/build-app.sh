#!/usr/bin/env bash
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
source "$REPO_ROOT/../memql/scripts/lib/capability.sh"
cap_init "cockpit.app.build" "Bundle the actual worker and native menu as MemQL.app."
cap_spec_param "worker" "Absolute path to the built macOS computer-use worker"
cap_spec_param "output" "Absolute output .app path"
cap_spec_param "version" "Version matching the worker"

function main() {
    cap_handle_meta "$@"
    cap_parse_flags "$@"
    local worker output version stage helper actual_version minimum_os
    worker="$(cap_param worker)"
    output="$(cap_param output "$REPO_ROOT/bin/MemQL.app")"
    version="$(cap_param version "$(cat "$REPO_ROOT/VERSION")")"
    [[ "$worker" == /* && -x "$worker" ]] || cap_fail 2 "worker must name a built executable"
    [[ "$output" == /* && "$output" == *.app ]] || cap_fail 2 "output must be an absolute .app path"
    actual_version="$("$worker" --version)"
    [[ "$actual_version" == "memql $version (computeruse)" ]] || cap_fail 3 "worker version/build does not match the bundle"
    minimum_os="$(xcrun vtool -show-build "$worker" | awk '$1 == "minos" || $1 == "version" {print $2; exit}')"
    [[ "$minimum_os" =~ ^[0-9]+\.[0-9]+(\.[0-9]+)?$ ]] || cap_fail 3 "could not determine the worker's minimum macOS version"
    [[ "${minimum_os%%.*}" -ge 13 ]] || minimum_os=13.0
    stage="$(mktemp -d "${TMPDIR:-/tmp}/memql-app.XXXXXX")"
    helper="$stage/MemQL.app/Contents/Library/LoginItems/MemQL Menu.app"
    mkdir -p "$(dirname "$helper")" "$stage/MemQL.app/Contents/MacOS" "$stage/MemQL.app/Contents/Resources"
    "$REPO_ROOT/scripts/macos/build-menubar.sh" --output="$helper" --version="$version" >&2 || cap_fail 5 "menu build failed"
    cp "$worker" "$stage/MemQL.app/Contents/MacOS/MemQL"
    cp "$REPO_ROOT/native/macos/WorkerInfo.plist" "$stage/MemQL.app/Contents/Info.plist"
    cp "$helper/Contents/Resources/MemQL.icns" "$stage/MemQL.app/Contents/Resources/MemQL.icns"
    /usr/libexec/PlistBuddy -c "Set :CFBundleShortVersionString $version" "$stage/MemQL.app/Contents/Info.plist" >&2
    /usr/libexec/PlistBuddy -c "Set :CFBundleVersion $version" "$stage/MemQL.app/Contents/Info.plist" >&2
    /usr/libexec/PlistBuddy -c "Set :LSMinimumSystemVersion $minimum_os" "$stage/MemQL.app/Contents/Info.plist" >&2
    # This is an honest development signature, not upgrade-stable Developer ID.
    codesign --force --sign - "$stage/MemQL.app" >&2 || cap_fail 5 "worker bundle signing failed"
    codesign --verify --deep --strict "$stage/MemQL.app" >&2 || cap_fail 3 "bundle signature verification failed"
    mkdir -p "$(dirname "$output")"
    if [[ -d "$output" ]] && diff -qr "$stage/MemQL.app" "$output" >/dev/null; then
        cap_info "MemQL app already current"
    else
        ditto "$stage/MemQL.app" "$output" >&2 || cap_fail 5 "could not write app bundle"
        cap_changed
    fi
    rm -rf "$stage"
    cap_result_set app "$output"
    cap_result_set signing "ad-hoc; Developer ID provisioning required for stable upgrades"
    cap_ok
}
main "$@"
