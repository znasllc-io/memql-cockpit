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
- **Worker modes** — per-user workers bring computer use into the platform: `HEADLESS` (shell / fs / http tools) on every build, `COMPUTERUSE` on the computer-use variant
- **Computer-use variant** — opt-in CGO build (`-tags computeruse`) with screenshot, mouse, keyboard, and window control via RobotGo; see [docs/computer-use.md](docs/computer-use.md)
- **Service install** — register as a LaunchAgent (macOS) or systemd user service (Linux)
- **gRPC client** — talks to memQL cluster nodes; no engine embedded

---

## Install

Grab the latest release — the **headless** build, macOS + Linux (arm64/amd64),
no Windows:

```bash
# one-liner (detects OS/arch, checksum-verified)
curl -fsSL https://raw.githubusercontent.com/znasllc-io/memql-cockpit/main/install.sh | sh

# pin a version, or an install dir
curl -fsSL https://raw.githubusercontent.com/znasllc-io/memql-cockpit/main/install.sh | MEMQL_COCKPIT_VERSION=v0.10.0 sh
curl -fsSL https://raw.githubusercontent.com/znasllc-io/memql-cockpit/main/install.sh | BIN_DIR=/usr/local/bin sh
```

Or, if you have Go (builds from source):

```bash
go install github.com/znasllc-io/memql-cockpit/cmd/memql-cockpit@latest      # or @vX.Y.Z
```

Both give you the headless build. The **computer-use** variant (screenshot /
mouse / keyboard) needs native tooling and is built from source — `make
cockpit-computeruse`, or `go install -tags computeruse
github.com/znasllc-io/memql-cockpit/cmd/memql-cockpit@latest` (see
[docs/computer-use.md](docs/computer-use.md)). Releases are cut with
[`make release`](#releasing).

## Build

The Cockpit ships in two build variants:

- **headless** (default) — pure Go, `CGO_ENABLED=0`; ships everywhere.
  The worker exposes the `HEADLESS` tool surface (shell / fs / http).
- **computer-use** (`-tags computeruse`, CGO + RobotGo) — adds the
  `workerComputer.*` control surface (screenshot / mouse / keyboard /
  window) and registers the `COMPUTERUSE` capability. Here "computer-use"
  is a machine-control surface, **not** a graphical UI. It needs native
  tooling (macOS Xcode CLT; Linux `gcc` + X11 dev headers), is built
  per-host, and is deliberately **not** shipped in `dist`. See
  [docs/computer-use.md](docs/computer-use.md).

```bash
make cockpit                            # headless variant (default, ships everywhere)
make cockpit-computeruse                # computer-use variant (CGO + RobotGo; adds workerComputer.*)
make cockpit-all-platforms              # cross-compile headless -> darwin/linux x arm64/amd64
make cockpit-computeruse-all-platforms  # computer-use variant, all platforms
make dist                               # versioned tar.gz archives + SHA256SUMS -> dist/ (headless only)
```

A full, auto-generated target list (with the `ARGS=` / `CRED_STORE=`
knobs) is always one `make help` away; the [Command reference](#command-reference)
below summarizes the day-to-day set.

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

The `make run*` family builds the binary and launches the Cockpit; the
members differ only in **where/how** they connect. Extra flags reach the
binary via `ARGS=...` (e.g. `make run ARGS="--cluster staging"`).

Against your **active cluster** — the current entry in
`~/.memql/clusters.yaml`, or an explicit `--endpoint`:

```bash
make run                            # headless, active clusters.yaml cluster
make run ARGS="--cluster staging"   # pick a named cluster from clusters.yaml
make run-computeruse                # same connection, but the computer-use variant
```

Against a **local k3d cluster** (engine brought up with `make up` in the memql
repo), one command builds and connects — it auto port-forwards the engine `bff`
node (the product-agnostic client edge) and launches the Cockpit against it, then
tears the forward down on exit:

```bash
make run-local                     # build + auto port-forward svc/bff + launch (guards against a non-local kube-context)
make forward                       # port-forward the bff edge only (for SDKs / other clients)
```

The port-forward is **local access config only**. In staging/prod the Cockpit
reaches the *same* bff node via the `cockpit.<domain>` front-door ingress, so
there you connect with a named cluster instead — same binary, config-driven:

```bash
./bin/memql-cockpit --cluster staging          # endpoint from ~/.memql/clusters.yaml
./bin/memql-cockpit --endpoint <host>          # or an explicit gRPC endpoint
./bin/memql-cockpit worker run                 # run as a per-user worker (HEADLESS; the computer-use build adds COMPUTERUSE)
./bin/memql-cockpit-computeruse worker setup   # one-time computer-use worker setup wizard
```

Cluster config lives at `~/.memql/clusters.yaml`; worker config at
`~/.memql/worker.yaml`. The install scripts under `scripts/install/`
register a LaunchAgent (macOS) or systemd user service (Linux).

### Command reference

Every target is a thin wrapper over the binary or a helper script; the
`make help` output is auto-generated from the same comments. The knobs
in the third column are Make variables — pass them inline, e.g.
`make run-local BFF_PORT=50051 CRED_STORE=keyring`.

| Target | Purpose | Key ARGS / env | Example |
|---|---|---|---|
| `make cockpit` | Build the headless binary → `bin/memql-cockpit` | — | `make cockpit` |
| `make cockpit-computeruse` | Build the computer-use binary → `bin/memql-cockpit-computeruse` (CGO + RobotGo) | — | `make cockpit-computeruse` |
| `make cockpit-all-platforms` | Cross-build headless for darwin/linux × arm64/amd64 | — | `make cockpit-all-platforms` |
| `make cockpit-computeruse-all-platforms` | Cross-build the computer-use variant for all platforms | — | `make cockpit-computeruse-all-platforms` |
| `make run` | Build + run headless against the active `clusters.yaml` cluster | `ARGS`, `CRED_STORE` | `make run ARGS="--cluster staging"` |
| `make run-computeruse` | Build + run the computer-use variant against the active cluster | `ARGS`, `CRED_STORE` | `make run-computeruse` |
| `make run-local` | Build + run against a **local k3d** cluster, auto port-forwarding `svc/bff` (guards the kube-context) | `ARGS`, `CRED_STORE`, `MEMQL_NS`, `BFF_SVC`, `BFF_PORT`, `MEMQL_ALLOW_CONTEXT` | `make run-local BFF_PORT=50051` |
| `make forward` | Port-forward `svc/bff` only — no launch (for SDKs / other clients) | `MEMQL_NS`, `BFF_SVC`, `BFF_PORT` | `make forward BFF_PORT=50051` |
| `make dist` | Package versioned `tar.gz` + `SHA256SUMS` into `dist/` (headless, all platforms) | — | `make dist` |

The knobs:

- **`ARGS`** — extra flags forwarded verbatim to the cockpit binary
  (e.g. `--cluster <name>`, `--endpoint <host>`, `worker run`).
- **`CRED_STORE`** — credential backend for `make`-launched runs:
  `file` (default; tokens in `~/.memql/`, mode 0600) or `keyring`. Make
  defaults to `file` because a locally-built, unsigned binary makes the
  macOS Keychain re-prompt on every rebuild; the shipped binary's own
  default is unchanged (auto-probe → Keychain). See
  [Credential storage](#credential-storage).
- **`MEMQL_NS` / `BFF_SVC` / `BFF_PORT`** — namespace / Service / local
  port for the `run-local` + `forward` port-forward. Defaults: `memql` /
  `bff` / `50051`.
- **`MEMQL_ALLOW_CONTEXT`** — set to `1` to skip `run-local`'s guard that
  the active kube-context is a local (`k3d-…`) cluster. Staging/prod use
  the *same* `memql` namespace + `bff` Service, so the guard exists to
  stop `run-local` silently forwarding to a remote cluster.

### Computer use

The worker's computer-use surface (`workerComputer.*`: screenshot,
mouse, keyboard, window control, displays, capabilities) is
documented in **[docs/computer-use.md](docs/computer-use.md)** --
architecture, the full 16-action vocabulary, the coordinate model,
the capability descriptor, the platform support matrix, permission
setup, and troubleshooting. The short version:

- **macOS** -- the computer-use worker needs two TCC grants for the
  `memql-cockpit-computeruse` binary under System Settings -> Privacy &
  Security: **Accessibility** (mouse + keyboard) and **Screen
  Recording** (screenshots). `worker setup` probes both; the worker
  re-checks them on every dispatch, so a mid-session revocation
  surfaces as a structured `permission_denied` with a remediation
  hint.
- **Linux** -- RobotGo drives **X11 only**; on Wayland (or with no
  display) input + screenshot actions return
  `display_server_unsupported` -- register HEADLESS-only there
  (`install-linux.sh` detects Wayland and does this automatically).
  Building the computer-use variant needs `gcc` plus the X11 dev
  packages listed in [docs/computer-use.md](docs/computer-use.md).

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

## Releasing

Releases are **tag-driven**: cutting a `vX.Y.Z` tag + GitHub Release triggers
`.github/workflows/release.yml`, which cross-builds and uploads the per-platform
binaries (`darwin`/`linux` × `arm64`/`amd64`) + `SHA256SUMS`.

`make release` recommends the next version from the commits since the last tag —
**no AI**, just conventional-commit prefixes (`feat!:`/`BREAKING CHANGE` → major,
`feat:` → minor, everything else → patch):

```bash
make release                          # recommend only (safe): shows the next version + why
make release ARGS="--cut"             # cut the recommended version (asks to confirm)
make release ARGS="--bump=minor --cut"       # force a bump level
make release ARGS="--version=1.2.3 --cut"    # force an explicit version
make release ARGS="--cut --yes"       # non-interactive
```

Cutting bumps `VERSION` + the `main.go` version constant, commits `release:
vX.Y.Z`, tags, pushes, and opens the GitHub Release. See
`scripts/release/cut-release.sh --help`.

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
