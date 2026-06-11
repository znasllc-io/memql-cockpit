# Computer Use

This document is the contract reference for the cockpit's computer-use
surface (`workerComputer.*`): the dispatch architecture, the action
vocabulary, the coordinate model, the capability descriptor, platform
support, permission setup, and troubleshooting. It documents what
shipped in the computer-use v2 epic (memql-cockpit#161).

Source-of-truth code paths (this repo unless noted):

- Dispatcher + consent + preflight: `cmd/memql-cockpit/internal/worker/tools/dispatcher.go`, `preflight.go`, `cmd/memql-cockpit/worker/consent/consent.go`
- Action handlers: `cmd/memql-cockpit/internal/worker/tools/computer_gui.go`, `window_gui_{darwin,linux,other}.go`, `wait.go`, `capabilities.go`
- Coordinate model: `cmd/memql-cockpit/internal/worker/tools/coords.go` (the package comment is the canonical contract)
- Server-side scope policy: `integrations/agent/worker/scope.go` (memql repo)
- Descriptor validation: `component/worker/capability_descriptor.go` (memql repo)

## Architecture

A computer-use call travels from the agent runtime to the user's
machine through five gates. Each gate fails with a structured error
code rather than letting the failure surface downstream as an opaque
RobotGo error.

```
  agent runtime (memql node)
        |
        v
  memql dispatch gate                 integrations/agent/worker/dispatch.go
    - action -> required capability + scope (scope.go);
      unknown actions denied with unknown_action
    - per-task approval: dispatch must carry a PlanId from an
      approved scope-elevation Plan (requestComputerUseScope)
    - user kill switch (ComputerUseEnabled preference)
    - agent standing scope (observe / full) must satisfy the action
        |
        v
  WorkerService gRPC stream           component/grpc/worker.proto (memql)
    bidirectional stream; ToolDispatch down, ToolResult up
        |
        v
  cockpit dispatcher                  worker/tools/dispatcher.go
    - panic recovery (cgo panics become tool_panic failures)
    - consent gate (per-host operator window; consent_required
      when no window is open -- applies to EVERY action,
      including capabilities and wait)
        |
        +--> capabilities / wait      build-agnostic; routed BEFORE
        |                             preflights and the per-build router
        v
  preflight chokepoint                worker/tools/preflight.go
    - macOS: Accessibility (input actions), Screen Recording
      (screenshot) -- per-call TCC reads
    - Linux: display-server gate (input + screenshot require X11)
        |
        v
  per-build action router
    - gui build:   computerActionHandlers map (computer_gui.go),
                   RobotGo-backed handlers
    - headless:    gui_unavailable for everything except the
                   build-agnostic capabilities + wait
```

### Build variants

| Build | Tag | workerComputer surface |
|---|---|---|
| `memql-cockpit` (headless, default) | none | `capabilities` + `wait` only; every other action returns `gui_unavailable`. Registers capability `HEADLESS`. |
| `memql-cockpit-gui` | `gui` (CGO + RobotGo) | Full 16-action vocabulary. Registers `HEADLESS` + `GUI`. |

The GUI action table (`computerActionHandlers` in `computer_gui.go`)
is the single source of truth: the dispatcher routes through it AND
the capability descriptor derives its advertised action list from it,
so the two can never drift.

## Action vocabulary

16 actions. "Consent class" is the cockpit consent gate's
classification (`consent.Classify`); "Scope" is the memql-side
requirement the agent's authorization must satisfy
(`workerComputerScope` in scope.go); "Headless" marks availability on
the non-gui build.

| Action | Consent class | Scope (memql) | Headless |
|---|---|---|---|
| `capabilities` | observe | observe (1) | yes |
| `wait` | observe | observe (1) | yes |
| `screenshot` | observe | observe | no |
| `cursor_position` | observe | observe | no |
| `display_info` | observe | observe | no |
| `window_list` | observe | observe | no |
| `mouse_move` | interact | full | no |
| `mouse_click` | interact (2) | full | no |
| `mouse_down` | interact (2) | full (1) | no |
| `mouse_up` | interact (2) | full (1) | no |
| `mouse_drag` | interact | full | no |
| `mouse_scroll` | interact | full | no |
| `key_type` | interact (2) | full | no |
| `key_combo` | interact | full | no |
| `key_hold` | interact (2) | full (1) | no |
| `window_focus` | interact | full | no |

(1) The memql-side scope table is deny-by-default: an action absent
from `workerComputerScope` is rejected at the memql dispatch gate with
`unknown_action`. Classification of the v2 additions (`capabilities`,
`wait`, `key_hold`, `mouse_down`, `mouse_up`) lands via memql#1333:
`capabilities` + `wait` admit at observe against the `HEADLESS`
capability (so a headless-only worker can serve them); the three input
primitives require full against `GUI`. Until that merges, those five
are dispatchable cockpit-side but denied server-side.

(2) Strict-mode high-risk subset. Under a `--strict` consent grant
these five actions (`key_type`, `key_hold`, `mouse_click`,
`mouse_down`, `mouse_up`) additionally block on a per-call operator
Allow/Deny approval (30s timeout, default deny). `mouse_down`/
`mouse_up`/`key_hold` are in the subset because they compose into
clicks and chords -- leaving them out would let strict mode be
bypassed by decomposition. A strict grant may carry a screen region;
a `mouse_click` whose live cursor falls inside it skips the approval
modal. See the consent section below.

Note: both consent classes require an open consent window -- observe
vs interact drives the audit stream and strict-mode behavior, not the
basic gate decision. Unknown (tool, action) pairs classify as
`unknown` and are treated like interact (deny-by-default).

### Per-action arguments and results

All results are JSON objects in `Success.result_json`. Failures carry
a structured `error_code` + message (see Troubleshooting).

**`screenshot`** -- capture a display (or a region of it).

- Args: `format` (`png` default | `jpeg`), `display` (int id from
  `display_info`, default 0 = primary), `region` `{x, y, w, h}`
  (optional sub-rect in logical coords relative to the chosen
  display), `quality` (1-100, jpeg only, default 80), `maxLongEdge`
  (downscale ceiling for the emitted long edge; default 1568, clamped
  to [512, 8000]).
- Result: `{format, display, width, height, sourceWidth, sourceHeight,
  scale, logicalWidth, logicalHeight, bytesBase64, sizeBytes,
  region?}`. `width`/`height` are the emitted dims (the space the
  model speaks), `sourceWidth`/`sourceHeight` the captured dims,
  `scale` = emitted/captured, `logicalWidth`/`logicalHeight` the
  display's input-space dims.
- A `maxLongEdge` override is for inspection/zoom only: mouse-action
  mapping always assumes the default policy, so click targets must be
  derived from default-policy screenshots.

**`cursor_position`** -- no args. Result `{x, y}` in LOGICAL
virtual-desktop input coordinates (raw `robotgo.Location()`), NOT in
emitted-screenshot space.

**`mouse_move`** -- `x`, `y` (required, emitted-screenshot space of
the target display), `display` (default 0), `smooth` (bool). Result
`{x, y, display, logicalX, logicalY, smooth}`.

**`mouse_click`** -- `button` (`left` default | `right` | `middle`),
`count` (1-3, clamped; 3 posts a real triple-click), legacy `double`
bool honored as `count=2`. Clicks at the CURRENT cursor position --
position with `mouse_move` first. Result `{button, count, double}`.

**`mouse_down` / `mouse_up`** -- `button` (`left` default | `right` |
`middle`). Deliberately stateless: a `mouse_up` with no preceding
`mouse_down` is an OS-level no-op, not an error. Result `{button}`.

**`mouse_drag`** -- `fromX`, `fromY`, `toX`, `toY` (required,
emitted-screenshot space), `display` (default 0). Both endpoints must
be on the SAME display; cross-display drags are deliberately
inexpressible (the drag path across mismatched scale factors is
undefined). Result includes the mapped logical endpoints.

**`mouse_scroll`** -- `dx`, `dy` wheel-tick deltas (at least one
non-zero). Not screen coordinates; no coordinate mapping applies.
Result `{dx, dy}`.

**`key_type`** -- `text` (required). Types the string. The typed text
is REDACTED from the result, the output preview, and the audit log
(it frequently contains credentials); result is `{chars,
text_redacted: true}`.

**`key_combo`** -- `keys` array; first element is the primary key,
the rest are modifiers (e.g. `["t", "cmd"]`). Result `{keys}`.

**`key_hold`** -- `key` (required), `durationMs` (clamped to
[1, 10000]). Press, hold, release; a failed release reports
`key_hold_failed` naming the release stage (a stuck key is
operator-visible information). Result `{key, durationMs}`.

**`display_info`** -- no args. Result `{width, height, displays:
[{id, x, y, width, height, scale, primary}], primary}`. Top-level
`width`/`height` are the primary display's logical size (back-compat);
`displays` rects are in virtual-desktop logical space (origins can be
negative for displays left of / above the primary). `scale` is the
capture pixel density: the primary display's native scale (2.0 on
Retina macOS), 1.0 for secondary displays (see platform matrix).

**`window_list`** -- no args. Result `{windows: [{id, title, app,
pid, x, y, width, height, focused}]}`. Coordinates are LOGICAL
(RobotGo input space), not emitted-screenshot space -- to click
inside a window, still map through a screenshot. On macOS, `title`
for other apps' windows requires the Screen Recording permission
(empty without it; `app` is always available). `id` is the platform
window handle (CGWindowID / X window id), stable for the window's
lifetime only.

**`window_focus`** -- `windowId` (number or numeric string, from
`window_list`). Result `{focused: true, id, app, method}` where
`method` names the platform mechanism (see platform matrix).

**`capabilities`** -- no args. Returns the capability descriptor
(below). Works on every build and never runs preflights:
introspection must work even (especially) when everything else is
broken.

**`wait`** -- `ms` (clamped to [1, 5000]). Bounded, context-cancelled
sleep; the build-agnostic pacing primitive. Longer pauses are issued
as multiple waits so each stays cancellable and audit-visible. Result
`{ms}`.

## Coordinate model

Canonical contract: the package comment in
`cmd/memql-cockpit/internal/worker/tools/coords.go`.

Model/tool coordinates are ALWAYS in the coordinate space of the most
recently emitted screenshot image: `(0,0)..(emittedWidth-1,
emittedHeight-1)`. Three spaces exist on a HiDPI host and they
differ:

- **logical (input) space** -- the points RobotGo moves/clicks in;
  `robotgo.GetScreenSize()` reports this size.
- **captured (physical) space** -- the raw capture's pixel
  dimensions; 2x the logical size on a 2x Retina display.
- **emitted space** -- the captured image after the downscale policy
  (long edge clamped to 1568 px by default). This is what the model
  actually sees and therefore the space it speaks.

The worker maps emitted-space coordinates to logical input
coordinates deterministically from the current display geometry plus
the fixed downscale policy. The mapping is **stateless** and
reproducible per call: no per-screenshot state is retained between a
screenshot and the mouse action that follows it. Screenshot payloads
report `width`/`height` (emitted), `sourceWidth`/`sourceHeight`
(captured), `scale` (emitted/captured) and
`logicalWidth`/`logicalHeight` so the mapping is reversible by the
caller. Rounding is half-away-from-zero, with clamping into the
display rect so edge pixels can never map past the display.

**Downscale policy:** screenshots are downscaled (CatmullRom, never
upscaled) so the long edge does not exceed 1568 px -- the Anthropic
vision sweet spot; larger images are downscaled by the API anyway, so
shipping more pixels only bloats the payload. The per-call
`maxLongEdge` override (clamped to [512, 8000]) exists for
inspection/zoom, but mouse mapping always assumes the default policy.

**Multi-display:** `screenshot`, `mouse_move` and `mouse_drag` accept
an optional `display` arg (an id from `display_info`; default 0 =
primary). Coordinates are ALWAYS interpreted in the emitted-screenshot
space OF THAT DISPLAY -- never in a cross-display virtual space -- so
the mapping stays stateless: the worker never remembers which display
was screenshotted last. The mapper for display D maps emitted coords
into D-local logical coords, then offsets by D's origin in the
virtual desktop (negative for displays left of / above the primary).
When `display` is absent, the behavior is exactly the single-display
behavior.

**Bounds validation (`out_of_bounds`):** emitted-space points are
validated against the emitted rect, and the mapped logical point is
re-validated against the UNION of per-display rects -- deliberately
not a bounding-box check. In an L-shaped or gapped layout, a point in
a gap between monitors has no target and fails with a structured
`out_of_bounds` naming every display rect, instead of silently
landing somewhere undefined.

## Capability descriptor

The worker's structured self-description of its computer-use surface
(memql-cockpit#162). Shape (`schemaVersion` 1):

```json
{
  "platform": "darwin",
  "displayServer": "quartz",
  "guiAvailable": true,
  "actions": ["display_info", "key_combo", "...", "wait"],
  "displays": 2,
  "schemaVersion": 1
}
```

- `platform`: `runtime.GOOS`.
- `displayServer`: `quartz` (macOS) | `x11` | `wayland` | `none`,
  detected at call time. Headless builds always report `none`.
- `guiAvailable`: whether the binary was built with the gui tag.
  Consumers that want "can I actually screenshot right now" must
  check `displayServer` too (a gui build on a Wayland session reports
  `guiAvailable: true, displayServer: "wayland"`).
- `actions`: the dispatchable actions -- the GUI router table plus
  the build-agnostic `wait`, sorted. A headless build advertises
  `["wait"]`, not `[]`. The meta `capabilities` action is
  deliberately NOT listed (it is the introspection channel itself and
  is unconditionally available).
- `displays`: attached display count at call time (memql-cockpit#165);
  0 on headless builds and when no RobotGo-drivable display server is
  reachable, at least 1 otherwise. Additive field -- schemaVersion
  stays 1; the memql server tolerates unknown JSON fields.

Where it surfaces:

1. **`workerComputer.capabilities`** -- the action result, on every
   build.
2. **`Register.capability_descriptor_json`** -- the same JSON
   (identical source: `ComputeCapabilities`), sent in the
   registration handshake so the server knows the action surface up
   front without a live round-trip. The server validates it
   (memql#1331, `ParseCapabilityDescriptor`): raw JSON at most 4096
   bytes, `schemaVersion` must be exactly 1, `displayServer` must be
   one of the four enum values, `platform` and every action name must
   match `^[A-Za-z0-9._-]{1,64}$`, at most 64 actions, no duplicates.
   An invalid descriptor REJECTS the registration; an absent one is
   fine (older builds register exactly as before).
3. **`v1:worker:registration.capabilityDescriptor`** -- persisted on
   the registration concept row (memql `dsl/worker/concepts.memql`),
   projected by the `workerRegistrationFull` shape.
4. **`agentworkerListWorkers`** -- the agent-side `listWorkers` tool
   includes a `capabilityDescriptor` entry per worker when one was
   sent at registration.

## Platform support matrix

| Action group | macOS (quartz) | Linux X11 | Linux Wayland |
|---|---|---|---|
| `capabilities`, `wait` | yes | yes | yes (no display needed) |
| `screenshot` | yes (Screen Recording TCC; per-call preflight) | yes | no -- `display_server_unsupported` |
| `cursor_position`, `display_info` | yes | yes | undefined (see note 3) |
| mouse input (`mouse_*`) | yes (Accessibility TCC; per-call preflight) | yes (XTEST) | no -- `display_server_unsupported` |
| keyboard input (`key_*`) | yes (Accessibility TCC; per-call preflight) | yes (XTEST) | no -- `display_server_unsupported` |
| `window_list` | yes -- CGWindowList; titles need Screen Recording | yes -- EWMH `_NET_CLIENT_LIST` | no -- `unsupported_on_platform` |
| `window_focus` | yes -- `method: "app_activate"` (note 1) | yes -- `method: "ewmh_active_window"` | no -- `unsupported_on_platform` |
| multi-display | yes (note 2) | yes (Xinerama; single-display fallback when absent) | n/a |

Notes:

1. **Focus semantics differ by platform** -- read the result's
   `method` field. macOS v1 semantics are APP-LEVEL focus: the window
   id is resolved to its owning pid and the application is activated
   (`app_activate`), which raises that app's frontmost window; raising
   a specific non-front window within a multi-window app needs
   per-window AX targeting and is deferred. Linux/X11 is a true
   per-window raise+focus via an `_NET_ACTIVE_WINDOW` request
   (`ewmh_active_window`), honoring the window manager's
   focus-stealing policy.
2. **Secondary-display capture caveat (macOS)** -- the primary
   display captures through RobotGo's native path at physical pixels
   (Retina quality). Secondary displays capture through the
   compositing path (`robotgo.Capture`) at LOGICAL resolution, so
   their capture scale is 1.0 (and `display_info` reports
   `scale: 1.0` for them). The coordinate mapper mirrors this
   (captured == logical on secondaries), so click targets stay
   correct -- only image sharpness differs. Retina-quality secondary
   capture needs a robotgo upgrade (v1.0.2 takes raw
   CGDirectDisplayIDs, not indexes) or direct cgo.
3. **Wayland is unsupported, by detection rather than by crash.**
   Detection (`detectDisplayServer`): `WAYLAND_DISPLAY` set or
   `XDG_SESSION_TYPE=wayland` means `wayland` -- this deliberately
   WINS over a set `DISPLAY`, so an XWayland session is still gated
   (decision recorded in #164/#168: RobotGo against XWayland is
   untested; if it proves workable this could soften to a warning).
   Input + screenshot fail the preflight with a structured
   `display_server_unsupported`; `window_list`/`window_focus` do
   their own detection and fail with `unsupported_on_platform`;
   `capabilities` and `wait` work (and the descriptor advertises
   `displayServer: "wayland"`, `displays: 0` so consumers can refuse
   up front). `cursor_position`/`display_info` are NOT preflight-gated
   (they post no input and capture nothing); their RobotGo X11 calls
   have undefined results on Wayland -- a hard failure is caught by
   the dispatcher's panic recovery (`tool_panic`). Register Wayland
   hosts HEADLESS-only (`install-linux.sh` detects Wayland and does
   this automatically).

Windows and other platforms: no GUI backend; `window_list`/
`window_focus` return `unsupported_on_platform`
(`window_gui_other.go`), the descriptor reports
`displayServer: "none"`.

## Permission setup

### macOS (TCC)

Two grants for the `memql-cockpit-gui` binary under System Settings
-> Privacy & Security:

- **Accessibility** -- gates synthetic mouse + keyboard events
  (everything `isComputerInputAction` matches). Checked per call via
  `AXIsProcessTrusted` (prompt-free, synchronous).
- **Screen Recording** -- gates `screenshot` (and window titles in
  `window_list`). Checked per call via
  `CGPreflightScreenCaptureAccess`.

Both preflights run on EVERY dispatch, so a grant revoked mid-session
surfaces as a clean `permission_denied` with a remediation hint on
the very next call instead of a raw RobotGo error or panic.

TCC grants are per signed-binary identity, and a command-line binary
launched from Terminal INHERITS Terminal's grants. That means a probe
can succeed from your shell while the same binary, launched detached
as a LaunchAgent at login, is denied -- the LaunchAgent needs the
cockpit-gui binary's OWN entry in System Settings. The setup wizard
reports both the active probe result and the per-binary
`tccutil check` status (macOS 14.4+) so you can tell the cases apart.

### Linux

- An **X11 (Xorg) session** is required at runtime: RobotGo drives
  X11 only (XTEST input, X11 capture). `DISPLAY` must be set and the
  X authority reachable. Wayland sessions are gated (see matrix
  note 3).
- Building the GUI variant needs CGO plus the X11 dev packages
  (Debian/Ubuntu names): `libxtst-dev`, `libxinerama-dev`,
  `libxkbcommon-dev`, `libxkbcommon-x11-dev`, `libpng-dev`,
  `libx11-dev`, `libxi-dev` (the set CI installs in the `build-gui`
  lane). The matching runtime libraries must be present on the host.

### The `worker setup` wizard

`memql-cockpit-gui worker setup` is the interactive permissions
preflight. On an interactive terminal it renders a single-panel TUI
(three steps on macOS: Accessibility -> Screen Recording -> Validate;
`R` re-probes, `O` opens the right System Settings pane); without a
TTY it falls back to plain printf output with the same probe logic.

The wizard probes the ACTUAL gated operations, using the same
detection the dispatcher enforces so its verdict cannot disagree with
runtime gating:

- macOS Accessibility: a real relative cursor move (CGEventPost) plus
  the `AXIsProcessTrusted` preflight -- both must pass.
- macOS Screen Recording: `CGPreflightScreenCaptureAccess`.
- Linux: display-server detection first (Wayland/none fails here with
  the same verdict the dispatcher would return), then an XTEST cursor
  probe and a small `SaveCapture` screenshot probe.

A denied step does not block: the worker can still register
HEADLESS-only, and GUI calls fail with structured errors. The
headless binary's `worker setup` just prints guidance to install
`memql-cockpit-gui`.

## Consent gate (operator side)

Every `workerHost`/`workerComputer` dispatch passes a per-host consent
gate before anything runs (memql-cockpit#64). Without an active
operator-granted window the call fails with `consent_required` -- this
includes `capabilities` and `wait`.

The worker exposes a local control socket (`~/.memql/worker.sock`,
mode 0600; override with `MEMQL_WORKER_CONSENT_SOCKET`):

```bash
memql-cockpit worker consent grant --window=1h [--strict]  # open a window
memql-cockpit worker consent revoke                        # close immediately
memql-cockpit worker consent status                        # show current state
memql-cockpit worker consent watch                         # live event tail
```

- A second `grant` overwrites the active window (most recent decision
  wins). `revoke` also denies every pending strict-mode approval.
- **Strict mode** (`--strict`): the high-risk subset (`key_type`,
  `key_hold`, `mouse_click`, `mouse_down`, `mouse_up`) blocks on a
  per-call Allow/Deny approval, surfaced in the cockpit's Workers tab
  (F6); unanswered approvals deny after 30 seconds. Other interact
  actions (`exec`, `fs_write`, `mouse_move`, `key_combo`, ...) stay
  admitted by the standing window.
- **Region exemption** (memql-cockpit#131): a strict grant made
  through the Workers-tab picker can carry a static screen rect; a
  `mouse_click` whose live cursor is inside it skips the approval
  modal. `key_type` has no cursor coordinate so it never qualifies.
  The CLI `grant` path never sets a region.
- A global kill switch (`Ctrl+E` from any cockpit tab) revokes the
  window and cancels pending approvals.

This gate is cockpit-side and operator-facing. It is independent of
the memql-side gates (per-task Plan approval, user kill-switch
preference, agent scope), which run before the call ever reaches the
worker.

## Troubleshooting

| Error code | Cause | Fix |
|---|---|---|
| `consent_required` | No active consent window on the worker host (or the window expired). | On the worker host: `memql-cockpit worker consent grant --window=<duration>`, or grant from the Workers tab (F6). |
| `permission_denied` | macOS TCC grant missing or revoked: Accessibility (input actions) or Screen Recording (screenshot). Checked per call, so mid-session revocation surfaces immediately. | System Settings -> Privacy & Security -> Accessibility / Screen Recording: enable `memql-cockpit-gui` (the binary itself, not just Terminal, for LaunchAgent use). Re-run `memql-cockpit-gui worker setup` to verify. |
| `display_server_unsupported` | Linux session is Wayland (`WAYLAND_DISPLAY`/`XDG_SESSION_TYPE` -- wins even when `DISPLAY` is set) or has no display server at all; RobotGo drives X11 only. | Log into an X11 (Xorg) session, or register the worker HEADLESS-only (`install-linux.sh` does this automatically on Wayland). |
| `out_of_bounds` | Coordinates outside the emitted screenshot rect, or the mapped logical point falls in a gap between displays (union-of-rects validation). The message names the valid rect(s). | Take a fresh default-policy `screenshot` of the target display and derive coordinates from THAT image; pass the same `display` arg on the mouse action. Don't target dead zones between monitors. |
| `gui_unavailable` | The connected worker is the headless build; only `capabilities` + `wait` exist there. | Install/run `memql-cockpit-gui` on the host (`make cockpit-gui`), then `worker setup` + `worker run`. Check `capabilities` first: `guiAvailable: false` means this build. |
| `window_not_found` | The `windowId` is not in the current enumeration -- window ids change as windows open/close. | Re-run `window_list` and use a fresh id. |
| `unsupported_on_platform` | `window_list`/`window_focus` on Wayland or a platform without a backend (no portable Wayland window-enumeration API exists). | Use an X11 session (Linux) or macOS; check the descriptor's `displayServer` before calling. |
| `unknown_action` (memql side) | The action is not in the memql scope table (`scope.go`) -- deny-by-default for unclassified actions. | Verify the action name; if it is a newly added cockpit action, the memql-side classification must land first (see "Evolving the contract"). |
| `tool_panic` | A cgo/RobotGo path panicked (environment mismatch the preflights do not cover, e.g. RobotGo reads on Wayland). The dispatcher recovers it to keep the stream alive; the stack is on the cockpit's stderr. | Check the cockpit log for the stack; fix the environment (usually display-server related) and report a preflight gap if reproducible. |

## Evolving the contract

Adding a `workerComputer` action is a three-repo lockstep
(memql-cockpit -> memql -> bff), in that order so nothing is exposed
to agents before every gate knows about it. Reference: epic
memql-cockpit#161 (the v2 wave) for worked examples.

1. **Cockpit (this repo)** -- implement and gate:
   - Handler in `computerActionHandlers` (`computer_gui.go`) for
     GUI-backed actions, or alongside `wait`/`capabilities` for
     build-agnostic ones (dispatcher routes those before the
     preflights and the per-build router). The capability descriptor
     picks the action up automatically from the handler map.
   - Consent class: add the action explicitly to `consent.Classify`
     (observe vs interact) and decide whether it joins the
     strict-mode high-risk subset (`isHighRiskAction`) -- anything
     that composes into typed text or clicks belongs there.
   - Preflight set: extend `isComputerInputAction` (Accessibility +
     display-server gates) if it posts input; screenshot-like capture
     needs the Screen Recording gate in `preflightComputerAction`.
   - Tests on the untagged side wherever possible (the `-tags gui`
     suite does not run in CI; the gui lanes build + vet only).
2. **memql** -- classify and admit:
   - `integrations/agent/worker/scope.go` `workerComputerScope`:
     required capability (`HEADLESS` vs `GUI`) + scope tier
     (`observe` vs `full`). Until this lands the memql dispatch gate
     denies the action with `unknown_action` -- deny-by-default is
     the designed failure mode, not a bug.
   - Safety descriptor classification + the
     `v1:worker:invocation.action` description in
     `dsl/worker/concepts.memql`.
3. **bff (last)** -- expose to the frontend: extend the
   `tools.memql` action enum so the agent tool schema advertises it.
   This goes last because it is the exposure step; an action listed
   there but unknown to memql or the cockpit fails confusingly.

Descriptor shape changes: additive JSON fields do NOT bump
`schemaVersion` (the server tolerates unknown fields -- `displays`
landed this way); incompatible shape changes bump the cockpit's
`capabilitySchemaVersion` and the memql server's
`CapabilityDescriptorSchemaVersion` in lockstep, since the server
admits exactly one version.
