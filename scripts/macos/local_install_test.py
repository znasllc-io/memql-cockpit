"""Validate the exact piped local installer without touching a real home/service."""
import argparse
import json
import os
import pathlib
import plistlib
import subprocess
import tempfile
import urllib.request


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--base-url', required=True)
    parser.add_argument('--version', required=True)
    args = parser.parse_args()
    base = args.base_url.rstrip('/')
    with urllib.request.urlopen(base + '/scripts/install/install-mac.sh') as response:
        script = response.read()
    with tempfile.TemporaryDirectory(prefix='memql-full-install-') as temp:
        root = pathlib.Path(temp)
        user = root / 'fresh user & space'
        user.mkdir()
        shim = root / 'shim'
        shim.mkdir()
        # --no-service must avoid even attempting a launchctl invocation.
        launch = shim / 'launchctl'
        launch.write_text('#!/bin/bash\nfunction main() { echo forbidden-live-service-call >&2; exit 99; }\nmain "$@"\n')
        launch.chmod(0o755)
        env = {k: v for k, v in os.environ.items() if not k.startswith('MEMQL_INSTALL_')}
        env.update(HOME=str(user), PATH=str(shim) + ':' + env['PATH'],
                   MEMQL_INSTALL_RAW_BASE=base + '/scripts/install',
                   MEMQL_INSTALL_VERSION=args.version, MEMQL_INSTALL_ALLOW_LOOPBACK_HTTP='1')
        token = 'mql_wkr_fixture_only_not_a_real_enrollment'
        command = ['bash', '-s', '--', '--token', token, '--cluster', 'https://fixture.invalid',
                   '--name', 'fresh-install-fixture', '--computeruse', '--user-local', '--no-service', '--no-menu',
                   '--download-base', base + '/releases/download/v' + args.version]
        result = subprocess.run(command, input=script, env=env, cwd=root, capture_output=True)
        if result.returncode:
            raise AssertionError(result.stdout.decode() + result.stderr.decode())
        app = user / 'Applications/MemQL.app'
        info = plistlib.loads((app / 'Contents/Info.plist').read_bytes())
        assert info['CFBundleShortVersionString'] == args.version
        assert info['CFBundleIdentifier'] == 'com.znasllc.memql-worker'
        cli = user / '.memql/bin/memql'
        assert cli.resolve() == (app / 'Contents/MacOS/MemQL').resolve()
        registry = (user / '.memql/workers.yaml').read_text()
        assert token in registry and 'https://fixture.invalid' in registry
        assert not (user / 'Library/LaunchAgents').exists()
        assert b'forbidden-live-service-call' not in result.stderr
        assert token.encode() not in result.stdout and token.encode() not in result.stderr
        # The same command can enroll again without changing the installed app.
        before = (app / 'Contents/MacOS/MemQL').read_bytes()
        repeated = subprocess.run(command, input=script, env=env, cwd=root, capture_output=True)
        assert repeated.returncode == 0, repeated.stdout.decode() + repeated.stderr.decode()
        assert (app / 'Contents/MacOS/MemQL').read_bytes() == before
        print(json.dumps({'ok': True, 'version': args.version, 'tested': 'exact piped local HTTP installer, dev suffix, fresh enrollment, repeated enrollment, native bundle/CLI, no services or OS registration, no token output'}))


if __name__ == '__main__':
    main()
