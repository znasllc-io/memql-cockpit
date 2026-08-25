# MemQL Cockpit

**Type:** Fleet worker runtime + cluster CLI for MemQL
**Binary:** `memql` (built from `cmd/memql/`)
**Display name:** "MemQL Cockpit"
**Language:** Go

The Cockpit is the machine-side half of the MemQL fleet: it enrolls a machine
against a cluster and runs as that machine's **worker** (headless shell / fs /
http tools everywhere; mouse + keyboard + screenshot on the computer-use
build). The TUI it once carried is gone (2026-08-25 slim-down — spec in
`docs/superpowers/specs/2026-08-25-cockpit-slim-rename-design.md`); the portal
and the VS Code extension own every interactive surface now. The engine lives
in a separate repo (`github.com/znasllc-io/memql`) and is consumed as a
**pinned sibling checkout** — see "The memql pin" below, which is the single
most surprising thing about this repository.

**The command name is `memql`, and the collision is deliberate:** the engine
repo also builds a `bin/memql`, but it ships only inside container images and
runs in pods. This CLI is what gets installed on operator machines — different
distribution channels, no PATH overlap. Do not "fix" this.

---

## Quick Start

```bash
make cockpit             # build the headless binary -> bin/memql
make cockpit-computeruse # the computer-use variant (CGO + RobotGo) -> bin/memql-computeruse
make worker              # build + run the worker (ARGS="--log-level=debug")
make clusters            # build + list the registered clusters
make test                # go test ./...
make lint                # fmt + vet
make tidy                # go mod tidy
make help                # every target, auto-generated from target comments

make dist                # versioned tar.gz archives + SHA256SUMS into dist/
make release             # recommend the next version; ARGS="--cut" tags + releases
```

`./...` really does mean everything here. **This is a single Go module** with no
`go.work` and no nested modules — the opposite of the memql repo, where a bare
`go test ./...` silently misses the engine. Do not import that repo's caution
into this one.

---

## The memql pin (read this before touching `go.mod`)

Cockpit consumes the engine's wire-tier packages through **`replace`
directives pointing at a sibling checkout at `../memql`**. Local builds
therefore resolve against whatever your memql working copy happens to be; CI
resolves against a **pinned commit**.

- **The pin lives in exactly one file: `.github/memql-pin`.** Every workflow
  reaches the sibling through `.github/actions/checkout-memql`, which reads it.
  The `memql-pin-guard` job in `ci.yml` fails if any workflow checks out
  `znasllc-io/memql` directly.
- **The `require` pseudo-version in `go.mod` is NOT the pin.** It is an
  epoch-zero placeholder that exists only because Go must validate a require
  against a real remote or a local `replace`. Reading it as the pin gives you
  the wrong commit and the wrong date.
- **Why pinned at all:** memql is being split into ~29 nested modules
  (memql#3228). No static cockpit `go.mod` is green both before and after a
  given module lands, and there is no atomic cross-repo merge. The pin buys the
  window.
- **To bump:** change the sha, add any newly-landed module's `require` **and**
  `replace`, and run `go mod tidy` — **all in one commit**. The truth table for
  which combinations fail (and how) is in `go.mod`'s own comment block; read it
  before guessing.
- **The 2026-08 blocker is gone, and the pin has moved past it.** The
  slim-down deleted every `component/genesis` importer (the TUI wizards and
  the embedded deploy runtime), and the local-apps bump then advanced the pin
  across the genesis removal (`36c19108`) and the engine / platform / server
  tiers in one commit. Two modules cockpit already imported —
  `component/identity` and `component/memql` — grew their own `go.mod` in that
  window and are now direct `require`s; the rest of the `replace` block is the
  transitive closure `go mod tidy` demanded, not a hand-picked list.

### Consequence: local build failures that are not your fault

If `go build ./...` here reports `updates to go.mod needed`, the usual cause is
that your `../memql` checkout has moved past (or lags) the pin. Check
`.github/memql-pin` against your memql sibling before debugging; a detached
checkout of the pinned sha is the reference state.

---

## Repository Structure

```
memql-cockpit/
├── cmd/memql/              Binary entry point: dispatch, cluster add/list/
│                           remove, login/logout, creds; variant consts
├── internal/
│   ├── auth/               Identity-service auth: browser code grant with
│   │                       loopback callback; RFC 8628 device flow fallback
│   │                       (device.go) for SSH / headless machines
│   ├── config/             ~/.memql/clusters.yaml registry, credential
│   │                       stores (keyring / file), identity discovery
│   │                       (/.well-known/memql-config.json client)
│   ├── crash/              Panic recovery + redacted crash reports
│   ├── worker/             The worker: pair / run / setup / config /
│   │                       consent; LaunchAgent + service glue; tools/
│   │                       (shell, fs, http; computeruse adds screenshot /
│   │                       mouse / keyboard / window via RobotGo);
│   │                       apps/ (local-app detection) + appsession/
│   │                       (the app-session runner) -- see Local apps
│   ├── lint/               `memql lint` — author-facing DSL validator
│   └── setupproject/       `memql setup project` — stamps a memql-project
│                           workspace (stdin prompts when flags are absent)
├── scripts/install/        Worker-machine installers (mac / linux): binary +
│                           service + worker.yaml; the portal composes the
│                           one-liner. install.sh at the root is the plain
│                           binary installer from GitHub releases
├── deploy/systemd/         memql-worker.service template (user systemd)
├── docs/                   computer-use.md, local-apps.md;
│                           docs/superpowers/specs/ designs
└── .github/memql-pin       THE pin. Single source of truth
```

## Build variants

Two builds, **one installed command name** (`memql`):

- **headless** (default, CGO off) — ships from GitHub releases; shell / fs /
  http worker tools.
- **computeruse** (`-tags computeruse`, CGO + RobotGo) — adds the
  workerComputer.* surface; built per-host (macOS Xcode CLT; Linux gcc +
  libxtst-dev / libxinerama-dev / libxkbcommon-dev / libpng-dev).

`memql --version` prints the variant; the worker's capability registration
carries it to the cluster. Dev cross-builds emit suffixed artifacts in `bin/`
(`memql-darwin-arm64`, `memql-computeruse`, ...) but an installed machine has
exactly one `memql`.

## Local apps as execution surfaces

The worker can delegate a planner Task to an app the user **already pays
for** -- Claude Code or Codex -- running on this machine, with MemQL's tools
reachable from inside it over MCP. The engine half is memql#4358; the
canonical record is the engine's
`docs/public/operate/local-apps.md`. `docs/local-apps.md` here covers the
machine side.

- **`internal/worker/apps`** detects `claude` / `codex` on PATH, their
  versions and their auth state, and the worker reports the inventory on
  `Register` and **every** `Heartbeat`. The engine derives `app:<id>` routing
  labels from it and has no other way to learn any of it.
- **`internal/worker/appsession`** runs the sessions:
  `AppSessionStart / Chunk / Control / End`, kinds `run` / `open` / `attach`,
  plus the MCP config writer, the Library pull/push, and the platform launch
  paths.

Four rules here are load-bearing, and each is the kind that fails silently:

1. **`signed_in=false` beats a guess.** A routing label needs `allowed` AND
   `signed_in`, so the router cannot pick a machine that would then refuse.
   Every probe reports false when it cannot tell -- including on macOS, where
   Claude Code's token is in the Keychain and the worker must not read it (a
   LaunchAgent raising a Keychain prompt is the same hazard the credential
   store already avoids).
2. **`apps_present` is always true on the beat.** proto3 cannot distinguish an
   empty repeated field from an absent one; `false` means "this build does not
   report apps", which is wrong for a machine that just uninstalled one.
3. **`apps.allow` in `policy.yaml` is default-deny.** An app session does what
   `workerHost.exec` does. An app present but unlisted is reported with
   `allowed=false` rather than omitted -- the portal can then say "present,
   blocked" instead of rendering it identically to "not installed".
4. **The MCP config file is deleted on every exit path.** The per-run bearer
   **cannot be revoked** (the engine's verify path is JWKS-only and DB-free),
   so deletion is the security control, not housekeeping. A `defer` is not
   enough: every write is recorded in a ledger under the state dir, and
   `appsession.Sweep` clears what a SIGKILL left behind at the next start.

`usage.known=false` when the app reported nothing, and `exit_code` passes
through unnormalised -- the engine records the first as billing "unknown" and
reads the second as a failed run.

## Worker + auth notes

- **Enrollment:** `memql cluster add <domain>` fetches
  `https://identity.<domain>/.well-known/memql-config.json` (falling back to
  the api./identity. convention when discovery is unreachable), registers the
  cluster and signs in. On a box with no browser (`DISPLAY` unset on linux, or
  the launch fails), sign-in falls back to the RFC 8628 device flow against
  `/device/code` + `/oauth/token`.
- **Services:** macOS LaunchAgent label `com.znasllc.memql-worker`; Linux
  user-systemd `memql-worker.service`. Installers retire the pre-rename
  `memql-cockpit-worker` agent/unit and binaries in place.
- **Credential stores:** OS keyring preferred, file fallback
  (`~/.memql/credentials/`, 0600). `MEMQL_COCKPIT_CRED_STORE` forces one.
  The keyring service name stays `com.znasllc.memql-cockpit` on purpose —
  renaming it would strand every existing entry for zero user-visible gain.
  The worker's own run path deliberately never resolves the keyring (a
  LaunchAgent must not trigger Keychain prompts); it keeps the lazy file
  default.
- **worker.yaml** (`~/.memql/worker.yaml`) carries cluster URL, worker token,
  name, capabilities. `memql worker config` prints the effective config.

## Testing

`go test ./...` is the whole suite (single module). The installer shell
library has its own tests: `bash scripts/install/lib_test.sh`.

## Releases

Tag-driven: `make release` recommends the next semver; `ARGS="--cut"` bumps
VERSION + `cmd/memql/main.go`, tags, and publishes the GitHub Release, which
`release.yml` fills with per-platform raw binaries (`memql-<os>-<arch>`),
tar.gz archives (`memql-<version>-<os>-<arch>.tar.gz`) and a SHA256SUMS
manifest — the names `install.sh` and `scripts/install/` download.
