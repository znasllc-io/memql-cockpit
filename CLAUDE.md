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
│   │                       (the app-session runner) -- see Local apps;
│   │                       models/ (runtime discovery + the hardware
│   │                       floor) + modelcall/ (the ModelCall server)
│   │                       -- see Local models; backup/ (the watched-folder
│   │                       sweeper) -- see Watched-folder backup
│   ├── lint/               `memql lint` — author-facing DSL validator
│   └── setupproject/       `memql setup project` — stamps a memql-project
│                           workspace (stdin prompts when flags are absent)
├── scripts/install/        Worker-machine installers (mac / linux): binary +
│                           service + worker.yaml; the portal composes the
│                           one-liner. install.sh at the root is the plain
│                           binary installer from GitHub releases
├── deploy/systemd/         memql-worker.service template (user systemd)
├── docs/                   computer-use.md, local-apps.md,
│                           local-models.md, watched-folders.md;
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

## Local models on the fleet

The worker can serve MemQL's own operations -- planning, conductor/routing,
suggestions, embeddings -- from a model running on **this** machine, over
the stream it already holds open. The engine half is memql#4676; the
canonical record is the engine's
`docs/public/operate/local-models.md`. `docs/local-models.md` here covers
the machine side.

- **`internal/worker/models`** discovers Ollama natively (`/api/tags` +
  `/api/show`) and reads OpenAI-compatible endpoints declared in
  `policy.yaml`. It also owns the **hardware floor** -- Apple Silicon /
  16 GB / macOS 13+, or x86_64 with a >= 8 GB discrete GPU. The check runs
  on the machine because only the machine can see its own GPU.
- **`internal/worker/modelcall`** serves `ModelCallStart / Delta / End /
  Cancel` against the local runtime: both kinds (`chat`, `embedding`),
  monotonic delta `seq`, envelope-owned deadlines, and both concurrency
  ceilings.

Five rules here are load-bearing, and each fails silently:

1. **Every capability defaults to ABSENT.** The engine is fail-closed: a
   model that says nothing about structured output is never selected for a
   structured prompt. A probe that cannot establish a capability must claim
   nothing -- an over-claim surfaces as a parse failure three layers away,
   naming nothing here. Ollama has no structured-output capability of its
   own, so `tools` is the proxy; an operator who disagrees declares the
   model under an OpenAI-compatible runtime instead.
2. **`max_concurrent` is the exception, and it is never absent.** It is the
   one attribute whose absence is PERMISSIVE -- the engine reads a missing
   ceiling as unlimited. `OLLAMA_NUM_PARALLEL` when set, otherwise 1.
3. **The floor GATES the inventory; it does not annotate it.** Below it a
   machine advertises nothing at all, and `Discover` does not even probe.
   `memql worker models` uses `Probe` instead, so an operator can still see
   what they have.
4. **Model labels are bound at Register, and Heartbeat cannot refresh
   them.** `Heartbeat` carries apps but no labels, and the engine's stream
   handler accepts `Register` exactly once. Re-advertising therefore costs
   a RECONNECT -- taken only when the advertised labels actually changed,
   only when nothing is in flight, and never twice inside two minutes.
5. **`sharedInference` is the owner's grant, never the cockpit's.** The
   engine reads it from `operatorLabels` alone, because `labels` is
   overwritten from Register on every reconnect. A cockpit that derived one
   would be granting itself a permission and revoking it whenever the lid
   closed.

`policy.yaml models.allow` is default-deny like `apps.allow`, and a model
present but unlisted is REPORTED as blocked rather than omitted. Usage
rides on `ModelCallEnd` exactly as the runtime reported it -- silence stays
silence, which the engine records as billing "unknown".


## Watched-folder backup (memql#4841)

One folder on this machine, kept arriving in a MemQL Library folder. The engine
half landed in epic memql#4783; this is the half that can see the origin.
`internal/worker/backup/`, plus `memql worker backup [--once]`. Operator doc:
[docs/watched-folders.md](docs/watched-folders.md).

**ONE-WAY, FOREVER.** Nothing in that package reads the Library and writes this
machine, and nothing deletes, moves or hides a copy because of something at the
origin. A file deleted here is FLAGGED there (`origin_gone`) and stays whole and
downloadable. Two-way sync and conflict resolution are refused deliberately --
that is the complexity cliff this sits on the safe side of, and a test asserts
the sweeper sends no destructive call at all.

**The credential is the SIGNED-IN USER'S, and it has to be.** The Library's
HTTP routes resolve an actor only for a `class="user"` (or classless) bearer;
the engine pins every machine class off that surface deliberately. So the
`mql_wkr_` token this process authenticates its STREAM with cannot reach
`/artifacts`, and neither can a PAT (PATs verify only on the identity node).
A machine that is paired but not signed in backs nothing up, which is the
ordinary state of a fresh worker and must not be a startup failure --
`backupBearer` returns nil and the manager is a working no-op. The sign-in is
resolved NON-INTERACTIVELY (`auth.EnsureValidTokenNonInteractive`): under a
LaunchAgent the browser step is not slow, it is a block on a window that will
never open.

**The graph says WHICH folder; this machine says WHETHER.** A watch row is
written from a browser, so its path is one the cluster names on somebody else's
computer -- the situation `appsession`'s `CheckWorkspace` exists for, and the
same answer. `policy.yaml`'s `backup.roots` is default-deny, and a refusal is
REPORTED (`originState=refused_by_policy`) rather than silent: a machine that
quietly ignored a watch is indistinguishable from one that is offline.

**A SCHEDULED WALK, NOT fsnotify**, and this is a decision rather than a
shortcut. A backup must RECONCILE -- everything that changed while the process
was down produced no event; the verify lane has to look on a schedule anyway to
answer `stale` and `origin_gone`; and recursive watches are not portable
(inotify is one watch per directory against a per-user cap, and exhausting it
presents as a backup that silently stops noticing). It also adds no dependency.
fsnotify is a sensible ACCELERATOR later; the sweep stays the source of truth.

**Three gates, cheapest first**: an unchanged (size, mtime) costs nothing; a
moved stamp with an unchanged digest costs one read and no upload; only new
bytes are sent. **The size a push DECLARES comes from a stat taken immediately
before it**, never from the walk's -- a stale smaller size makes the session
route send only that many bytes of a file that has since grown, the engine's
commit check passes (staged == declared), and the copy is silently truncated
and then never repaired, because the digest already matches.

The ledger under `<state_dir>/backup/<watchId>.json` is a CACHE -- losing it
costs one expensive sweep, because every re-push is keyed on `(machine, path)`
and lands as a new VERSION rather than a duplicate. It is saved on the way OUT
of a sweep, deferred, so an interrupted one keeps what it already sent; and it
carries the open session id, which is what makes the resume real (a fresh
session's inventory is empty by construction, so asking one what it holds
resumes nothing).

**`memql worker backup --once` needs a recorded registration id.** The id
arrives on a RegisterAck inside a connection only the running worker holds, so
the loop persists it and the command reads it. Without one the command REFUSES
-- sweeping as nobody matched no rows, pushed nothing and reported success.

**A 200 is not success.** `/memql/query` answers HTTP 200 with the refusal in
an `errors` array, so a client that only checked the status would read "you may
not see these rows" as "you are watching nothing" -- and a backup with nothing
to do looks exactly like one that is up to date. Every call reads `errors`
first.

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
