#!/usr/bin/env bash
set -euo pipefail

# Native menu companion only. The worker stays a separate LaunchAgent.
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
source "$REPO_ROOT/../memql/scripts/lib/capability.sh"
cap_init "cockpit.menubar.build" "Build the native macOS menu companion."
cap_spec_param "output" "Output .app bundle (default: bin/MemQL Cockpit.app)"
cap_spec_param "version" "Display version (default: repository VERSION)"

function main() {
    cap_handle_meta "$@"
    cap_parse_flags "$@"
    local output version stage
    output="$(cap_param output "$REPO_ROOT/bin/MemQL Cockpit.app")"
    version="$(cap_param version "$(cat "$REPO_ROOT/VERSION")")"
    [[ "$output" == /* && "$output" == *.app ]] || cap_fail 2 "output must be an absolute .app path"
    command -v swiftc >/dev/null || cap_fail 4 "swiftc is required (Xcode command line tools)"
    command -v codesign >/dev/null || cap_fail 4 "codesign is required on macOS"
    stage="$(mktemp -d "${TMPDIR:-/tmp}/memql-menubar.XXXXXX")/MemQL Cockpit.app"
    mkdir -p "$stage/Contents/MacOS" "$stage/Contents/Resources"
    swiftc -target "$(uname -m)-apple-macosx13.0" -framework AppKit "$REPO_ROOT/native/macos/Mark.swift" "$REPO_ROOT/native/macos/PermissionSetup.swift" "$REPO_ROOT/native/macos/tests/PermissionSetupTests.swift" -o "$(dirname "$stage")/permission-tests" >&2 || cap_fail 5 "permission tests failed to compile"
    "$(dirname "$stage")/permission-tests" >&2 || cap_fail 5 "permission behavior checks failed"
    swiftc -O -target "$(uname -m)-apple-macosx13.0" -framework AppKit "$REPO_ROOT/native/macos/Mark.swift" "$REPO_ROOT/native/macos/PermissionSetup.swift" "$REPO_ROOT/native/macos/main.swift" -o "$stage/Contents/MacOS/MemQLCockpit" >&2 || cap_fail 5 "native menu build failed"
    cp "$REPO_ROOT/native/macos/Info.plist" "$stage/Contents/Info.plist"
    cp "$REPO_ROOT/native/macos/mark.svg" "$stage/Contents/Resources/mark.svg"
    "$stage/Contents/MacOS/MemQLCockpit" --write-iconset="$(dirname "$stage")/MemQL.iconset" >&2 || cap_fail 5 "app icon generation failed"
    iconutil -c icns "$(dirname "$stage")/MemQL.iconset" -o "$stage/Contents/Resources/MemQL.icns" >&2 || cap_fail 5 "app icon packaging failed"
    /usr/libexec/PlistBuddy -c "Set :CFBundleShortVersionString $version" "$stage/Contents/Info.plist" >&2
    /usr/libexec/PlistBuddy -c "Set :CFBundleVersion $version" "$stage/Contents/Info.plist" >&2
    "$stage/Contents/MacOS/MemQLCockpit" --check-icon >&2 || cap_fail 5 "bundled MemQL menu icon failed rendering validation"
    codesign --force --sign - "$stage" >&2 || cap_fail 5 "menu signing failed"
    mkdir -p "$(dirname "$output")"
    if [[ -d "$output" ]] && diff -qr "$stage" "$output" >/dev/null; then
        cap_info "Menu bundle already current"
    else
        ditto "$stage" "$output" >&2 || cap_fail 5 "could not write menu bundle"
        cap_changed
    fi
    rm -rf "$(dirname "$stage")"
    cap_result_set app "$output"
    cap_ok
}
main "$@"
