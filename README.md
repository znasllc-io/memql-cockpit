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

- **Multi-tab TUI** — DevOps, Concepts, Editor, Settings — all in one terminal; the unified Concepts tab consumes `@displayCard` hints to render rows nicely per concept
- **DSL editor + linter** — write `.memql` files with structured validation
- **Worker modes** — per-user workers bring computer use into the platform: `HEADLESS` (shell / fs / http tools) on every build, `GUI` on the gui variant
- **GUI variant** — opt-in CGO build with screenshot, mouse, keyboard, and window control via RobotGo; see [docs/computer-use.md](docs/computer-use.md)
- **Service install** — register as a LaunchAgent (macOS) or systemd user service (Linux)
- **gRPC client** — talks to memQL cluster nodes; no engine embedded

---

## Build

```bash
make cockpit          # headless variant (default, ships everywhere)
make cockpit-gui      # GUI variant with screenshot/mouse/keyboard
                      # (requires CGO + RobotGo deps -- see Makefile)
make cockpit-all-platforms       # cross-compile to darwin/linux x arm64/amd64
make cockpit-gui-all-platforms   # GUI variant, all platforms
make dist             # versioned tar.gz archives + SHA256SUMS -> dist/
```

Output lands under `bin/`. Check the build's version with
`./bin/memql-cockpit --version` (or `make version`). See
[VERSIONING.md](VERSIONING.md) for the versioning scheme (semver,
`0.9.0` baseline, git tag as source of truth) and the link to memQL's
hub compatibility matrix.

`make dist` packages the distributable artifacts operator machines and CI
runners install (one tar.gz per platform + a checksum manifest). The
**Release binaries** workflow attaches them to every GitHub Release. For
how an operator or CI runner installs the cockpit and runs a deploy — and
how the deploy control surface is provisioned with cluster creds + the
genesis envelope — see [docs/deploy-runner.md](docs/deploy-runner.md).

## Run

```bash
./bin/memql-cockpit                # main IDE (multi-tab TUI)
./bin/memql-cockpit worker run     # run as a per-user worker (HEADLESS; the gui build adds GUI)
./bin/memql-cockpit-gui worker setup  # one-time GUI worker setup wizard
```

Cluster config lives at `~/.memql/clusters.yaml`; worker config at
`~/.memql/worker.yaml`. The install scripts under `scripts/install/`
register a LaunchAgent (macOS) or systemd user service (Linux).

### Computer use

The worker's computer-use surface (`workerComputer.*`: screenshot,
mouse, keyboard, window control, displays, capabilities) is
documented in **[docs/computer-use.md](docs/computer-use.md)** --
architecture, the full 16-action vocabulary, the coordinate model,
the capability descriptor, the platform support matrix, permission
setup, and troubleshooting. The short version:

- **macOS** -- the GUI worker needs two TCC grants for the
  `memql-cockpit-gui` binary under System Settings -> Privacy &
  Security: **Accessibility** (mouse + keyboard) and **Screen
  Recording** (screenshots). `worker setup` probes both; the worker
  re-checks them on every dispatch, so a mid-session revocation
  surfaces as a structured `permission_denied` with a remediation
  hint.
- **Linux** -- RobotGo drives **X11 only**; on Wayland (or with no
  display) input + screenshot actions return
  `display_server_unsupported` -- register HEADLESS-only there
  (`install-linux.sh` detects Wayland and does this automatically).
  Building the GUI variant needs `gcc` plus the X11 dev packages
  listed in [docs/computer-use.md](docs/computer-use.md).

### Computer-use consent gate

Every `workerHost` / `workerComputer` tool call passes through a
**per-host consent gate**: without an active operator-granted window
the worker rejects the call with `consent_required`. Manage windows
over the local control socket (`~/.memql/worker.sock`, mode 0600):

```bash
memql-cockpit worker consent grant --window=1h [--strict]  # open a window
memql-cockpit worker consent revoke                        # close immediately
memql-cockpit worker consent status                        # show current state
memql-cockpit worker consent watch                         # live event tail
```

`--strict` flags the window for **per-action approval** on the
high-risk subset (`key_type`, `key_hold`, `mouse_click`, `mouse_down`,
`mouse_up`); interactive Allow/Deny enforcement previously lived in the
Workers tab (memql-cockpit#64, #130) but was removed with that tab in
memql-cockpit#216 -- worker consent is now managed via the `worker
consent` subcommands. Close a window immediately with `worker consent
revoke`. (The in-TUI Workers consent dashboard + region picker and the
global `Ctrl+E` kill switch were removed with the other panels in
memql-cockpit#216 -- consent is managed via the `worker consent`
subcommands above; the worker daemon itself is unaffected.) Details:
[docs/computer-use.md](docs/computer-use.md).

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
  (`internal/deploy/`, `internal/harness/`, `internal/lint/`,
  `internal/setupproject/`, `internal/worker/`).
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
