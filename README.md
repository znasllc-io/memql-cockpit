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
  from releases as tar.gz archives; computer-use (`-tags computeruse`, CGO +
  RobotGo) ships as prebuilt `memql-computeruse-<os>-<arch>` release assets,
  built natively per platform. `memql --version` names the variant.

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
one-liner for you when you add a machine. Their `--computeruse` flag installs
the prebuilt computer-use binary from the same release; building it from
source (below) remains the alternative. Their `--inference` flag runs
`memql worker setup --inference --non-interactive` once the worker is up, and
never fails the install over it — a machine that paired fine and could not set
up local models is still a working worker.

### Uninstall

One line, like the install. It stops and removes the service, removes the
binary and its symlink, and removes `~/.memql/worker.yaml` — the file that
holds the token:

```bash
# Linux
curl -fsSL https://raw.githubusercontent.com/znasllc-io/memql-cockpit/main/scripts/install/uninstall-linux.sh | bash -s -- [--purge] [--user-local]

# macOS: remove one enrollment (shared runtime stays while other homes remain)
curl -fsSL https://raw.githubusercontent.com/znasllc-io/memql-cockpit/main/scripts/install/uninstall-mac.sh | bash -s -- --cluster=https://api.example.com [--user-local]
# For whole-machine worker removal, use --all-homes instead of --cluster=URL.
```

On macOS, last-home or full removal resets only the installed MemQL apps’
Accessibility and Screen Recording decisions; shared sibling enrollments keep
their permissions. Reset failures are reported, and cached Settings rows may
need manual removal. `--purge` is refused while another enrollment remains. CLI credentials,
cluster settings, certificates and backups are retained even with a purge.

Without `--purge`, `~/.memql/policy.yaml` and the state directory (logs,
ledgers) stay, and the script says so; `--user-local` removes a `--user-local`
install from `~/.memql/bin` instead of `/usr/local/bin`. The machine's
registration on the cluster is not touched from here — revoke it from MemQL OS
(Fleet -> Machines).

## Commands

```
memql cluster add <domain|url>    Register a cluster (discovery + OAuth login)
memql cluster list | remove       Manage saved clusters
memql login | logout <cluster>    (Re-)authenticate / drop credentials
memql access [<cluster>]          What this cluster says you are: role slug,
                                  name and rank, groups, account scope (--json)
memql creds <subcommand>          Inspect / migrate the credential store
memql worker pair <code>          Redeem a pairing code, write worker.yaml, run
memql worker run                  Run the worker (what the service invokes)
memql worker setup                Computer-use permission pre-flight (TCC / X11)
memql worker setup --inference    Turn this machine into an inference machine:
                                  install a model runtime, pull the models,
                                  write models.allow, signal the worker
memql worker models               What local models this machine offers, or why
                                  it offers none (--pull / --allow change it)
memql worker config | consent     Show config / manage consent
memql lint [path]                 Validate a .memql file or DSL tree
memql setup project [flags]       Stamp a new product workspace from the template
memql --version                   Version + build variant
```

`memql worker setup --non-interactive` reports missing permissions with honest
exit codes and never prompts — for scripted installs. The codes are the
capability-script contract's: 2 bad invocation, 3 refused because a required
confirmation could not be asked for, 4 a prerequisite is absent, 5 something
failed.

One command takes a qualifying machine from bare to serving:

```bash
memql worker setup --inference
```

It checks the hardware floor, installs the runtime this platform can serve
from (macOS: Ollama natively; Linux: the container with the GPU passed
through) after printing the exact commands and asking, pulls `llama3.1:8b`
and `nomic-embed-text` with byte counts on screen, writes `models.allow`, and
signals the running worker. Nothing in it runs `sudo` — where a fix needs
root, the command is printed for you. Details:
[docs/local-models.md](docs/local-models.md).

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
