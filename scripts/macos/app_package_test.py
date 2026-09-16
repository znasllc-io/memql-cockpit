"""Exercise the shipped app installer in a temporary home, with no live agents."""
import argparse
import hashlib
import json
import os
import pathlib
import plistlib
import shutil
import subprocess
import tempfile
import tarfile
import io


def run(command, env, ok=True):
    result = subprocess.run(command, env=env, text=True, capture_output=True)
    if ok and result.returncode:
        raise AssertionError(result.stdout + result.stderr)
    return result


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--archive', required=True, type=pathlib.Path)
    archive = parser.parse_args().archive.resolve()
    repo = pathlib.Path(__file__).resolve().parents[2]
    with tempfile.TemporaryDirectory(prefix='memql-app-test-') as temp:
        root = pathlib.Path(temp)
        unpack = root / 'download'
        unpack.mkdir()
        env = dict(os.environ)
        # Exercise the same closed-layout and digest verification as install.
        run(['bash', '-c', 'source "$1"; fetch_macos_app "$2" "$3" "$4"', 'fixture',
             str(repo / 'scripts/install/lib.sh'), archive.parent.as_uri(),
             'arm64' if 'arm64' in archive.name else 'amd64', str(unpack)], env)
        source = unpack / 'unpacked'
        # A valid checksum does not authorize traversal or symlink extraction.
        for kind in ['traversal', 'symlink']:
            malicious = root / kind
            malicious.mkdir()
            bad_archive = malicious / archive.name
            with tarfile.open(bad_archive, 'w:gz') as tf:
                info = tarfile.TarInfo('MemQL.app/Contents/Resources/../escape')
                if kind == 'symlink':
                    info.name = 'MemQL.app/Contents/MacOS/MemQL'
                    info.type = tarfile.SYMTYPE
                    info.linkname = '/etc/passwd'
                else:
                    info.size = 1
                tf.addfile(info, io.BytesIO(b'x') if kind == 'traversal' else None)
            bad_archive.with_name(bad_archive.name + '.sha256').write_text(hashlib.sha256(bad_archive.read_bytes()).hexdigest() + '  ' + bad_archive.name + '\n')
            stage = malicious / 'stage'
            stage.mkdir()
            rejected = run(['bash', '-c', 'source "$1"; fetch_macos_app "$2" "$3" "$4"', 'fixture',
                            str(repo / 'scripts/install/lib.sh'), malicious.as_uri(),
                            'arm64' if 'arm64' in archive.name else 'amd64', str(stage)], env, ok=False)
            assert rejected.returncode == 3 and not (stage / 'unpacked').exists()
        user = root / 'user & space'
        user.mkdir()
        shim = root / 'shim'
        shim.mkdir()
        state = root / 'agents'
        state.mkdir()
        calls = root / 'calls'
        launch = shim / 'launchctl'
        launch.write_text('''#!/bin/bash
function main() {
    printf '%s\\n' "$*" >> "$MEMQL_TEST_CALLS"
    local label
    case "$1" in
        print)
            label="${2##*/}"
            if [[ -f "$MEMQL_TEST_STATE/$label.stopping" && "${MEMQL_TEST_STUBBORN:-}" != 1 ]]; then
                rm -f "$MEMQL_TEST_STATE/$label.stopping" "$MEMQL_TEST_STATE/$label"
                return 0 # one last visible observation before removal
            fi
            test -f "$MEMQL_TEST_STATE/$label" ;;
        bootstrap)
            label="$(/usr/libexec/PlistBuddy -c 'Print :Label' "$3")"
            [[ ! -f "$MEMQL_TEST_STATE/$label" ]] || return 5
            [[ "${MEMQL_TEST_BOOTSTRAP_FAIL:-}" != 1 ]] || return 5
            if [[ -f "$MEMQL_TEST_STATE/$label.retry" ]]; then
                rm "$MEMQL_TEST_STATE/$label.retry"; return 5
            fi
            touch "$MEMQL_TEST_STATE/$label" ;;
        bootout) label="${2##*/}"; touch "$MEMQL_TEST_STATE/$label.stopping" ;;
        unload) label="$(/usr/libexec/PlistBuddy -c 'Print :Label' "$2")"; rm -f "$MEMQL_TEST_STATE/$label" ;;
        kickstart) label="${@: -1}"; label="${label##*/}"; test -f "$MEMQL_TEST_STATE/$label" ;;
        *) return 98 ;;
    esac
}
main "$@"
''')
        launch.chmod(0o755)
        sleep = shim / 'sleep'
        sleep.write_text('#!/bin/bash\nexit 0\n')
        sleep.chmod(0o755)
        tcc = shim / 'tccutil'
        tcc.write_text('#!/bin/bash\nfunction main() { test "$#" = 3; }\nmain "$@"\n')
        tcc.chmod(0o755)

        # Some macOS versions print missing-key diagnostics to stdout. The
        # activation capability must still emit exactly one JSON result.
        plutil = shim / 'plutil'
        plutil.write_text('''#!/bin/bash
function main() {
    if [[ "$1" == -remove && "$2" == AssociatedBundleIdentifiers ]] && ! /usr/libexec/PlistBuddy -c 'Print :AssociatedBundleIdentifiers' "$3" >/dev/null 2>&1; then
        printf '%s\\n' 'No value to remove at key path AssociatedBundleIdentifiers'
        return 1
    fi
    /usr/bin/plutil "$@"
}
main "$@"
''')
        plutil.chmod(0o755)

        env.update(HOME=str(user), PATH=str(shim) + ':' + env['PATH'],
                   MEMQL_TEST_STATE=str(state), MEMQL_TEST_CALLS=str(calls))
        private = user / '.memql'
        (private / 'bin').mkdir(parents=True)
        (private / 'credentials').mkdir()
        protected = {}
        for name in ['worker.yaml', 'workers.yaml', 'policy.yaml', 'credentials/token']:
            path = private / name
            path.write_text('preserve-this-fixture-' + name)
            path.chmod(0o600)
            protected[path] = path.read_bytes()
        old_worker = private / 'bin/old-worker'
        old_worker.write_text('old rollback binary')
        cli = private / 'bin/memql'
        cli.symlink_to(old_worker)
        agents = user / 'Library/LaunchAgents'
        agents.mkdir(parents=True)
        worker_plist = agents / 'com.znasllc.memql-worker.plist'
        before = {'Label': 'com.znasllc.memql-worker', 'ProgramArguments': [str(cli), 'worker', 'run', '--log-level=debug'],
                  'RunAtLoad': True, 'KeepAlive': True, 'EnvironmentVariables': {'HOME': str(user), 'KEEP': 'yes'},
                  'StandardOutPath': str(private / 'custom.log')}
        worker_plist.write_bytes(plistlib.dumps(before))
        for label in ['com.znasllc.memql-cockpit-worker', 'com.visionarys.memql-cockpit-worker']:
            (agents / (label + '.plist')).write_bytes(plistlib.dumps({'Label': label, 'ProgramArguments': [str(old_worker)]}))
            (state / label).touch()
        legacy_menu = user / 'Applications/MemQL Cockpit.app'
        legacy_menu.parent.mkdir()
        shutil.copytree(source / 'MemQL.app/Contents/Library/LoginItems/MemQL Menu.app', legacy_menu)
        destination = user / 'Applications/MemQL.app'
        files = ['bash', str(source / 'scripts/macos/install-app-files.sh'), '--app=' + str(source / 'MemQL.app'),
                 '--destination=' + str(destination), '--cli-path=' + str(cli)]
        result = json.loads(run(files, env).stdout)
        assert result['changed'] is True
        assert cli.resolve() == (destination / 'Contents/MacOS/MemQL').resolve()
        assert old_worker.read_text() == 'old rollback binary'
        backups = list(destination.parent.glob('.memql-app-rollback.*'))
        assert any((p / 'previous-cli').is_symlink() for p in backups)
        assert json.loads(run(files, env).stdout)['changed'] is False
        assert list(destination.parent.glob('.memql-app-rollback.*')) == backups
        info = plistlib.loads((destination / 'Contents/Info.plist').read_bytes())
        assert info['CFBundleIdentifier'] == 'com.znasllc.memql-worker'
        assert info['CFBundleExecutable'] == info['CFBundleDisplayName'] == info['CFBundleName'] == 'MemQL'
        assert (destination / 'Contents/Resources' / info['CFBundleIconFile']).stat().st_size > 1000
        (state / 'com.znasllc.memql-worker').touch()
        (state / 'com.znasllc.memql-worker.retry').touch()
        activate = ['bash', str(source / 'scripts/macos/activate-app.sh'), '--app=' + str(destination)]
        assert json.loads(run(activate, env).stdout)['changed'] is True
        after = plistlib.loads(worker_plist.read_bytes())
        expected = dict(before)
        expected['ProgramArguments'] = [str(destination / 'Contents/MacOS/MemQL'), *before['ProgramArguments'][1:]]
        expected['AssociatedBundleIdentifiers'] = ['com.znasllc.memql-worker']
        assert after == expected, (after, expected)
        menu = plistlib.loads((agents / 'com.znasllc.memql-cockpit-menubar.plist').read_bytes())
        assert menu['ProgramArguments'] == [str(destination / 'Contents/Library/LoginItems/MemQL Menu.app/Contents/MacOS/MemQLCockpit')]
        assert not legacy_menu.exists()
        assert list(user.glob('Applications/.memql-legacy-menu.*/previous.app'))
        for label in ['com.znasllc.memql-cockpit-worker', 'com.visionarys.memql-cockpit-worker']:
            assert not (agents / (label + '.plist')).exists()
            assert not (state / label).exists()
        count = calls.read_text().count('bootout')
        assert json.loads(run(activate, env).stdout)['changed'] is False
        assert calls.read_text().count('bootout') == count, 'idempotent activation restarted a service'
        assert all(path.read_bytes() == data for path, data in protected.items())
        # Matching files/marker must recover an unloaded service, not kickstart it.
        (state / 'com.znasllc.memql-worker').unlink()
        assert json.loads(run(activate, env).stdout)['changed'] is True
        marker = private / 'state/app-activation.sha256'
        original_plist = worker_plist.read_bytes()
        marker.write_text('old build')
        stubborn = dict(env, MEMQL_TEST_STUBBORN='1')
        refused = run(activate, stubborn, ok=False)
        assert refused.returncode == 5 and not json.loads(refused.stdout)['ok']
        assert not marker.exists() and worker_plist.read_bytes() == original_plist
        assert all(path.read_bytes() == data for path, data in protected.items())
        failed = run(activate, dict(env, MEMQL_TEST_BOOTSTRAP_FAIL='1'), ok=False)
        assert failed.returncode == 5 and not json.loads(failed.stdout)['ok']
        assert not marker.exists()
        assert json.loads(run(activate, env).stdout)['ok']
        # A valid source does not authorize replacing an unrelated app.
        bad = root / 'unrelated/MemQL.app'
        (bad / 'Contents').mkdir(parents=True)
        (bad / 'Contents/Info.plist').write_bytes(plistlib.dumps({'CFBundleIdentifier': 'other.app'}))
        refused = run(files[:-2] + ['--destination=' + str(bad), '--cli-path=' + str(cli)], env, ok=False)
        assert refused.returncode == 3
        # Fresh service creation uses the same bundled executable, no raw CLI.
        worker_plist.unlink()
        (private / 'state/app-activation.sha256').unlink()
        assert json.loads(run(activate, env).stdout)['changed'] is True
        fresh = plistlib.loads(worker_plist.read_bytes())
        assert fresh['ProgramArguments'] == [str(destination / 'Contents/MacOS/MemQL'), 'worker', 'run']
        assert fresh['EnvironmentVariables'] == {'HOME': str(user)}
        assert fresh['AssociatedBundleIdentifiers'] == ['com.znasllc.memql-worker']
        # Full uninstall is tested only in this disposable fixture home.
        run(['bash', str(repo / 'scripts/install/uninstall-mac.sh'), '--user-local', '--all-homes'], env)
        assert not destination.exists() and not cli.exists()
        assert not (private / 'worker.yaml').exists() and not (private / 'workers.yaml').exists()
        assert (private / 'policy.yaml').read_bytes() == protected[private / 'policy.yaml']
        assert (private / 'credentials/token').read_bytes() == protected[private / 'credentials/token']
        assert backups[0].exists()
        print('PASS: app identity/icon, archive traversal/link refusal, CLI rollback, preserved service arguments/config/credentials, embedded menu, legacy cleanup, idempotence, fresh service, scoped uninstall; service calls stubbed.')


if __name__ == '__main__':
    main()
