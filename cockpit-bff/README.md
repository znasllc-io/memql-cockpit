# cockpit-bff

The memQL Cockpit's own **backend-for-frontend** node — the product-neutral
engine `bff` role, owned by this repo. Part of the per-client BFF model
(znasllc-io/memql-cockpit#288): the engine stays client-agnostic and every
client (thin SPA, thick TUI) gets its own BFF edge. The CoPresent SPA has
`memql-bff-copresent`; the Cockpit connects to **this** node at
`cockpit.<domain>` (gRPC).

It is a thin carrier: `main.go` wraps the engine's shared `app.Run` lifecycle
and blank-imports **no** product DSL — so no product code reaches the binary.
Behaviour tracks the `memql` engine; cockpit-specific server-side projections,
if ever needed, plug in here through the engine's `RegisterTree` /
`RegisterPlugin` seams.

## Build

Local (against the sibling engine checkout via `replace ../../memql`):

```bash
GOWORK=off go build -tags bff -o bin/cockpit-bff .
```

The deployable image builds on the **GitHub build server** (OIDC → ACR
`acrmemql`), never locally — see `.github/workflows/build-cockpit-bff-image.yml`
(`workflow_dispatch` with a `version` input). This mirrors the hard rule that
staging/prod images are build-server-produced.

## Deploy

The `cockpit-bff` node + its `cockpit.<domain>` gRPC front door are defined in
this repo's `deploy/` overlays (see #290). The Cockpit's connection contract
targets `cockpit.<domain>` (see #291).
