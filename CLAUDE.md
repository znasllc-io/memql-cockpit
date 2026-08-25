# memQL Cockpit

**Type:** Terminal-native IDE and operations console for MemQL
**Binary:** `memql-cockpit` (built from `cmd/memql-cockpit/`)
**Display name:** "memQL Cockpit" -- shown in the header chrome and Settings
**Language:** Go (tcell TUI)

The Cockpit is the terminal sibling of the MemQL Portal: it manages clusters,
explores concepts, browses and authors MemQL, drives deployments, and runs as a
**worker** on the operator's own machine. The engine lives in a separate repo
(`github.com/znasllc-io/memql`) and is consumed as a **pinned sibling checkout**
-- see "The memql pin" below, which is the single most surprising thing about
this repository.

---

## Quick Start

```bash
make cockpit             # build the headless binary -> bin/memql-cockpit
make cockpit-computeruse # the computer-use variant (CGO + RobotGo)
make run                 # build + run against a clusters.yaml cluster
                         #   ARGS="--cluster local|staging|..."
make forward             # port-forward the local k3d svc/bff for `--cluster local`

make test                # go test ./...
make lint                # fmt + vet
make tidy                # go mod tidy
make help                # every target, auto-generated from target comments

make dist                # versioned tar.gz archives + SHA256SUMS into dist/
make release             # recommend the next version; ARGS="--cut" tags + releases
```

`./...` really does mean everything here. **This is a single Go module** with no
`go.work` and no nested modules -- the opposite of the memql repo, where a bare
`go test ./...` silently misses the engine. Do not import that repo's caution
into this one.

---

## The memql pin (read this before touching `go.mod`)

Cockpit consumes ~21 memql packages through **`replace` directives pointing at a
sibling checkout at `../memql`**. Local builds therefore resolve against
whatever your memql working copy happens to be; CI resolves against a **pinned
commit**.

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
- **To bump:** change the sha, add the newly-landed module's `require` **and**
  `replace`, and run `go mod tidy` -- **all in one commit**. The truth table for
  which combinations fail (and how) is in `go.mod`'s own comment block; read it
  before guessing.

### Consequence: local build failures that are not your fault

If `go build ./...` here reports `updates to go.mod needed`, the usual cause is
that your `../memql` checkout has moved past the pin, not that this repo is
broken. Check `.github/memql-pin` against your memql sibling before debugging.

> **Known pin-bump blocker (as of 2026-08-23).** Six files import
> `memql/component/genesis` -- `cli/app.go`, `cmd/memql-cockpit/main.go`,
> `cli/wizard/{genesis,config,runlocal}` -- and memql **deleted that package**
> on 2026-08-16 (`36c19108`, `feat(config)!: delete the genesis envelope`). The
> pin (`3b913f37`, 2026-08-07) predates the removal, which is the only reason
> CI is green. Bumping the pin past that commit requires porting the
> sealed-envelope wizards onto whatever replaced it (`component/envregistry` +
> the recovery-key flow) in the same PR.

---

## Repository Structure

```
memql-cockpit/
├── cli/                    The multi-tab IDE. See cli/CLAUDE.md -- the
│   │                       authoritative spec for every TUI surface
│   ├── ui/  canvas/        TUI primitives: Screen, Theme, Header, layout;
│   │                       and the pixel framebuffer. ALL surfaces use these
│   ├── cluster/            DevOps tab: cluster manager, topology, deployments
│   ├── concepts/           Concepts tab: generic row browser
│   ├── dsledit/ editor/    Editor tab: pack browser + bundle authoring
│   ├── settings/           Settings tab
│   ├── auth/               Identity-service auth (magic-link + PKCE-ish code grant)
│   ├── splash/             Launch splash -- the first surface on start
│   ├── wizard/             Launch-time single-panel wizards:
│   │                         config/    Configuration screen (env-var registry)
│   │                         genesis/   First-launch envelope wizard
│   │                         runlocal/  "Set up local cluster"
│   ├── healing/            Self-healing healed-pack review flow (memql#2144)
│   ├── dockerprobe/        Enumerates local docker containers for a cluster
│   ├── crash/              Panic recovery + crash reports (see below)
│   └── config/             ~/.memql/clusters.yaml load/save
├── cmd/memql-cockpit/      Binary entry point + focused subcommands:
│   └── internal/
│       ├── worker/         `worker run` -- the operator's machine as a worker
│       ├── deploy/         `deploy` / `run` -- DevOps DSL deployment bundle
│       ├── harness/        `harness trace <planId>` -- plan timeline dump
│       ├── lint/           `lint <path>` -- author-facing DSL validator
│       └── setupproject/   `setup project` -- stamps a memql-project workspace
├── docs/                   computer-use.md, deploy-runner.md
├── deploy/  scripts/  assets/
└── .github/memql-pin       THE pin. Single source of truth
```

---

## Build tags

| Tag | What it adds | Constraint |
|---|---|---|
| _(none)_ | Headless build. The default, and what `make cockpit` produces | `CGO_ENABLED=0`, cross-compiles to every platform |
| `computeruse` | Wraps RobotGo for screenshot + mouse + keyboard, adding the `workerComputer.*` tool surface | **`CGO_ENABLED=1` required**; needs X11 dev headers on Linux |

CI builds and vets both. A change that compiles headless can still break the
computer-use lane -- that lane is not optional.

---

## Two rules that reviewers enforce

Both are stated in full in [cli/CLAUDE.md](cli/CLAUDE.md); they are repeated
here because they bind code outside `cli/` too.

1. **Canonical-TUI rule.** Every interactive surface goes through `cli/ui` +
   `cli/canvas`. `fmt.Println` interactive flows in new subcommands are a
   regression. Non-interactive single-shot output (`cluster list`, `--version`)
   stays printf, and every TUI surface MUST detect a non-TTY and fall back to
   printf so CI and install scripts don't choke on escape codes.
2. **SDK-only rule.** Every wire call goes through `memql/sdk/go/` --
   `client/` for queries/mutations/subscriptions, `sense/` for language
   intelligence, `worker/` for worker dials. No direct `grpc.NewClient`, no
   `memqlv1` imports, no raw DSL strings. **If you can't express something
   through the SDK, the SDK needs to grow** -- open an issue on memql, add the
   typed method there, come back.

---

## CI

`ci.yml` runs six jobs: `build` (headless), `test` (`go test -count=1
-timeout=300s ./...`), `gofmt -l`, `vet`, `build-computeruse` (CGO + X11
headers), and `memql-pin-guard`. Separate workflows cover CodeQL, gitleaks,
govulncheck, SBOM, OpenSSF Scorecard and install-script lint.

Every Go lane checks out the pinned memql sibling first, so a lane that fails
only on the pin bump is telling you about the engine, not about your diff.

---

## Crash reports

`cli/crash` turns "panic on the goroutine holding the terminal" into a rendered
message in the affected pane (or a restored terminal plus a printed message),
an error code for the user, and a structured log to hand to support. When you
add a goroutine that can touch the screen, route its recovery through this
package rather than letting the panic escape.

---

## Documentation

- [cli/CLAUDE.md](cli/CLAUDE.md) -- **the TUI spec.** Tab order, layout bands,
  every pane's keys, list-pane conventions, the layout-edge glyph rule, the
  connection pool and the single-live-connection invariant. Read it before
  changing any surface under `cli/`.
- [docs/computer-use.md](docs/computer-use.md) -- the computer-use worker build.
- [docs/deploy-runner.md](docs/deploy-runner.md) -- the deploy runner.
- [VERSIONING.md](VERSIONING.md) -- version + release policy.
- The engine, DSL and wire contract live in the **memql** repo; its root
  `CLAUDE.md` is the reference for anything server-side.

---

## Documentation Style

No emojis. Use `[ ]` / `[x]` checkboxes and "SUCCESS:" / "ERROR:" / "WARNING:" /
"INFO:" text indicators. Applies to docs, CLI output and all user-facing text.
