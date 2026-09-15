# macOS menu bar companion

Cockpit 0.14.0 and later include the companion in the macOS token installer
used by MemQL OS. Both headless and computer-use installations get the menu;
`--no-menu` skips it and `--no-service` skips both LaunchAgents. Older explicitly
selected worker releases skip the companion. The menu reads the worker
LaunchAgent's executable path, supporting both `/usr/local/bin` and user-local
installs without adding a second worker.

Each release publishes `memql-menubar-darwin-arm64.tar.gz` and
`memql-menubar-darwin-amd64.tar.gz`, each with a `.sha256` sidecar. The installer
verifies the digest, allowed archive paths, code signature, and matching worker
version. Packages contain the app, its installer, and the pinned capability
runtime; a user machine does not need Swift, Python, or a source checkout.
The ordinary macOS uninstall script also removes the companion and its agent;
custom destinations and previous app backups are kept.

For development, build the worker with `make cockpit-computeruse-host`, then build and install
the current user's menu companion with `make menubar-install`. Xcode command
line tools and the repository's pinned `../memql` sibling are required. The
build/install scripts use that sibling's shared capability-script runtime.

The menu app lives at `~/Applications/MemQL Cockpit.app` and starts at login as
`com.znasllc.memql-cockpit-menubar`. Its icon uses the canonical nine-node MemQL
mark from the engine's `brand/mark.svg`, rendered as a macOS template image.
The companion does not start another worker and does not acquire TCC grants.
The existing `com.znasllc.memql-worker` LaunchAgent remains the worker.

## Connections

Each server has **Pause connection** / **Allow connecting**. Pausing cancels
only that home's stream and in-flight work; other homes keep running. The
worker writes that home's `enabled` setting atomically to `workers.yaml`,
retaining its enrollment token, other settings and comments. A paused home
stays paused across worker relaunch; a supervisor with every home paused stays
available to receive local resume commands. Duplicate enrollments for one server must be resolved in `workers.yaml` before
changing their connection policy; the menu refuses an ambiguous pause.
Menu status distinguishes paused,
pausing, connected, connecting/retrying, and an unavailable background worker.

**Quit menu bar — worker keeps running** quits only the companion. It does not
change any server policy. The companion returns at the next login, or launch
`MemQL Cockpit.app` again.

**Open MemQL OS** uses a home's explicit `os_url` setting, or the known local
`https://os.memql.localhost/` ingress for `api.memql.localhost`. It never guesses
an OS URL for another API host. `os_url` must be an HTTP(S) URL without embedded
credentials, a query string or a fragment.

## Permission reporting

The worker reports passive current-process `AXIsProcessTrusted` and
`CGPreflightScreenCaptureAccess` results on Register and every 15-second
heartbeat. The menu also queries the running worker, rather than testing its
own process. Setup in Terminal can inherit Terminal's grants and does not prove
that the detached worker has permission. Grant the installed worker executable
in macOS Privacy & Security, not the menu companion. macOS may require a worker
restart after changing a grant; the UI never promotes a denied result merely
because setup succeeded or a checkbox was clicked.

The additive wire contract is `PermissionDecision` unknown=0, granted=1,
denied=2. `PermissionStatus` fields 5/6/7 carry accessibility/screen recording/X11
states, field 8 is `checked_at`, field 9 is `probe_context=worker-process`.
`Heartbeat.permissions` is field 8. The worker emits valid protobuf wire data
against the older engine pin and also works after a generated-code pin bump.
Legacy bools are true only for measured grants. Unsupported checks remain
unknown with an explanatory detail.

Linux computer-use builds report X11 granted only after a bounded passive
`xdpyinfo` query succeeds. A failed connection or missing display reports denied;
Wayland, missing `xdpyinfo`, or a timed-out probe report unknown. macOS TCC checks
are unknown on Linux. No input event or screenshot is part of reporting.
Linux semantics have fixture tests; live Linux validation is separate.

## Guided permission setup (development; not in 0.14.0)

**Set Up Permissions…** opens a native window with separate Accessibility and
Screen Recording requests. It opens once automatically when a supported worker
first reports missing access; opening the window does not request any grant.
The user's explicit request invokes `AXIsProcessTrustedWithOptions` or
`CGRequestScreenCaptureAccess` **inside the running worker** through its private
control socket. Acknowledgment means the request was queued, not approved.
Only one OS request may be pending at a time.

**Open Settings** opens the relevant Privacy & Security pane. **Show Worker in
Finder** reveals the installed executable with symlinks resolved. macOS can
retain an entry for an older binary after an upgrade; inspect the file instead
of trusting its display name. Recovery changes to existing grants remain
explicit actions in System Settings; Cockpit does not reset TCC records.

**Restart Worker…** asks for confirmation because it interrupts active work
and every server connection. It checks the service's loaded PID and program,
then uses `launchctl kickstart -k` for the current user's worker label. It does
not rewrite enrollments or connection policy. The window waits for a fresh
report from a different PID; a new process with denied access remains denied.
Reports older than 15 seconds, unavailable workers, and unsupported probes
cannot confirm readiness. Headless and older workers show an update hint.

The browser bridge is **`memql-cockpit://permissions`**, optionally ending in
`/`, with no credentials, port, query, fragment, or action path. It only opens
the window. Fleet must gate this link on a future released companion version
and offer **Install or update Cockpit** if the handler is unavailable. Opening
the URL is never evidence of installation or granted permissions. There is no
HTTP bridge and no remote grant/restart endpoint.

Local status adds `executable`, `permission_requests` and
`permission_request_pending`. The explicit CLI equivalent is:

```sh
# Read PID from the actual service status first; substitute that PID below.
memql worker control --action=status
memql worker control --action=request-permission --permission=accessibility --pid=123
memql worker control --action=request-permission --permission=screen_recording --pid=123
```

The server rejects unknown permissions, missing/stale PIDs, unsupported builds,
and overlapping OS requests. Passive status and heartbeat paths never prompt.

### Signing and upgrades

The current release binaries are ad-hoc signed. Their designated requirements
can contain the build's code hash, so an unchanged path or filename does not
establish continuity across upgrades. A fixed identifier added to another
ad-hoc signature does not solve this. See Apple's
[code-signing requirements](https://developer.apple.com/documentation/technotes/tn3127-inside-code-signing-requirements)
and [DTS confirmation of permission loss with ad-hoc builds](https://developer.apple.com/forums/thread/819406).

Distribution signing needs a provisioned **Developer ID Application** identity
and a stable worker signing identifier (planned: `com.znasllc.memql-worker`).
The menu retains `com.znasllc.memql-cockpit-menubar`. Before enabling signed
releases, provision the certificate/private key securely in CI, sign both native
architectures before hashing and packaging, verify the designated requirement
and Team ID, and validate notarization and upgrade behavior on a test Mac.
Release signing must fail if requested credentials or signature verification
are missing; it must not fall back silently to ad-hoc signing. The initial
migration from ad-hoc to Developer ID can require user approval again.

This change does not provision signing credentials or promise grant retention.
No certificate or workflow-signing configuration is modified by onboarding.

## Logs and local control

**Show Logs** opens a native window with text search, server and severity
filters, and follow/pause. It shows the latest 300 entries from a bounded tail
of the local worker log. The worker strips secrets and URL paths/queries and
admits only diagnostic attributes into structured entries. Logs remain local;
there is no upload/export endpoint. When the service is down, the CLI can read
the current user's default worker log directly for troubleshooting.

The current OS user controls their own worker. There is no cluster-role gate.
The local socket is `~/.memql/control/worker.sock`, inside an owner-only `0700`
directory, with mode `0600` and peer-UID checks on both ends. The server never
accepts arbitrary log paths. CLI equivalents:

```sh
memql worker control --action=status
memql worker control --action=logs
memql worker control --action=home --home=example --enabled=false
memql worker control --action=home --home=example --enabled=true
```

The control service refuses to share a live socket with a second worker.
Single-home CLI overrides are not controlled by the menu; use the normal
`workers.yaml` supervisor for menu controls.

## Local rollback

Keep a copy of the previous worker executable before updating it. Changing an
ad-hoc signed executable changes its code identity and may require macOS grants
to be renewed. Replacing the worker must preserve its symlink, configuration,
policy and service arguments. The menu installer changes only its own app and
LaunchAgent and leaves a previous app bundle in the staging directory beside
the destination when replacing one.

## Release inventory

The release workflow attaches a CycloneDX JSON and XML SBOM for each of the
eight worker binaries after all builds finish. Each inventory is read from the
actual artifact and includes its binary hashes, build settings, and Go modules.
The tool does not execute downloaded binaries. Relationships and licenses not
present in build metadata are not inferred. These files replace the old
module-wide SBOM job, which failed while resolving unused engine modules at
unpublished placeholder versions; they do not inventory native system libraries
or the Swift companion.
