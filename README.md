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

- **Multi-tab TUI** — agents, auth, client, cluster, config, editor, explorer, settings — all in one terminal
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

Output lands under `bin/`.

## Run

```bash
./bin/memql-cockpit                # main IDE (multi-tab TUI)
./bin/memql-cockpit worker run     # run as a per-user worker (computer_use_headless / computer_use_embodied)
./bin/memql-cockpit-gui worker setup  # one-time GUI worker setup wizard
```

Cluster config lives at `~/.memql/clusters.yaml`; worker config at
`~/.memql/worker.yaml`. The install scripts under `scripts/install/`
register a LaunchAgent (macOS) or systemd user service (Linux).

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
