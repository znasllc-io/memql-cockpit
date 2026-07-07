# cockpit-bff deployment

The memQL Cockpit's own backend-for-frontend node — the product-neutral engine
`bff` role built from this repo's [`cockpit-bff/`](../../cockpit-bff/) module.
Part of the per-client BFF epic (znasllc-io/memql-cockpit#288): the Cockpit
connects to **`cockpit.<domain>`** instead of borrowing the product SPA's
`bff.<domain>`.

## Files

| File | What |
|------|------|
| `cockpit-bff.yaml` | `cockpit-bff` Deployment + Service (engine `bff` role, `:50051` gRPC) |
| `cockpit-front-door.yaml` | nginx gRPC Ingress fronting `cockpit.<domain>` → `cockpit-bff:50051` |

## Coexistence design (locked #288)

A cluster can run **several** BFFs side by side — one per client. `cockpit-bff`
is a **distinct service** so it never collides with the product `bff`:

- It carries **no** `MEMQL_WORKER_PEERS` and does no AI forwarding — worker
  nodes parent to `bff-active:50058` (the product bff), never here.
- It serves only the Cockpit's read surface (topology / concepts over
  `MemqlService`). Deploys ride the `deployEngineCluster` automation path
  (#292), not a gRPC call over this connection — so it needs no
  `DeployControlService` either.

## Bring-up

`cockpit-bff` layers over an engine cluster through the engine's downstream
carrier hook (`memql/docs/public/operate/downstream-stacks.md`). The engine
builds the node image from **this** repo's `cockpit-bff/Dockerfile` and places
this overlay:

```bash
# From the workspace root, over a running (or fresh) engine cluster:
make -C ../memql up \
    CARRIER_REPO="$(pwd)" \
    CARRIER_NODES=cockpit-bff \
    OVERLAY_PATH=deploy/k8s \
    APP_NAME=cockpit-bff \
    EXTRA_PORTS=50051:50051
```

Locally there is no public DNS/cert, so reach the node via port-forward and
point the Cockpit at the plaintext endpoint (until #291 makes
`cockpit.<domain>` the default):

```bash
kubectl port-forward -n memql svc/cockpit-bff 50051:50051
# then set the cluster endpoint to localhost:50051 in ~/.memql/clusters.yaml
```

## Owner-validated (build server + cluster)

Per the hard rules, the deployable image builds on the **GitHub build server**
(`build-cockpit-bff-image.yml`, OIDC → ACR), never a dev laptop; and the
running node + the `cockpit.<domain>` front door validate on a **cluster**
(staging is currently torn down). The env placeholders here
(`identity.<domain>` issuer, image tag, DB/genesis via `memql-secrets` /
`memql-db-pool`) are patched per environment exactly like the engine base
manifests — tune them to your cluster before deploy.
