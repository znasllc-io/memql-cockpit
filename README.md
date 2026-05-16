# memql-cockpit

Terminal-native IDE and operations console for memQL clusters.

Lifted from `github.com/visionarys-io/memql` on 2026-05-14 as part
of the monorepo carve-up. The cockpit communicates with memQL
clusters over gRPC (`MemqlService.Stream` and `NodeService.Stream`)
and does not embed the memQL engine.

## Build

```bash
make cockpit          # headless variant (default, ships everywhere)
make cockpit-gui      # GUI variant with screenshot/mouse/keyboard
                      # (requires CGO + RobotGo deps -- see Makefile)
make cockpit-all-platforms       # cross-compile to darwin/linux x arm64/amd64
make cockpit-gui-all-platforms   # GUI variant, all platforms
```

Output lands under `bin/`.

## Run

```bash
./bin/memql-cockpit                # main IDE (multi-tab TUI)
./bin/memql-cockpit worker run     # run as a per-user worker (computer_use)
./bin/memql-cockpit-gui worker setup  # one-time GUI worker setup wizard
```

Cluster config lives at `~/.memql/clusters.yaml`; worker config at
`~/.memql/worker.yaml`. The install scripts under `scripts/install/`
register a LaunchAgent (macOS) or systemd user service (Linux).

## Module structure

- `cmd/memql-cockpit/` -- binary entry point + per-subcommand internals
  (`internal/authorize/`, `internal/lint/`, `internal/worker/`).
- `cli/` -- TUI primitives (`ui/`, `canvas/`) + product views
  (`agents/`, `auth/`, `client/`, `cluster/`, `config/`,
  `editor/`, `explorer/`, `settings/`).
- `scripts/install/` -- platform installers.

## memQL core dependency

This module depends on `github.com/visionarys-io/memql` for:
- `component/grpc/gen` -- generated proto types (wire surface)
- `component/node/gen` -- generated node proto types
- `component/node` -- node client / connection primitives
- `component/identity/workerpairing` -- worker pairing protocol
- `component/memql/dslimports` -- DSL import resolution (for `lint`)
- `core/id` -- canonical id validation

During local development the `replace` directive in `go.mod` points
at a sibling `../memql/` tree. Once memql core is published with a
real version tag, drop the replace and pin the version.
