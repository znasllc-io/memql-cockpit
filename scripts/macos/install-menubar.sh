#!/usr/bin/env bash
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
source "$REPO_ROOT/../memql/scripts/lib/capability.sh"
cap_init "cockpit.menubar.install" "Install and launch the current user's native menu companion."
cap_spec_param "app" "Built app bundle (default: bin/MemQL Cockpit.app)"
cap_spec_param "destination" "Installed app path (default: ~/Applications/MemQL Cockpit.app)"

function main() {
    cap_handle_meta "$@"
    cap_parse_flags "$@"
    local app destination plist label uid_value stage
    app="$(cap_param app "$REPO_ROOT/bin/MemQL Cockpit.app")"
    destination="$(cap_param destination "$HOME/Applications/MemQL Cockpit.app")"
    label="com.znasllc.memql-cockpit-menubar"
    uid_value="$(id -u)"
    [[ "$destination" == "$HOME/"* && "$destination" == *.app ]] || cap_fail 2 "destination must be a .app under the current user's home"
    [[ -x "$app/Contents/MacOS/MemQLCockpit" ]] || cap_fail 4 "build the menu bundle first"
    command -v launchctl >/dev/null || cap_fail 4 "launchctl is required on macOS"
    command -v python3 >/dev/null || cap_fail 4 "python3 is required for plist serialization"
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
    python3 - "$plist" "$destination/Contents/MacOS/MemQLCockpit" "$label" <<'PY'
import os, plistlib, sys
path, executable, label = sys.argv[1:]
data = plistlib.dumps({'Label':label,'ProgramArguments':[executable],'RunAtLoad':True,'KeepAlive':{'SuccessfulExit':False},'LimitLoadToSessionType':'Aqua','ProcessType':'Interactive'})
if not os.path.exists(path) or open(path,'rb').read()!=data:
    with open(path,'wb') as f:f.write(data)
    os.chmod(path,0o600)
PY
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
