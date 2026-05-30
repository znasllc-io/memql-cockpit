<p align="center">
  <img src="assets/logo.svg" alt="memQL Cockpit" width="500">
</p>

<h1 align="center">memQL Cockpit</h1>

<p align="center">
  <strong>Terminal-native IDE and operations console for memQL clusters.</strong><br>
  Multi-tab TUI with worker modes that bring computer-use into the platform.
</p>

<p align="center">
  <a href="https://github.com/znasllc-io/memql-cockpit/actions/workflows/ci.yml"><img src="https://github.com/znasllc-io/memql-cockpit/actions/workflows/ci.yml/badge.svg" alt="CI"></a>
  <a href="LICENSE"><img src="https://img.shields.io/github/license/znasllc-io/memql-cockpit?color=blue" alt="License"></a>
  <img src="https://img.shields.io/github/go-mod/go-version/znasllc-io/memql-cockpit" alt="Go version">
  <img src="https://img.shields.io/github/last-commit/znasllc-io/memql-cockpit" alt="Last commit">
  <a href="https://goreportcard.com/report/github.com/znasllc-io/memql-cockpit"><img src="https://goreportcard.com/badge/github.com/znasllc-io/memql-cockpit" alt="Go Report Card"></a>
</p>

<p align="center"><sub><em>Designed and built with Claude as co-author.</em></sub></p>

> **Status: Alpha / pre-1.0 — not production-ready.** memQL Cockpit is under active development and tracks memQL core. The TUI, worker contract, and configuration are still evolving; expect breaking changes between commits. Suitable for experimentation and early-design feedback today.

---

## What is memQL Cockpit?

memQL Cockpit is the terminal-native IDE and operations console for [memQL](https://github.com/znasllc-io/memql) clusters. It's a multi-tab TUI that gives engineers and operators one place to write, lint, and execute DSL; explore cluster state; manage identity and workers; and observe what the platform is doing in real time. It communicates with memQL clusters over gRPC (`MemqlService.Stream` and `NodeService.Stream`) and does not embed the memQL engine.

## Features

- **Multi-tab TUI** — clusters, chat, concepts, planner, settings — all in one terminal; the unified Concepts tab consumes `@displayCard` hints to render rows nicely per concept
- **DSL editor + linter** — write `.memql` files with structured validation
- **Worker modes** — `computer_use_headless` and `computer_use_embodied` bring computer use into the platform as per-user workers
- **GUI variant** — opt-in CGO build with screenshot, mouse, and keyboard via RobotGo
- **Service install** — register as a LaunchAgent (macOS) or systemd user service (Linux)
- **gRPC client** — talks to memQL cluster nodes; no engine embedded

> Demo recording (asciinema) coming soon.

---

## Build

```bash
make cockpit          # headless variant (default, ships everywhere)
make cockpit-gui      # GUI variant with screenshot/mouse/keyboard
                      # (requires CGO + RobotGo deps -- see Makefile)
make cockpit-all-platforms       # cross-compile to darwin/linux x arm64/amd64
make cockpit-gui-all-platforms   # GUI variant, all platforms
```

Output lands under `bin/`. Check the build's version with
`./bin/memql-cockpit --version` (or `make version`). See
[VERSIONING.md](VERSIONING.md) for the versioning scheme (semver,
`0.9.0` baseline, git tag as source of truth) and the link to memQL's
hub compatibility matrix.

## Run

```bash
./bin/memql-cockpit                # main IDE (multi-tab TUI)
./bin/memql-cockpit worker run     # run as a per-user worker (computer_use_headless / computer_use_embodied)
./bin/memql-cockpit-gui worker setup  # one-time GUI worker setup wizard
```

Cluster config lives at `~/.memql/clusters.yaml`; worker config at
`~/.memql/worker.yaml`. The install scripts under `scripts/install/`
register a LaunchAgent (macOS) or systemd user service (Linux).

### Computer-use consent gate

Every `workerHost` / `workerComputer` tool call dispatched against a
running worker passes through a **per-host consent gate**. Without
an active operator-granted window, the worker rejects the call with
`consent_required` -- the agent cannot drive shell / fs / mouse /
keyboard on your machine until you say so.

The worker starts a local control socket at
`~/.memql/worker.sock` (mode 0600, owner-only). Use the
`memql-cockpit worker consent ...` subcommand from a different
terminal to manage windows:

```bash
memql-cockpit worker consent grant --window=1h     # open a 1-hour window
memql-cockpit worker consent grant --window=5m     # open a 5-minute window
memql-cockpit worker consent revoke                # close immediately
memql-cockpit worker consent status                # show current state
memql-cockpit worker consent watch                 # live tail of grant/revoke/dispatch events
```

The `--strict` flag on `grant` enables **per-action approval** on
the high-risk subset (`workerComputer.key_type` +
`workerComputer.mouse_click`). When strict is on, those two actions
block on a per-call approval -- the worker emits an
`approval_requested` event over `watch`, the operator clicks Allow
or Deny in the Workers tab, and the worker either admits or rejects
the call. Approvals time out and default to deny after 30 seconds.
Revoking the consent window also cancels every pending approval.

Other ClassInteract actions (`exec`, `fs_write`, `mouse_move`,
`key_press`) stay admitted by the standing window under strict
mode -- typed text and mouse clicks are the calls the spec singles
out as load-bearing for the second consent decision.

**Region exemption.** A strict grant can carry an optional
screen-coordinate region rect. A `mouse_click` whose cursor falls
INSIDE the region is admitted without the per-action approval
modal -- the operator pre-authorised that zone of the screen.
Clicks outside the region still pop the Allow/Deny modal.
`key_type` has no cursor coordinate, so the region exemption never
applies to it -- typed text stays fully gated under strict mode.
The region is set in the Workers-tab Grant flow (see below); the
CLI `grant` path always uses plain strict mode (no region).

#### Workers tab (in-cockpit dashboard)

The Workers tab (F5) is the in-cockpit surface for the same gate.
It maintains a long-lived `watch` connection to `~/.memql/worker.sock`
and renders:

- The current consent state (granted / no consent / offline) plus
  expiry, window length, and the strict flag.
- A live tail of every worker dispatch (allowed + denied), newest
  first, capped at 256 entries.
- In-pane Grant / Revoke: `G` opens a duration picker
  (5 min / 1 hour / 8 hours), `S` toggles strict before submitting,
  `Enter` grants; `R` revokes immediately.
- **Region picker** (strict grants only): after toggling strict on,
  `Enter` opens a region picker — a schematic of the screen with a
  box you move with the arrow keys and resize with Shift+Arrows.
  `Enter` grants with that region as the in-region exemption rect;
  `N` skips the region (strict grant that gates every high-risk
  call); `Esc` steps back to the duration picker.
- **Strict-mode per-action approval**: when a strict window is open
  and the agent calls `key_type`, or a `mouse_click` outside the
  region, the worker blocks and the tab pops a modal naming the
  tool + action. Press `A` to ALLOW once, `D` to DENY. Multiple
  pending approvals queue FIFO; the modal cycles through them as
  you respond.

A global kill switch — **`Ctrl+E` from any tab** — calls the same
`revoke` op without making the user switch to the Workers tab
first. The notification feed surfaces the outcome. Revoke cancels
every pending strict-mode approval too.

Reference: memql-cockpit#64.

### Credential storage

OAuth access + refresh tokens are stored via a pluggable
`CredentialStore`. Backends:

- **OS keyring (preferred)** -- Keychain on macOS, Secret Service
  (gnome-keyring / KWallet via libsecret) on Linux, Credential
  Manager on Windows. Selected automatically when the host exposes
  a working keyring. Service name: `com.znasllc.memql-cockpit`.
- **File (fallback)** -- `~/.memql/credentials/<cluster>.json` at
  mode 0600. Used on CI runners, headless servers, and any host
  where the OS keyring can't be reached. Always available.

The cockpit logs the active backend at startup. Override with
`MEMQL_COCKPIT_CRED_STORE=file` or `MEMQL_COCKPIT_CRED_STORE=keyring`
(the latter errors out at startup when the keyring is unavailable
rather than silently falling back to disk).

To move existing on-disk tokens into the OS keyring:

```bash
memql-cockpit creds migrate-to-keyring   # idempotent; deletes source files on success
memql-cockpit creds status               # show the active backend + cached clusters
```

The cluster registry itself (`~/.memql/clusters.yaml`) still lives
on disk -- it carries the endpoint / OIDC issuer / optional PAT
needed before any keyring access. The load-time mode validator
(0600 enforced) catches drift on that file too.

## Module structure

- `cmd/memql-cockpit/` -- binary entry point + per-subcommand internals
  (`internal/authorize/`, `internal/lint/`, `internal/worker/`).
- `cli/` -- TUI primitives (`ui/`, `canvas/`) + product views
  (`agents/`, `auth/`, `client/`, `cluster/`, `config/`,
  `editor/`, `explorer/`, `settings/`).
- `scripts/install/` -- platform installers.

## memQL core dependency

This module depends on `github.com/znasllc-io/memql` for:
- `component/grpc/gen` -- generated proto types (wire surface)
- `component/node/gen` -- generated node proto types
- `component/node` -- node client / connection primitives
- `component/identity/workerpairing` -- worker pairing protocol
- `component/memql/dslimports` -- DSL import resolution (for `lint`)
- `core/id` -- canonical id validation

During local development the `replace` directive in `go.mod` points
at a sibling `../memql/` tree. Once memql core is published with a
real version tag, drop the replace and pin the version.

## License

Apache License 2.0 — see [LICENSE](LICENSE).
