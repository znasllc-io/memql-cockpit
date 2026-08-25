<p align="center">
  <img src="assets/logo.svg" alt="MemQL Cockpit" width="500">
</p>

<h1 align="center">MemQL Cockpit</h1>

<p align="center">
  <strong>The fleet worker runtime and cluster CLI for <a href="https://github.com/znasllc-io/memql">MemQL</a>.</strong><br>
  Installed as the <code>memql</code> command on the machines you enroll in your fleet.
</p>

<p align="center">
  <a href="https://github.com/znasllc-io/memql-cockpit/actions/workflows/ci.yml"><img src="https://github.com/znasllc-io/memql-cockpit/actions/workflows/ci.yml/badge.svg" alt="CI"></a>
  <a href="LICENSE"><img src="https://img.shields.io/github/license/znasllc-io/memql-cockpit?color=blue" alt="License"></a>
  <img src="https://img.shields.io/github/go-mod/go-version/znasllc-io/memql-cockpit" alt="Go version">
  <img src="https://img.shields.io/github/last-commit/znasllc-io/memql-cockpit" alt="Last commit">
  <a href="https://goreportcard.com/report/github.com/znasllc-io/memql-cockpit"><img src="https://goreportcard.com/badge/github.com/znasllc-io/memql-cockpit" alt="Go Report Card"></a>
</p>

<p align="center"><sub><em>Designed and built with Claude as co-author.</em></sub></p>

> **Status: Alpha / pre-1.0 — not production-ready.** MemQL Cockpit is under
> active development and tracks MemQL core. The worker contract and
> configuration are still evolving; expect breaking changes between releases.

---

## What is MemQL Cockpit?

MemQL Cockpit turns a machine you own into a **worker** in your MemQL fleet:
agents on the cluster can dispatch shell / filesystem / HTTP work to it
(headless), and — on the opt-in computer-use build — drive its mouse, keyboard
and screen. Machines are managed from the MemQL Portal's Fleet section:
pairing, labels, routing policy, activity.

The installed command is **`memql`**:

- **Enroll a machine** — `memql cluster add <domain>` registers the cluster
  (identity discovery + OAuth sign-in; RFC 8628 device flow on headless / SSH
  boxes) and `memql worker pair <code>` redeems a pairing code from the portal.
- **Run as a service** — a per-user LaunchAgent on macOS
  (`com.znasllc.memql-worker`) or user-systemd unit on Linux
  (`memql-worker.service`), auto-started at login.
- **Two build variants, one command** — headless (default, CGO-free) ships
  from releases; computer-use (`-tags computeruse`, CGO + RobotGo) is built
  per-host. `memql --version` names the variant.

It communicates with MemQL clusters over gRPC (`MemqlService.Stream` and
`WorkerService.Stream`) and does not embed the MemQL engine.

> Note: the MemQL engine repo also builds a binary named `memql`, but it ships
> only inside container images and runs in pods. This CLI is what gets
> installed on operator machines — different channels, no PATH overlap.

## Install

```bash
# one-liner (detects OS/arch, checksum-verified)
curl -fsSL https://raw.githubusercontent.com/znasllc-io/memql-cockpit/main/install.sh | sh

# pin a version, or an install dir
curl -fsSL https://raw.githubusercontent.com/znasllc-io/memql-cockpit/main/install.sh | MEMQL_COCKPIT_VERSION=v0.10.0 sh
curl -fsSL https://raw.githubusercontent.com/znasllc-io/memql-cockpit/main/install.sh | BIN_DIR=/usr/local/bin sh
```

Or, with Go (builds from source):

```bash
go install github.com/znasllc-io/memql-cockpit/cmd/memql@latest
```

Worker-machine installers (binary + service + worker.yaml in one step) live in
`scripts/install/` — the MemQL Portal's Fleet page composes the exact
one-liner for you when you add a machine.

## Commands

```
memql cluster add <domain|url>    Register a cluster (discovery + OAuth login)
memql cluster list | remove       Manage saved clusters
memql login | logout <cluster>    (Re-)authenticate / drop credentials
memql creds <subcommand>          Inspect / migrate the credential store
memql worker pair <code>          Redeem a pairing code, write worker.yaml, run
memql worker run                  Run the worker (what the service invokes)
memql worker setup                Computer-use permission pre-flight (TCC / X11)
memql worker config | consent     Show config / manage consent
memql lint [path]                 Validate a .memql file or DSL tree
memql setup project [flags]       Stamp a new product workspace from the template
memql --version                   Version + build variant
```

`memql worker setup --non-interactive` reports missing permissions with honest
exit codes and never prompts — for scripted installs.

## Build

```bash
make cockpit             # headless -> bin/memql
make cockpit-computeruse # computer-use variant -> bin/memql-computeruse
                         #   (CGO; macOS Xcode CLT / Linux libxtst-dev etc.)
make test                # go test ./...  (single module -- this really is everything)
make dist                # versioned tar.gz archives + SHA256SUMS into dist/
```

The engine is consumed as a pinned sibling checkout — read the memql-pin
section in [CLAUDE.md](CLAUDE.md) before touching `go.mod`.

Computer-use setup and permissions: [docs/computer-use.md](docs/computer-use.md).

## License

[MIT](LICENSE)
