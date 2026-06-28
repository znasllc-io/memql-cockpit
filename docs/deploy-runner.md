# Cockpit deploy: packaging, distribution & runner-surface provisioning

This runbook covers how the **memQL Cockpit** is packaged for distribution and
how the `deploy` control surface gets its cluster credentials at runtime —
from an operator machine and from a CI runner. It is the operator-facing half
of I17 (memql#2228), part of the DevOps DSL deployment bundle epic
(memql#2212).

> **Status.** The deploy *command infrastructure* (role gate, audit trail,
> version pin, embedded runtime, surface wiring) is live. A **live** deploy
> that actually mutates a cluster is **owner-gated** and not fully wired until
> I13 (memql#2220) ships the runner capability surface. Everything below is
> safe to run today as a **dry-run / no-op**.

---

## 1. Distributable binary

The cockpit ships as a single static (CGO-disabled) headless binary per
platform. There is no installer; operators and CI runners download or build
the binary and put it on `PATH`.

### Build / package locally

```bash
make cockpit               # host platform -> bin/memql-cockpit
make cockpit-all-platforms # cross-compile darwin/linux x arm64/amd64
make dist                  # versioned tar.gz archives + SHA256SUMS -> dist/
```

`make dist` produces, for each of darwin-arm64, darwin-amd64, linux-amd64,
linux-arm64:

```
dist/memql-cockpit-<version>-<os>-<arch>.tar.gz   # binary (renamed memql-cockpit) + LICENSE + README
dist/memql-cockpit-<version>-SHA256SUMS           # checksum manifest over the archives
```

The version is the exact git tag on a tagged checkout, else the `VERSION`
file (see `VERSIONING.md`).

### Released artifacts

The **Release binaries** workflow (`.github/workflows/release.yml`) runs on
every published GitHub Release (and `workflow_dispatch` against an existing
tag). It builds all platforms, runs `make dist`, and attaches both the raw
binaries and the `dist/` archives + `SHA256SUMS` to the release.

### Install on an operator machine or CI runner

```bash
VER=0.9.0; OS=linux; ARCH=amd64       # or darwin/arm64, etc.
base=memql-cockpit-$VER-$OS-$ARCH
curl -sSLO https://github.com/znasllc-io/memql-cockpit/releases/download/v$VER/$base.tar.gz
curl -sSLO https://github.com/znasllc-io/memql-cockpit/releases/download/v$VER/memql-cockpit-$VER-SHA256SUMS
shasum -a 256 -c memql-cockpit-$VER-SHA256SUMS --ignore-missing
tar -xzf $base.tar.gz
sudo install memql-cockpit /usr/local/bin/memql-cockpit
memql-cockpit --version
```

---

## 2. Runner-surface credential provisioning

A deploy initiated from outside the target cluster needs three things, all
sourced from the environment (CI secrets / Azure Key Vault) and **never
committed**:

| Need | Env var | Secret? | Notes |
|------|---------|---------|-------|
| Cluster API access | `MEMQL_COCKPIT_KUBECONFIG` (falls back to `KUBECONFIG`) | path to a secret file | the file content is the secret, not the path |
| ArgoCD server | `MEMQL_COCKPIT_ARGOCD_SERVER` | no | e.g. `https://argocd.staging.internal` |
| ArgoCD auth | `MEMQL_COCKPIT_ARGOCD_AUTH_TOKEN` | **yes** | never logged |
| Genesis envelope | `MEMQL_COCKPIT_GENESIS_ENVELOPE` (inline base64) **or** `MEMQL_COCKPIT_GENESIS_ENVELOPE_FILE` (path) | **yes** | the sealed cluster env/secret bundle |

Role / identity for the pre-flight gate (separate from the above):

| Env var | Purpose |
|---------|---------|
| `MEMQL_COCKPIT_ROLE` | caller role (`developer`+ for forward deploy, `owner` for rollback). Default `reader` → denied. **TODO(I13):** derive from cluster identity instead. |
| `MEMQL_COCKPIT_ACTOR` | identity recorded in the audit trail (falls back to `$USER`) |

The cockpit reads this contract via `ResolveSurface` and prints a **redacted**
readiness line on every deploy, e.g.:

```
runner surface: kubeconfig=ready(/home/op/.kube/config) argocd=ready(https://argocd.staging.internal) genesis=ready
```

Secret *values* are never printed — only `ready`/`absent` and the non-secret
ArgoCD server URL + kubeconfig path. A dry-run never requires any of these; a
live deploy with an incomplete surface prints a `WARNING` listing what is
missing (and is a no-op until I13).

### Operator machine

```bash
export MEMQL_COCKPIT_KUBECONFIG="$HOME/.kube/staging.config"
export MEMQL_COCKPIT_ARGOCD_SERVER="https://argocd.staging.internal"
export MEMQL_COCKPIT_ARGOCD_AUTH_TOKEN="$(az keyvault secret show --vault-name <kv> --name argocd-staging-token --query value -o tsv)"
export MEMQL_COCKPIT_GENESIS_ENVELOPE="$(az keyvault secret show --vault-name <kv> --name genesis-envelope-staging --query value -o tsv)"
export MEMQL_COCKPIT_ROLE=developer

memql-cockpit deploy --env=staging --ref=0.9.0 --dry-run   # preview, no side effects
memql-cockpit deploy --env=staging --ref=0.9.0             # live (owner-gated; no-op until I13)
```

> **Never** put these in a committed `.env`, shell rc checked into git, or a
> repo file. Source them per-session from Key Vault as above.

### CI runner

Set the credentials as **`staging` GitHub Environment secrets** (so they are
gated behind the environment's required-reviewer approval):

- `MEMQL_COCKPIT_KUBECONFIG_B64` — base64 of the kubeconfig (the workflow
  decodes it to `~/.memql/kubeconfig`)
- `MEMQL_COCKPIT_ARGOCD_SERVER`
- `MEMQL_COCKPIT_ARGOCD_AUTH_TOKEN`
- `MEMQL_COCKPIT_GENESIS_ENVELOPE`

The `MEMQL_SIBLING_TOKEN` secret (already used by CI) lets the workflow check
out the `znasllc-io/memql` sibling for the `../memql` replace directive.

---

## 3. CI deploy job

`.github/workflows/deploy-staging.yml` runs `memql-cockpit deploy
--env=staging`:

- **Triggers:** `workflow_dispatch` (inputs: `ref`, `dry_run` default `true`)
  and `release: published` (always dry-run on release).
- **Gate:** runs in the `staging` GitHub Environment. Configure **required
  reviewers** on that environment so the **owner approves** before the job
  starts. A live (`dry_run=false`) run is therefore owner-gated.
- **No-op safety:** with no creds set, the job builds the cockpit and runs the
  deploy as a redacted no-op. Exit code `3` (automation/runner surface pending
  I10/I13) is treated as a non-fatal notice so the wired-but-not-yet-live state
  does not fail the run.

### Going live (owner checklist) — `TODO(owner)`

1. Land **I13 (memql#2220)** so the deployment automation's capability actions
   resolve to a fully-wired runner surface (today a live run resolves the
   automation but its DB-backed steps no-op).
2. Create the `staging` GitHub Environment and add the **owner** as a required
   reviewer.
3. Add the four runner-surface secrets above to the `staging` environment,
   sourced from Key Vault.
4. Dispatch **Deploy staging (cockpit)** with `dry_run=false`, approve the
   environment gate, and confirm the deploy reaches the cluster.

Until step 1, keep `dry_run=true`.
