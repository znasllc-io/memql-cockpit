# CLI Directory

**Purpose:** memQL Cockpit -- a tcell-based TUI for managing clusters,
exploring concepts, browsing agents, authoring MemQL, and every
other interactive flow shipped from `memql-cockpit`.

**Binary:** `memql-cockpit` (built from `cmd/memql-cockpit/`).
**Display name:** "memQL Cockpit" -- shown in the header chrome and Settings.
**Language:** Go
**Entry point:** `app.go` (top-level App orchestrator)

---

## Canonical-TUI rule

**Every interactive surface shipped by `memql-cockpit` uses the
`cli/ui/` (tcell) and `cli/canvas/` (pixel framebuffer) primitives.**
This is non-negotiable: if a flow asks the user to make a sequence of
choices, watch a probe run, or see live state evolve, it goes through
the TUI. Sprinkling `fmt.Println` interactive flows in new
subcommands is a regression and reviewers should reject it.

The rule covers two layout patterns:

### 1. Multi-tab IDE (the operations console)

The original `memql-cockpit` invocation -- launches `cli.App`, mounts
the Clusters / Explorer / Agents / Settings tabs, full F1..F4
navigation. This is what `app.go` orchestrates today; documented in
the rest of this file.

### 2. Single-panel wizard

Used for focused, time-bounded flows like
`memql-cockpit-gui worker setup`. Same `Header` chrome ("memQL
Cockpit" branding on the left, contextual hint on the right, dark
band shared with the bottom hint bar). The tab bar is replaced with
a context-specific hint footer ("Enter:Continue R:Re-probe ..."), and
the content area renders ONE bordered panel centered in the available
space. Optional pixel vignettes live on the panel via
`cli/canvas/{Canvas,Renderer}` -- circles, lines, the 4x5 bitmap
font, paint into a `Canvas` then `Renderer.DrawCanvas` projects it
into a `Rect`. Use the same theme palette
(`Theme.Accent`/`Success`/`Warning`/`Error`/`Subtle`); never invent
colors per-screen.

Both patterns share the same primitives: `Screen` (FillRect, DrawBox,
DrawText), `Theme`, `Header`, `Notifications`, `WrapText`, `Rect`,
`FlexRow`/`FlexColumn`. Add new primitives to `cli/ui/` rather than
inlining tcell calls in feature packages.

### When NOT to use the TUI

- Pure non-interactive output: `cluster list`, `--version`, `worker
  config` (single-shot dumps that pipe well). These stay printf.
- Detected non-TTY stdin/stdout: any TUI surface MUST detect non-TTY
  (e.g. piped output, CI, install scripts) and fall back to a plain
  printf path so headless callers don't choke on tcell escape codes.
  `golang.org/x/term.IsTerminal(int(os.Stdin.Fd()))` is the canonical
  check.

### Where to put new TUI surfaces

Per-feature TUI surfaces live next to the feature they serve, but
import only `cli/ui` and `cli/canvas` -- not `cli/app`, not
`cli/cluster`, not other tab-specific packages. This keeps the
multi-tab IDE and the wizards independently buildable. Examples:

- `cmd/memql-cockpit/internal/worker/wizard_gui.go` -- worker setup
  wizard, single-panel layout (build tag `gui`).
- Future: `cmd/memql-cockpit/internal/onboarding/` -- first-launch
  flow, single-panel layout, optionally chained.

The rule of thumb: if it lives under `cli/` it's part of the
multi-tab IDE; if it lives under `cmd/memql-cockpit/internal/` it's a
focused subcommand surface that imports the TUI primitives.

---

## Tab Order

The tab bar lives at the top of the screen. Tabs are ordered so that
"connect to a cluster" is the first thing the user sees:

1. **Clusters (F1)** -- cluster manager + partition manager + topology
2. **Explorer (F2)** -- concept tree + MemQL source editor
3. **Agents (F3)** -- materialized `v1:agents:agent` directory
4. **Settings (F4)** -- credentials, theme, version

Explorer / Agents are gated on a connected, selected cluster --
they show a placeholder message until the user presses Enter on a
cluster row in the Clusters tab.

---

## Clusters Tab Layout

```
┌──────────────────────────┬───────────────────────────────────┐
│ CLUSTERS                 │ TOPOLOGY                          │
│  CLUSTER MANAGER         │                                   │
│   ▸ ● local       *      │   [bff] -- [cognition]            │
│     ○ acme               │       \                           │
│   ─────                  │        [agent]                    │
│   Endpoint  ...          │                                   │
│   Status    connected    │                                   │
│  A:Add E:Edit Enter:Sel  │                                   │
│  Tab:Partitions          │                                   │
│ ─────────────────────────│                                   │
│  PARTITION MANAGER       │                                   │
│   ▸ ● default     *      │                                   │
│     ● acme               │                                   │
│   A:Add E:Edit Enter:Sel │                                   │
└──────────────────────────┴───────────────────────────────────┘
```

The left column splits 50/50 vertically when a cluster is connected:

- **Top half**: cluster manager (list + detail + add/edit form).
- **Bottom half**: partition manager for the connected cluster.
- **Right pane**: topology diagram (current cluster's nodes + health), or
  the architecture-model drill-down navigator when toggled with `X`.

When the add/edit form opens it takes the full left column for typing
room; the partition pane re-appears on save/cancel.

### Architecture navigator (the `X` toggle)

The Topology pane has a second mode driven by the embedded
architecture model (`memql/component/architecture/embedded/`). Press
`X` (or `x`) from the live-topology view to switch in; press `X` /
`Esc` to switch back. While active:

- `Up` / `Down` move the row highlight.
- `Enter` zooms into the highlighted node (cluster -> service ->
  package -> type -> method).
- `Backspace` zooms out one level.
- Rows render as `[kind] name observable observe=level n=N p95=Xms err=Y% (count)`.
  The observability triple comes from `v1:observability:codeMetric`
  rows fetched by `QueryClientMetricsFetcher` on connection-up;
  count is the number of children inside the row's node.

Source: `cli/cluster/architecture.go` (`ArchView`),
`cli/cluster/metrics_fetcher.go` (`QueryClientMetricsFetcher`).
The live-topology grid stays the source of truth for red/green
health; the navigator is a *static + per-FQN-metrics* overlay on
the same pane, not a replacement.

---

## Connection Pool

Each cluster the user opens this session gets its own `connEntry` in
`a.pool[clusterName]` (see `pool.go`). Switching clusters is a pool
lookup, not a reconnect. Each entry runs an independent lifecycle
goroutine driving an `entryState` machine:

| State       | Meaning                                                    |
|-------------|------------------------------------------------------------|
| Idle        | Created, lifecycle hasn't tried to dial yet.               |
| Connecting  | Active dial in flight.                                     |
| Connected   | Stream up; subscribers running.                            |
| Backoff     | Attempt N failed; sleeping before attempt N+1 (15/30/45s). |
| Failed      | Cycle exhausted (3 attempts). Waits for manual `R:Retry`.  |

The lifecycle uses **bounded retry**: at most 3 attempts per cycle with
linear backoff (15s → 30s → 45s). After failure the entry sits in
`Failed` until the user explicitly retries -- no infinite reconnect
storms. On stream loss after a successful connect the entry transitions
back to `Backoff` and re-enters the cycle automatically.

The CLI distinguishes intentional shutdowns from stream errors via
`Dispatcher.Done()` vs `Dispatcher.Unexpected()`. Switching clusters
via `Cancel()` does NOT trigger reconnect; only `Unexpected()` does.

---

## Partition Selector

Every cluster has a sticky per-cluster partition choice. The selector
lives in the bottom half of the Clusters tab.

The partition the user selects scopes **domain data** (cognition
spaces/agents, HR milestones, user concepts) -- their next query /
mutation / subscription runs against rows tagged with that partition.
It does NOT hide infrastructure data: the cluster topology and the
partition list itself are global-scoped concepts
(`@scope("global")` in their `.memql` files) that live in the reserved
`_system` partition. You can switch to any partition and the clusters
pane still shows the full topology and the full partition list.

- **Source of truth**: `v1:platform:partition` rows fetched via
  `listPartitions({})` on connect, then kept fresh by a CDC subscription
  (`graph.node.created.*.v1:platform:partition`). Because the concept
  is global-scoped, these events fire under `_system` regardless of
  the subscriber's envelope partition.
- **Selection**: highlight a row + Enter. The chosen partition name is
  pushed onto the connection's `Dispatcher.SetPartition(name)` so every
  subsequent gRPC envelope auto-stamps `partition: <name>` -- the
  engine uses this for partition-scoped writes/reads but ignores it
  for global-scoped concepts.
- **Persistence**: written to `~/.memql/clusters.yaml` as
  `selected_partition: <name>` under the cluster's entry. Restored on
  next launch via `newConnEntry` -> `cfg.SelectedPartition`.
- **Default fallback**: if no selection is recorded, the entry uses
  `"default"`. The bootstrap automation
  (`automations/v1/platform/bootstrapDefaultPartition`) seeds the
  `default` partition on `system.startup`, so a fresh cluster always has
  at least one row to select.
- **Protection**: the `default` row cannot be deleted from the CLI
  (matches the `local` cluster protection).
- **Soft delete only**: `D` writes a new version with `status:
  "draining"`, which filters out of the visible list. Hard delete is
  admin-only and tracked in [docs/ROADMAP.md](../docs/ROADMAP.md).
- **Edit**: only `partitionType` is editable in the CLI today
  (Standard / Dedicated / Personal). `displayName` and `description`
  exist on the concept but are CoPresent's domain to surface; the CLI
  doesn't expose form fields for them.
- **Slug validation**: enforced at keystroke time and at server pre-
  insert (`core/id.ValidatePartitionName`), DNS-label shape, max 50
  chars, no leading underscore (`_system` is reserved).

---

## Key Bindings

### Global
| Key      | Action                              |
|----------|-------------------------------------|
| F1..F4   | Switch tab                          |
| Ctrl+Q   | Quit                                |
| Ctrl+T   | Cycle theme                         |

### Clusters tab (Cluster Manager focus)
| Key   | Action                                              |
|-------|-----------------------------------------------------|
| ↑/↓   | Move highlight (also moves topology view)           |
| Enter | Select cluster (drives Explorer/Agents)             |
| A     | Add a new cluster                                   |
| E     | Edit highlighted cluster                            |
| D     | Delete highlighted cluster (not "local")            |
| Esc   | Cancel an in-flight dial                            |
| R     | Manual retry after Failed                           |
| Tab   | Cycle: Cluster Manager → Partitions → Topology      |

### Clusters tab (Partition Manager focus)
| Key   | Action                                              |
|-------|-----------------------------------------------------|
| ↑/↓   | Move highlight                                      |
| Enter | Set as the selected partition for this cluster      |
| A     | Add a new partition (form: Name + Type)             |
| E     | Edit highlighted partition (Name read-only; Type editable) |
| D     | Soft-delete highlighted partition (not "default")   |
| Tab   | Cycle to Topology pane                              |

When the user creates a partition through the CLI, the resulting two
calls (`mutationCreatePartition` then `mutationGrantPartitionAccess`)
are issued by the app layer's `OnAdd` callback -- the form just
collects the slug + type and hands them off.

### Add/Edit forms
| Key       | Action                              |
|-----------|-------------------------------------|
| Tab       | Next field                          |
| Space     | Cycle Type (partition form only)    |
| Enter     | Save                                |
| Esc       | Cancel                              |
| Ctrl+N    | Toggle "No Auth" (cluster form)     |

### Clusters tab (Topology pane focus)
| Key         | Action                                              |
|-------------|-----------------------------------------------------|
| WASD        | Pan the live-topology grid                          |
| R           | Reset pan to origin                                 |
| X           | Toggle Architecture navigator (drill-down)          |

### Architecture navigator (X-toggled)
| Key         | Action                                              |
|-------------|-----------------------------------------------------|
| ↑/↓         | Move highlight                                      |
| Enter       | Zoom into highlighted node                          |
| Backspace   | Zoom out one level                                  |
| Esc / X     | Return to live topology                             |

---

## Files

| File                     | What                                                          |
|--------------------------|---------------------------------------------------------------|
| `app.go`                 | Top-level App: tab bar, screen loop, callback wiring          |
| `pool.go`                | Per-cluster `connEntry` + lifecycle state machine             |
| `cluster/clusters_view.go` | Clusters tab layout, focus, cluster manager                |
| `cluster/partitions_view.go` | Partition manager (bottom-left pane)                     |
| `cluster/topology.go`    | Live cluster topology grid (right pane) + Architecture toggle |
| `cluster/architecture.go`| `ArchView`: drill-down navigator over `topology.model.json`   |
| `cluster/metrics_fetcher.go` | `QueryClientMetricsFetcher`: codeMetric overlay fetch      |
| `client/dispatcher.go`   | Multiplexed gRPC stream, partition auto-stamping              |
| `client/queries.go`      | `QueryClient` (also runs mutations via ExecuteQuery)          |
| `config/clusters.go`     | `~/.memql/clusters.yaml` load/save                            |
| `explorer/`              | Explorer tab (concept tree + editor)                          |
| `agents/`                | Agents tab (list + RenderAgent detail block)                  |
| `editor/`                | Reusable text editor with Sense integration                   |
| `settings/`              | Settings tab                                                  |
| `ui/`                    | Theme, screen, tab bar, layout primitives                     |

---

## List-pane conventions

Any pane that renders a list of selectable items (clusters,
partitions, future analogues) should follow the same UX rules so
keybinding hints stay consistent and predictable:

- **Highlight vs selection are two axes.** The "highlight" is the
  cursor — moves with arrow keys, never persists. The "selection"
  (or "active") is the chosen item — marked with `*` (strict
  single-cell ASCII, see "Layout-edge glyph rule" below), persists
  in config, drives downstream behavior (working cluster / active
  partition / etc.).
- **`Enter` always means "promote highlighted to selected".** Never
  overload it with secondary behavior like "and also retry". Other
  keys do other things.
- **Hint strip is context-aware.** `Enter:Select` only appears when:
  - The highlighted item is in a state where selection makes sense
    (e.g. cluster is `connected`; partition has no state but is
    always selectable in principle).
  - The highlighted item is **not already the selected one**
    (Enter on the active row is a no-op, so don't advertise it).
  Lifecycle / repair keys take Enter's place when applicable
  (`Esc:Cancel` while connecting, `R:Retry` when failed, etc.).
- **Sticky pinned + sorted rest.** When a list has a fallback
  invariant (cluster `local`, partition `default`), pin it at index
  0 with a `─` divider, sort the rest alphabetically, and keep the
  pinned row visible across scroll. The scrollable region is
  scoped to the rest. See `ClustersView` / `PartitionsView` Draw
  methods + `ui.ScrollTo` / `ui.DrawScrollbar`.
- **Pane chrome stays pinned.** Action hints, optional confirmation
  prompts, detail block (when present) all live in fixed-height
  bands at the bottom -- only the row list scrolls inside what's
  left. Use `ui.DrawBottom` / `ui.DrawBottomBlocks` for the chrome.

When you add a new list-bearing pane, copy these conventions.
Existing implementations: `cli/cluster/clusters_view.go` and
`cli/cluster/partitions_view.go`.

---

## Layout-edge glyph rule

**Cells adjacent to a pane border (left or right edge, divider
columns, scroll-bar tracks) must use strictly single-width
glyphs.** Anything else risks the terminal rendering the glyph
as 2 cells, overflowing into the border column, and visually
shifting the divider by one position on that row -- which then
cascades into the right-pane content rendering as broken.

The trap is Unicode's **East Asian Ambiguous Width** category.
Glyphs like `◆`, `◇`, `●`, `○`, `◌`, `◯`, `▸`, `▼`, `★`, `•`
have width "Ambiguous" per Unicode; many Linux/macOS terminal +
font combinations render them as 2 cells wide even though they
look 1-wide in some IDEs. tcell tracks them as 1 cell, the
terminal paints 2, and the next character you placed gets
visually consumed.

**Safe at the edge** -- strictly Narrow (EAW=Na or N):
  - ASCII printables (`*`, `>`, `+`, `=`, letters, digits)
  - Box-drawing characters `─ │ ┌ ┐ └ ┘ ├ ┤ ┬ ┴ ┼` and their
    heavy / double variants (single-line family is EAW=Na)

**Unsafe at the edge** -- known Ambiguous, avoid:
  - Diamonds: `◆ ◇ ◈ ◊ ♦`
  - Filled circles: `● ○ ◌ ◍ ◎ ◯`
  - Arrows: `→ ← ↑ ↓ ▸ ▶ ▼ ▲`
  - Stars / bullets: `★ ☆ • ◦ ◉`
  - Most Geometric Shapes block (U+25xx)

**Where the rule applies:**
  - The "active row" marker on `ClustersView` / `PartitionsView`
    (right-edge, two cells from the divider).
  - Scroll-bar thumb / track glyphs (right-edge of any scrollable
    pane).
  - The leftmost cell of any pane that has a left border drawn
    by a sibling pane.

**Where it's relaxed:**
  - Interior cells that have at least 2 cells of buffer to any
    border. The status icon at column 3 of a 30-wide pane is
    fine; if it doubles to 2 wide, the buffer absorbs it.
  - Notification icons in the header (no adjacent border).
  - File-tree icons inside the explorer (interior).

When introducing a new edge-adjacent glyph, default to ASCII or
verify the codepoint is `EAW=Na` (see
https://www.unicode.org/Public/UCD/latest/ucd/EastAsianWidth.txt).
A comment near the call site referencing this rule is good
practice so the next refactor doesn't quietly reintroduce the
bug.

---

## Wire Contract

Every gRPC envelope leaves the CLI carrying:

- `message_id` -- correlation id (UUID per request).
- `partition` -- auto-stamped from `Dispatcher.partition` if the caller
  didn't already set it. The dispatcher's partition is updated by
  `setSelected` and by partition `Enter`.

The server uses `partition` to scope reads/writes/subscriptions. See
`docs/core/events.md` and the partition section in the root `CLAUDE.md`.
