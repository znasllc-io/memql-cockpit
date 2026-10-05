#!/usr/bin/env python3
"""Offline installer UX regressions using the real shell renderer and drivers."""
import errno
import http.server
import os
from pathlib import Path
import pty
import select
import signal
import stat
import subprocess
import tempfile
import threading
import time
import unittest

HERE = Path(__file__).resolve().parent


class InstallerUITest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="memql-installer-test-")
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.env = dict(os.environ, HOME=str(self.root), XDG_STATE_HOME=str(self.root / "state"),
                        TERM="xterm-256color", LANG="en_US.UTF-8", LC_ALL="", LC_CTYPE="",
                        COLUMNS="80")
        self.env.pop("NO_COLOR", None)

    def command(self, body):
        preamble = '''set -euo pipefail
source "$1/lib.sh"
TEST_ROOT="$2"
function install_ui_log_directory() { printf '%s/logs\\n' "$TEST_ROOT"; }
'''
        return ["bash", "-c", preamble + body, "fixture", str(HERE), str(self.root)]

    def run_ui(self, body):
        return subprocess.run(self.command(body), env=self.env, capture_output=True, text=True, timeout=10)

    def log(self):
        files = list((self.root / "logs").glob("install-*"))
        self.assertEqual(len(files), 1)
        self.assertEqual(stat.S_IMODE(files[0].stat().st_mode), 0o600)
        return files[0].read_text()

    def test_quiet_redacted_diagnostics_and_parent_state(self):
        result = self.run_ui('''
install_ui_init
function work() {
    echo 'INFO: diagnostic detail mql_wkr_secret-value' >&2
    echo 'Authorization: Bearer another-secret'
    printf '\\033[31mcolored detail\\033[0m\\n'
    INSTALL_UI_RESULT='Machine configured'
    PARENT_STATE=kept
}
install_ui_stage 'Configuring this machine' work
[[ "$PARENT_STATE" == kept ]]
''')
        self.assertEqual(result.returncode, 0, result.stderr + result.stdout)
        self.assertIn("Machine configured", result.stdout)
        self.assertNotIn("diagnostic detail", result.stdout)
        self.assertNotIn("\x1b", result.stdout)
        log = self.log()
        self.assertIn("INFO: diagnostic detail [redacted]", log)
        self.assertIn("Bearer [redacted]", log)
        self.assertNotIn("secret", log)
        self.assertNotIn("\x1b", log)
        self.assertEqual(result.stderr, "")

    def test_verbose_is_redacted_and_log_is_fully_drained(self):
        result = self.run_ui('''
INSTALL_VERBOSE=yes
install_ui_init
function work() {
    echo 'DETAIL mql_wkr_do-not-print'
    local n=0
    while [[ "$n" -lt 1500 ]]; do echo "diagnostic $n"; n=$((n + 1)); done
    echo LAST_DIAGNOSTIC
}
install_ui_stage 'Checking' work
''')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("DETAIL [redacted]", result.stdout)
        self.assertIn("LAST_DIAGNOSTIC", result.stdout)
        self.assertNotIn("do-not-print", result.stdout + self.log())
        self.assertTrue(self.log().endswith("LAST_DIAGNOSTIC\n"))

    def test_failure_inside_callback_stops_install_and_preserves_exit(self):
        result = self.run_ui('''
install_ui_init
function cleanup_native_stage() { echo 'cleanup ran'; }
function work() {
    echo 'download details' >&2
    bash -c 'exit 7'
    echo MUST_NOT_RUN
}
install_ui_stage 'Downloading Cockpit' work
echo MUST_NOT_CONTINUE
''')
        self.assertEqual(result.returncode, 7)
        self.assertIn("Could not finish: Downloading Cockpit", result.stdout)
        self.assertIn("Details: ", result.stdout)
        self.assertIn("--verbose", result.stdout)
        self.assertNotIn("download details", result.stdout)
        self.assertNotIn("MUST_NOT", result.stdout + self.log())
        self.assertIn("cleanup ran", self.log())

    def test_optional_models_never_turn_install_success_into_failure(self):
        for code, label in ((0, "Local models ready"), (3, "Local models need your approval"),
                            (5, "Local model setup needs attention")):
            with self.subTest(code=code):
                binary = self.root / "fake memql"
                binary.write_text(f'#!/bin/bash\nif [[ "$1" == --version ]]; then echo "memql 0.16.0 (headless)"; else echo runtime-detail >&2; exit {code}; fi\n')
                binary.chmod(0o700)
                result = self.run_ui('''
install_ui_init
INSTALLED_BINARY="$TEST_ROOT/fake memql"
INSTALL_SERVICE_STATE=started
install_ui_stage 'Preparing local models' setup_inference "$INSTALLED_BINARY"
install_ui_finish
''')
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertIn(label, result.stdout)
                self.assertIn("Cockpit v0.16.0 is installed.", result.stdout)
                self.assertNotIn("runtime-detail", result.stdout)
                if code:
                    self.assertIn("fake\\ memql worker setup --inference", result.stdout)
                    self.assertNotIn("--non-interactive", result.stdout)

    def test_service_not_started_is_honest(self):
        result = self.run_ui('''
install_ui_init
function read_binary_version() { echo 0.16.0; }
INSTALLED_BINARY=/fixture/memql
install_ui_finish
''')
        self.assertEqual(result.returncode, 0)
        self.assertIn("Worker not started", result.stdout)
        self.assertNotIn("Worker started", result.stdout)

    def test_real_drivers_preflight_stays_quiet_and_changes_no_worker_state(self):
        for driver in ("install-mac.sh", "install-linux.sh"):
            with self.subTest(driver=driver):
                result = subprocess.run(
                    ["bash", str(HERE / driver), "--token", "mql_wkr_fixture", "--cluster", "https://fixture.example",
                     "--download-base", "file:///nonexistent-memql-assets", "--user-local", "--no-service", "--plain"],
                    env=self.env, capture_output=True, text=True, timeout=10)
                self.assertEqual(result.returncode, 4, result.stdout + result.stderr)
                self.assertIn("Could not finish: Checking the release", result.stdout)
                self.assertNotIn("curl:", result.stdout + result.stderr)
                self.assertNotIn("flavour:", result.stdout)
                self.assertFalse((self.root / ".memql").exists())
                self.assertFalse((self.root / "Library/LaunchAgents").exists())
                self.assertFalse((self.root / ".config/systemd").exists())

    def test_real_drivers_fresh_and_current_with_deferred_models(self):
        class Asset(http.server.BaseHTTPRequestHandler):
            def do_HEAD(self):
                self.send_response(200)
                self.end_headers()

            def do_GET(self):
                self.do_HEAD()
                self.wfile.write(b'''#!/bin/bash
if [[ "$1" == --version ]]; then echo 'memql 0.16.0 (headless)'; exit 0; fi
echo 'runtime approval needed' >&2
exit 3
''')

            def log_message(self, *_args):
                pass

        server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Asset)
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        try:
            for driver in ("install-mac.sh", "install-linux.sh"):
                fixture_home = self.root / driver
                fixture_home.mkdir()
                env = dict(self.env, HOME=str(fixture_home), MEMQL_INSTALL_VERSION="0.16.0",
                           MEMQL_INSTALL_ALLOW_LOOPBACK_HTTP="1")
                command = ["bash", str(HERE / driver), "--token", "mql_wkr_fixture",
                           "--cluster", "https://fixture.example", "--user-local", "--no-service",
                           "--inference", "--download-base", f"http://127.0.0.1:{server.server_port}"]
                for attempt in ("fresh", "current"):
                    with self.subTest(driver=driver, attempt=attempt):
                        result = subprocess.run(command, env=env, capture_output=True, text=True, timeout=10)
                        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
                        self.assertIn("Cockpit v0.16.0 is installed", result.stdout)
                        self.assertIn("Worker not started", result.stdout)
                        self.assertIn("Local models need your approval", result.stdout)
                        self.assertNotIn("runtime approval needed", result.stdout)
                        self.assertNotIn("mql_wkr_fixture", result.stdout + result.stderr)
                        if attempt == "current":
                            self.assertIn("is up to date", result.stdout)
                        registry = (fixture_home / ".memql/workers.yaml").read_text()
                        self.assertEqual(registry.count("token:"), 1)
                        self.assertIn("https://fixture.example", registry)
                        self.assertEqual(result.stderr, "")
        finally:
            server.shutdown()
            server.server_close()
            thread.join()

    def run_pty(self, body, interrupt=False):
        master, slave = pty.openpty()
        proc = subprocess.Popen(self.command(body), env=self.env, stdin=slave, stdout=slave,
                                stderr=slave, start_new_session=True)
        os.close(slave)
        output = b""
        deadline = time.monotonic() + 8
        sent = False
        try:
            while time.monotonic() < deadline:
                if interrupt and b"\x1b[?25l" in output and not sent:
                    os.killpg(proc.pid, signal.SIGTERM)
                    sent = True
                ready, _, _ = select.select([master], [], [], .1)
                if ready:
                    try:
                        data = os.read(master, 65536)
                        if not data:
                            break
                        output += data
                    except OSError as exc:
                        if exc.errno != errno.EIO:
                            raise
                        break
                elif proc.poll() is not None:
                    break
            proc.wait(timeout=2)
        finally:
            if proc.poll() is None:
                os.killpg(proc.pid, signal.SIGKILL)
                proc.wait()
            os.close(master)
        return proc.returncode, output.decode()

    def test_tty_animation_and_interrupt_restore_cursor(self):
        rc, output = self.run_pty('''
install_ui_init
function work() { sleep 5; }
install_ui_stage 'Installing Cockpit' work
''', interrupt=True)
        self.assertEqual(rc, 143, output)
        self.assertIn("\x1b[?25l", output)
        self.assertIn("\x1b[?25h", output)
        self.assertIn("Installation interrupted", output)
        self.assertNotIn("is installed", output)

    def test_tty_logo_and_plain_fallback(self):
        for plain in ("no", "yes"):
            with self.subTest(plain=plain):
                rc, output = self.run_pty(f'''
INSTALL_PLAIN={plain}
install_ui_init
function work() {{ sleep 0.4; }}
install_ui_stage 'Checking' work
''')
                self.assertEqual(rc, 0, output)
                self.assertIn("MemQL Cockpit", output)
                if plain == "yes":
                    self.assertNotIn("\x1b", output)
                    self.assertNotIn("⣿", output)
                else:
                    self.assertIn("⣿", output)
                    self.assertIn("●", output)
                    self.assertIn("\x1b[?25h", output)

    def test_terminal_preferences_and_narrow_width(self):
        for preference in ("no-color", "dumb", "ascii", "narrow"):
            with self.subTest(preference=preference):
                original = self.env.copy()
                self.env.update({
                    "no-color": {"NO_COLOR": "1"},
                    "dumb": {"TERM": "dumb"},
                    "ascii": {"LC_ALL": "C"},
                    "narrow": {"COLUMNS": "40"},
                }[preference])
                try:
                    rc, output = self.run_pty('install_ui_init\n')
                    self.assertEqual(rc, 0, output)
                    self.assertIn("MemQL Cockpit", output)
                    if preference in ("no-color", "dumb"):
                        self.assertNotIn("\x1b", output)
                    if preference != "no-color":
                        self.assertNotIn("⣿", output)
                finally:
                    self.env = original

    @unittest.skipIf(os.geteuid() == 0, "root does not request sudo")
    def test_password_prompt_visible_outside_diagnostics(self):
        rc, output = self.run_pty('''
function sudo() {
    if [[ "$1" == -v ]]; then echo 'PASSWORD PROMPT' >&2; return 0; fi
    return 1
}
install_ui_init
install_ui_stage 'Installing Cockpit' require_sudo
''')
        self.assertEqual(rc, 0, output)
        self.assertIn("PASSWORD PROMPT", output)
        self.assertNotIn("PASSWORD PROMPT", self.log())
        self.assertIn("\x1b[?25h", output)


if __name__ == "__main__":
    unittest.main()
