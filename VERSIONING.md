# Versioning

memQL Cockpit follows [Semantic Versioning 2.0.0](https://semver.org/).
Versions are `MAJOR.MINOR.PATCH`:

- **MAJOR** — incompatible CLI / config / wire changes.
- **MINOR** — backwards-compatible features.
- **PATCH** — backwards-compatible fixes.

## The git tag is the source of truth

A release **is** an annotated git tag of the form `vMAJOR.MINOR.PATCH`
(e.g. `v0.9.0`) on `main`. Everything else mirrors that tag:

- The [`VERSION`](VERSION) file carries the plain semver string
  (`0.9.0`, no `v` prefix, no build suffix). It tracks the most
  recent tag and is bumped in the same commit that precedes the tag.
- The binary stamps its version at build time. `make` resolves the
  version via `git describe --tags --exact-match` (the exact tag when
  the checkout sits on one) and falls back to the `VERSION` file
  otherwise, then injects it through `-ldflags "-X main.version=..."`.
  The compiled-in `main.version` constant (`0.9.0`) is the fallback
  for `go build`-only / `go install` flows that don't go through the
  Makefile.

There is **no epoch / timestamp suffix**. Earlier builds stamped
`0.1.0-<unix-epoch>` via a `make version-stamp` target; that target
has been removed. Versions are plain semver, pinned to tags.

## How the Cockpit reports its version

```
$ memql --version
memql 0.9.0
```

The same string is sent on the worker-protocol `Register` handshake
(`cockpitVersion()` in
`cmd/memql/internal/worker/connect.go`) so the hub can see
which Cockpit a worker is running.

## Baseline: 0.9.0

`0.9.0` is the current baseline for the public beta runway. The first
stable release will be **1.0.0**, cut when the beta stabilizes. Until
then we move through the `0.9.x` / `0.x` range.

## Compatibility with memQL (compatibility, not lockstep)

The Cockpit and the memQL hub version **independently** — the Cockpit
is a client of the hub, not a piece released in lockstep with it.
What matters is the **minimum memQL / worker-protocol version the
Cockpit speaks**, which the Cockpit declares as its compatibility
floor. A Cockpit works against any hub at or above that floor.

The cross-component compatibility matrix — which Cockpit versions
speak to which hub / protocol versions — lives in memQL's hub
document:

- [`COMPATIBILITY.md`](https://github.com/znasllc-io/memql/blob/main/COMPATIBILITY.md)
  in [`znasllc-io/memql`](https://github.com/znasllc-io/memql).

When the Cockpit's minimum supported memQL / protocol version
changes, update that matrix in the hub repo and reference it here.

## Releasing

1. Land all changes for the release on `main` via PR.
2. Bump [`VERSION`](VERSION) to the new semver and merge.
3. Tag `main`: `git tag -a vX.Y.Z -m "vX.Y.Z" && git push origin vX.Y.Z`.

The tag is the release; the `VERSION` file and `main.version` constant
simply reflect it.
