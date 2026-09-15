# macOS menu bar companion

Build the worker with `make cockpit-computeruse-host`, then build and install
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
available to receive local resume commands. Menu status distinguishes paused,
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
