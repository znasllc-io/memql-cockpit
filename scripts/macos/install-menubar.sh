#!/usr/bin/env bash
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
# Release archives carry the pinned runtime; developer checkouts use the sibling.
if [[ -f "$REPO_ROOT/scripts/lib/capability.sh" ]]; then
    source "$REPO_ROOT/scripts/lib/capability.sh"
else
    source "$REPO_ROOT/../memql/scripts/lib/capability.sh"
fi
cap_init "cockpit.menubar.install" "Install and launch the current user's native menu companion."
cap_spec_param "app" "Built app bundle (default: bin/MemQL Cockpit.app)"
cap_spec_param "destination" "Installed app path (default: ~/Applications/MemQL Cockpit.app)"

function main() {
    cap_handle_meta "$@"
    cap_parse_flags "$@"
    local app destination plist label uid_value stage plist_stage
    app="$(cap_param app "$REPO_ROOT/bin/MemQL Cockpit.app")"
    destination="$(cap_param destination "$HOME/Applications/MemQL Cockpit.app")"
    label="com.znasllc.memql-cockpit-menubar"
    uid_value="$(id -u)"
    [[ "$destination" == "$HOME/"* && "$destination" == *.app ]] || cap_fail 2 "destination must be a .app under the current user's home"
    [[ -x "$app/Contents/MacOS/MemQLCockpit" ]] || cap_fail 4 "build the menu bundle first"
    command -v launchctl >/dev/null || cap_fail 4 "launchctl is required on macOS"
    command -v plutil >/dev/null || cap_fail 4 "plutil is required for plist serialization"
    codesign --verify --deep --strict "$app" >&2 || cap_fail 3 "app signature verification failed"
    mkdir -p "$(dirname "$destination")" "$HOME/Library/LaunchAgents"
    if [[ ! -d "$destination" ]] || ! diff -qr "$app" "$destination" >/dev/null; then
        launchctl bootout "gui/$uid_value/$label" >/dev/null 2>&1 || true
        stage="$(mktemp -d "$(dirname "$destination")/.memql-menubar.XXXXXX")"
        ditto "$app" "$stage/MemQL Cockpit.app" >&2 || cap_fail 5 "could not stage menu bundle"
        if [[ -d "$destination" ]]; then
            mv "$destination" "$stage/previous.app"
        fi
        mv "$stage/MemQL Cockpit.app" "$destination" || cap_fail 5 "could not install menu bundle"
        # Keep the previous bundle beside the installed application for rollback.
        cap_changed
    fi
    plist="$HOME/Library/LaunchAgents/$label.plist"
    plist_stage="$(mktemp "$HOME/Library/LaunchAgents/.memql-menu.XXXXXX")"
    plutil -create xml1 "$plist_stage"
    plutil -insert Label -string "$label" "$plist_stage"
    plutil -insert ProgramArguments -array "$plist_stage"
    plutil -insert ProgramArguments.0 -string "$destination/Contents/MacOS/MemQLCockpit" "$plist_stage"
    plutil -insert RunAtLoad -bool true "$plist_stage"
    plutil -insert KeepAlive -dictionary "$plist_stage"
    plutil -insert KeepAlive.SuccessfulExit -bool false "$plist_stage"
    plutil -insert LimitLoadToSessionType -string Aqua "$plist_stage"
    plutil -insert ProcessType -string Interactive "$plist_stage"
    chmod 600 "$plist_stage"
    if [[ ! -f "$plist" ]] || ! cmp -s "$plist_stage" "$plist"; then
        launchctl bootout "gui/$uid_value/$label" >/dev/null 2>&1 || true
        mv "$plist_stage" "$plist"
        cap_changed
    else
        rm -f "$plist_stage"
    fi
    if ! launchctl print "gui/$uid_value/$label" >/dev/null 2>&1; then
        launchctl bootstrap "gui/$uid_value" "$plist" >&2 || cap_fail 5 "could not load menu LaunchAgent"
        cap_changed
    fi
    launchctl kickstart "gui/$uid_value/$label" >&2 || cap_fail 5 "could not start menu"
    cap_result_set app "$destination"
    cap_result_set launch_agent "$label"
    cap_result_set worker "unchanged; the menu controls the existing worker"
    cap_ok
}
main "$@"
