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
the Clusters / Concepts / Settings tabs, full F1..F3 navigation. This
is what `app.go` orchestrates today; documented in the rest of this
file. (Previous Explorer + Agents tabs were folded into the unified
Concepts tab on 2026-05-16 / 2026-05-21 respectively -- the generic
row browser supersedes both; the Agents-tab retirement landed when
the Concepts surface started consuming @displayCard hints per
memql-cockpit#126.)

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

1. **Clusters (F1)** -- cluster manager + topology
2. **Concepts (F2)** -- generic browser for every registered concept.
   Renders rows + detail uniformly per concept by consuming the
   `@displayCard(...)` hints memql core publishes on
   `ConceptInfo.display_card` (memql#160). v1:agents:agent rows live
   here too -- the dedicated Agents tab was retired in
   memql-cockpit#126 once Concepts could render them well.
3. **Settings (F3)** -- credentials, theme, version

The Concepts tab is gated on a connected, selected cluster -- it shows
a placeholder message until the user presses Enter on a cluster row in
the Clusters tab.

> The Planner, Skills, Workers, Safety, and Bundles tabs were removed in
> memql-cockpit#216 -- the cockpit is scoped to cluster management +
> concept browsing + settings for now. The `memql-cockpit worker run`
> computer-use daemon (and its `worker setup` / `worker consent`
> subcommands) is unaffected; only the in-TUI Workers monitor panel +
> the global Ctrl+E consent kill switch went away. See docs/computer-use.md
> for the daemon, which stays.

### Concepts tab layout

Three panes (left -> right): concept picker, row list with search,
generic detail renderer. The detail pane is "Hybrid C" -- no concept-
specific rendering, just a recursive walk of the row's payload +
provenance + intrinsics. New concepts work the day they're declared
without a renderer update. Press `v` on a selected row to swap the
detail pane to the time-series version history; `Esc` returns.
`:` jumps to the search box (filters the row list in-memory). The
prompt + active query render in the pane's bottom chrome band -- never
as a strip below the title -- per the [Panel chrome contract](#panel-
chrome-contract) below. Tab cycles focus between the three panes.

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
└──────────────────────────┴───────────────────────────────────┘
```

- **Left pane**: cluster manager (list + detail + add/edit form).
- **Right pane**: a **persistent vertical split** (memql-cockpit#221) --
  the live topology grid on TOP and the per-cluster Deployments section
  anchored to the BOTTOM, both always visible. The top region can be
  swapped for the architecture-model drill-down navigator with `X`.

### Persistent split (memql-cockpit#221)

The right pane is a vertical split with two always-on regions:

```
┌───────────────────────────────────┐
│ local                             │  cluster-name title
│   [bff] ── [cognition]            │  TOPOLOGY region (live grid)
│        \                          │
│         [agent]                   │
│  Nodes: 4  Online:4   WASD:Pan …  │  one-line topology tally + pan hints
│ ───────────────────────────────── │  divider
│ DEPLOYMENT HISTORY                │  DEPLOYMENTS region (bottom band)
│  succeeded 2026.6.21 staging azure│
│  ─────                            │
│ DEPLOYMENT dep-aaaa  succeeded    │  detail block (selected deployment)
│ Deployments: 2     Up/Dn:Move … │  deployments hint bar (last row)
└───────────────────────────────────┘
```

The split layout lives in `cluster/topology.go` `View.Draw`:
the deployments band height is `clamp(40% of pane, min 8, max 14)`
rows, bottom-anchored, with a 1-row `─` divider above it and the
topology grid filling the rest. `drawTopology` is handed only the top
sub-`Rect`, which is what stops the grid from bleeding into the
deployments band (the overlap bug #221/#220 fixed). There is **no
toggle** -- both regions are always present. (A graphical topology
preview of the *selected* deployment is a follow-up; for now the
selected deployment's node composition is shown textually in the
deployments detail block -- see the `TODO(#221)` in `View.Draw`.)

### Deployments section (memql-cockpit#207)

The bottom band is the concept-driven **Deployments section**. It is
the integrative deliverable of the Deployment & Topology Overhaul
(epic memql#1871) and reads the `v1:cluster:deployment` concept rows
landed in that epic.

- **History list** -- `DeploymentsForCluster` (clusterId resolved
  via `ExistingCluster`), rendered newest-first by createdAt with
  a status token (succeeded / in-progress / pending / failed /
  superseded / rolled-back) + version + env + provider.
- **Select -> topology** -- `Enter` on a row loads that deployment's
  nodes (`NodesForDeployment`) and the orphans
  (`NodesNotInDeployment`), rendered as a count-vs-expected
  summary + per-node health/version, with orphans flagged in the
  warning color.
- **Controls** (SDK `DeployControlClient` wrappers): `C` cut-version
  (env -> bump picker, previews `SuggestNextVersion`, then
  `CutVersion`), `G` deploy the selected pending deployment
  (`Deploy`), `B` roll back to the selected succeeded deployment
  (`RollbackDeployment`, type-to-confirm). Role matrix (locked,
  epic #1871): view = any role; cut/deploy = developer + admin +
  owner (`client.RoleDeveloper`/`RoleAdmin`/`RoleOwner`); rollback =
  owner only. Disallowed controls are hidden/disabled in the hint
  bar and the keys are no-ops.

The **live topology** (top region) also got truthfulness upgrades from
the same issue: each node box carries a third line (running version +
short deploymentId, or an `[orphan]` tag), orphaned/stale nodes
(stopped, or carrying a non-current deploymentId) render with the
warning border, the topology tally flags any node type below the
expected replica count (`expectedNodesPerType`), and `parentId`
discovery links are drawn as edges on top of the static
service-relationship map.

Files: `cluster/deployments.go` (section view + history + per-
deployment topology), `cluster/deployments_controls.go` (cut/deploy/
rollback modal), `cluster/deploy_shared.go` (shared async-fire +
gRPC-status + draw helpers the section reuses). The app wires the SDK
closures in `wireCluster` (`OnDeploymentsShown` / `OnSelectDeployment`
/ `OnDeploymentsChanged`), refreshes the history on a periodic
`deploymentsRefreshLoop`, and parses rows in `app.go`
(`parseDeployments` / `parseDeploymentNodes`).

> The earlier **deployment-v2 surface** (an always-on STAGING/PROD
> Argo/Rollouts status strip + a `D`-key DeployStaging/Promote/Rollback
> modal; files `cluster/deploy.go` / `deploy_controls.go` /
> `deploy_grpcstatus.go` plus `app.go`'s `refreshDeployStatus` poll) was
> removed in memql-cockpit#221 when the persistent split landed. The
> concept-driven Deployments section above is the single deployment
> surface now.

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

Every configured cluster gets a `connEntry` in `a.pool[clusterName]`
(see `pool.go`), but **only ONE entry holds a live connection at a
time -- the selected ("working") cluster** (epic #239). This is the
single-live-connection model; the rest are listed but not dialed.

### Single live connection (the rule)

- **Boot** (`connect()`): opens the full lifecycle for the **selected**
  cluster only (restored from `~/.memql/clusters.yaml`). Every other
  cluster is **registered** (`registerEntry`) into the pool as metadata
  -- the row renders with its config-derived status, but no lifecycle
  goroutine, no stream, no subscriber, no token refresher, no initial
  load. This is what kills the old boot-time N-cluster dial storm.
- **Selection drives the connection** (`setSelected`, CM2): pressing
  Enter on a cluster makes it the working cluster AND brings it up.
  The previously-selected cluster is **demoted** (`demoteToIdle`) back
  to a registered-idle row -- its full connection (stream + subscriber
  + refresher) is torn down. So the single live connection FOLLOWS the
  selection. The teardown's `Close()` runs OFF the UI thread
  (`replaceWithRegistered`) so a switch never freezes the event loop on
  the stream-drain wait (cf. #191).
- **`setViewed` (arrow-key highlight) is side-effect-free** -- it only
  repaints the topology pane for the highlighted cluster; it never
  connects. A non-selected, non-connected highlight shows status + an
  `Enter:Connect` hint, not a stale topology.

### Live-connection lifecycle (the selected cluster)

The selected cluster's entry runs a lifecycle goroutine driving an
`entryState` machine:

| State       | Meaning                                                    |
|-------------|------------------------------------------------------------|
| Idle        | Registered, not dialed (every non-selected configured row).|
| Connecting  | Active dial in flight.                                     |
| Connected   | Stream up; subscribers running.                            |
| Backoff     | Attempt N failed; sleeping before attempt N+1 (15/30/45s). |
| Failed      | Cycle exhausted (3 attempts). Waits for manual `R:Retry`.  |
| NeedsConfig | Missing endpoint/auth -- `L:Authorize`. Never dials.       |
| NeedsToken  | Configured but no cached token -- `L:Login`. Never dials.  |

`connEntry.lifecycleStarted` records whether `runLifecycle` was
launched; registered-idle entries never start one, and `Close()` skips
the stream-drain wait for them so quit stays fast across N clusters.

The lifecycle uses **bounded retry**: at most 3 attempts per cycle with
linear backoff (15s → 30s → 45s). After failure the entry sits in
`Failed` until the user explicitly retries -- no infinite reconnect
storms. On stream loss after a successful connect the entry transitions
back to `Backoff` and re-enters the cycle automatically. The CLI
distinguishes intentional shutdowns from stream errors via
`Dispatcher.Done()` vs `Dispatcher.Unexpected()`; switching clusters
via `Close()`/`Cancel()` does NOT trigger reconnect, only
`Unexpected()` does.

### Liveness probe (the non-selected clusters)

Registered-idle clusters get a lightweight periodic **liveness probe**
(`livenessProbeLoop`, CM3) so their row shows an at-a-glance up/down
dot without a full connection. One shared ~25s ticker; each sweep
probes every eligible (non-selected, `stateIdle`) cluster through a
bounded worker pool (`probeConcurrency`) -- no probe storm. A probe is
a single short-timeout `client.Connect` + immediate `Close`: **no
token, no subscribe, no initial loads, no refresher**. The handshake
completes without credentials and an auth rejection still proves
reachability, so only a transport failure (dial refused / TLS /
timeout) counts as down. The result lives on `connEntry.probe`
(`probeStatus`, distinct from the lifecycle `State`) and maps onto the
existing `available` (reachable) / `unreachable` (down) row dots.
`setProbe` only triggers a redraw on a real transition, so a
steady-state sweep produces zero frames.

### Redraw coalescing (no flicker)

All of the above churn collapses into minimal frames: `postRedraw`
coalesces bursts via the `redrawPending` atomic (only the first caller
in a quiet window posts an `EventInterrupt`; the event loop clears the
flag before `draw()` so nothing is dropped), and the probe's `setProbe`
change-gate means re-confirming the same reachability emits no redraw.
The single-connection invariant + stable-frame behavior is locked by
`cli/single_connection_invariant_test.go`.

---

## Key Bindings

### Global
| Key      | Action                                                         |
|----------|----------------------------------------------------------------|
| F1..F3   | Switch tab                                                     |
| Ctrl+Q   | Quit                                                           |
| Ctrl+T   | Cycle theme                                                    |

### Clusters tab (Cluster Manager focus)
| Key   | Action                                              |
|-------|-----------------------------------------------------|
| ↑/↓   | Move highlight (also moves topology view)           |
| Enter | Make highlighted the working cluster -- connects it if not already live, tears the previous one down to probe-only (single live connection, #239) |
| A     | Add a new cluster                                   |
| E     | Edit highlighted cluster                            |
| D     | Delete highlighted cluster (not "local")            |
| Esc   | Cancel an in-flight dial                            |
| R     | Manual retry after Failed                           |
| Tab   | Cycle: Cluster Manager → Topology                   |

### Add/Edit cluster form (single Domain field)

The cluster Add/Edit form collects ONE value: a **Domain** (e.g.
`staging.copresent.ai`). Endpoint / Issuer / ClientId are composed
from it by convention on save (`composeFromDomain` in
`cluster/clusters_view.go`), the same convention
`autoSeedLocalFromGenesis` uses for the local row:

```
Endpoint = https://bff.<domain>
Issuer   = https://identity.<domain>
ClientId = cockpit
Name     = slug(<domain>)   (dots -> dashes; the clusters.yaml key)
```

The form previews `bff.<domain>` / `identity.<domain>` live as you
type. `Name` is the immutable config key (preserved across edits so a
cached token isn't orphaned); the Domain is stored on
`ClusterConfig.Domain` so Edit round-trips it. Authorization is NOT
part of the form -- after save the row is `needs-login`, and
`L:Authorize`/`L:Login` runs OAuth against the composed Issuer +
ClientId.

Deployments that DON'T follow the `bff.`/`identity.` convention
(local plaintext `host:port`, custom hostnames, a PAT, a different
client id) are edited directly in `~/.memql/clusters.yaml` -- the form
is intentionally convention-only. (The previous multi-field form +
its discovery-URL / well-known fetch path were removed; see
memql-cockpit#197.)

| Key       | Action                              |
|-----------|-------------------------------------|
| (type)    | Edit the Domain value               |
| Enter     | Compose + Save                      |
| Esc       | Cancel                              |

### Clusters tab (Topology pane focus)

The right pane is a persistent split: the topology grid (top) and the
always-on Deployments section (bottom). The topology pan/reset/arch
keys and the deployments navigation/control keys are both live at once
(they don't collide -- `B`/`C`/`G` + arrows drive deployments; `WASD`
pans the grid). There is no toggle.

| Key         | Action                                                  |
|-------------|---------------------------------------------------------|
| WASD        | Pan the live-topology grid (top region)                 |
| R           | Reset pan to origin                                     |
| X           | Toggle Architecture navigator (drill-down, top region)  |
| ↑/↓         | Move the Deployments history cursor (bottom region)     |
| Enter       | Load the selected deployment's topology (nodes/orphans) |
| C           | Cut a new version (developer/admin/owner)               |
| G           | Deploy the selected pending deployment (developer+)     |
| B           | Roll back to the selected succeeded deployment (owner)  |

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
| `cluster/topology.go`    | Right-pane persistent split: live topology grid (top) + Architecture toggle + `View.Draw` split layout |
| `cluster/deployments.go` | Deployments section (bottom band): history list + per-deployment detail/topology |
| `cluster/deployments_controls.go` | Cut/deploy/rollback concept modal for the Deployments section |
| `cluster/deploy_shared.go` | Shared deploy helpers (async-fire outcome, gRPC PermissionDenied mapping, colored-token / wrapped-text writers) reused by the Deployments section |
| `cluster/architecture.go`| `ArchView`: drill-down navigator over `topology.model.json`   |
| `cluster/metrics_fetcher.go` | `QueryClientMetricsFetcher`: codeMetric overlay fetch      |
| _gRPC client_            | Lives in `memql/sdk/go/client/` -- `Connection`, `Dispatcher`, `QueryClient`, `SubscriptionManager`. Cockpit imports the SDK rather than reimplementing the wire layer. |
| `sense/`                 | `SenseClient` over the SDK Dispatcher -- editor-side wrappers for MemQL Sense (Tokenize / Diagnose / Complete / Hover). |
| `config/clusters.go`     | `~/.memql/clusters.yaml` load/save                            |
| `concepts/`              | Concepts tab (concept picker + row list + generic renderer)   |
| `editor/`                | Reusable text editor with Sense integration                   |
| `settings/`              | Settings tab                                                  |
| `ui/`                    | Theme, screen, tab bar, layout primitives                     |

---

## List-pane conventions

Any pane that renders a list of selectable items (clusters, future
analogues) should follow the same UX rules so keybinding hints stay
consistent and predictable:

- **Highlight vs selection are two axes.** The "highlight" is the
  cursor — moves with arrow keys, never persists. The "selection"
  (or "active") is the chosen item — marked with `*` (strict
  single-cell ASCII, see "Layout-edge glyph rule" below), persists
  in config, drives downstream behavior (working cluster, etc.).
- **`Enter` always means "promote highlighted to selected".** Never
  overload it with secondary behavior like "and also retry". Other
  keys do other things.
- **Hint strip is context-aware.** `Enter:Select` only appears when:
  - The highlighted item is in a state where selection makes sense
    (e.g. cluster is `connected`).
  - The highlighted item is **not already the selected one**
    (Enter on the active row is a no-op, so don't advertise it).
  Lifecycle / repair keys take Enter's place when applicable
  (`Esc:Cancel` while connecting, `R:Retry` when failed, etc.).
- **Sticky pinned + sorted rest.** When a list has a fallback
  invariant (cluster `local`), pin it at index 0 with a `─`
  divider, sort the rest alphabetically, and keep the pinned row
  visible across scroll. The scrollable region is scoped to the
  rest. See `ClustersView` Draw method + `ui.ScrollTo` /
  `ui.DrawScrollbar`.
- **Pane chrome stays pinned.** Action hints, optional confirmation
  prompts, detail block (when present) all live in fixed-height
  bands at the bottom -- only the row list scrolls inside what's
  left. Use `ui.DrawBottom` / `ui.DrawBottomBlocks` for the chrome.

When you add a new list-bearing pane, copy these conventions.
Reference implementation: `cli/cluster/clusters_view.go`.

---

## Panel chrome contract

**Every interactive pane shipped by the cockpit follows the same
chrome layout, no exceptions.** This is the contract reviewers should
point at when something else gets added. It exists because the
Clusters tab established a pattern the rest of the UI quietly drifted
away from -- search strips below titles, count footers stuck at the
bottom row, ad-hoc hint formats. Anything that diverges is a
regression.

### The bands (top -> bottom)

```
[title]      Pane title + cursor/count. One row, split into two
             halves: the bare pane name flush LEFT (anchored at
             bounds.X+1), and the cursor/count flush RIGHT (last
             cell at bounds.X+Width-2). Rendered via `ui.PaneTitle`
             -- panes must NOT hand-roll the title row. Examples
             (`·····` is the right-aligned gap, not literal cells):

                 CLUSTERS·······································
                 CONCEPTS····································1/77
                 ROWS·······················3/12 filtered from 40
                 DETAIL·································line 4/27
                 SPACES········································5

             Rules:
               * No parentheses around the counter.
               * No embedded ids, names, or colons in the title --
                 the title is the pane's identity, not the row it
                 happens to be showing. `ROWS: v1:cognition:space`
                 and `CHAT: <spaceName>` are the anti-patterns this
                 split exists to prevent; the highlighted row in
                 the adjacent pane already carries that id.
               * Counter is supplementary. When title + a 2-cell
                 gap + counter doesn't fit, `ui.PaneTitle` drops
                 the counter first -- the title wins. Use
                 `ui.FormatCursor` / `FormatFiltered` / `FormatLine`
                 / `FormatCount` to build the counter string.
               * Focused pane renders in accent-bold; unfocused in
                 subtle-bold. `PaneTitle.Focused` flips both halves.
               * Form overlay titles rewrite in place
                 (" ADD CLUSTER ", " EDIT CLUSTER ") -- no second
                 title stacked inside the form, no counter.

[content]    The scrollable list / detail / form body. Whatever
             vertical space remains after subtracting the title +
             optional detail + chrome bands.

[detail]     (optional) Pinned detail block for the highlighted row
             -- Endpoint/Auth/Status/Node for clusters, etc. Lives
             ABOVE the chrome with a one-row gap; rows that have
             nothing to show stay blank so the list viewport keeps
             a stable height as the user arrows around.

[chrome]     Anchored to the LAST row(s) of the pane via
             `ui.DrawBottom` / `ui.DrawBottomBlocks`. Holds action
             hints, search input when active, or a confirmation
             prompt (delete / discard / etc.). NEVER a count
             footer -- that lives in the title.
```

### Action-hint format

Hints in the chrome band are a single line of `Key:Label` chips
joined by **two spaces** (no commas, no pipes, no "Press X to Y"
prose):

```
A:Add  E:Edit  Enter:Select  D:Del
↑/↓:Move  Enter:Open  :Search  v:Versions  Tab:Cycle
```

Rules:
- **`Key:Label`** -- no space around the colon. `Key` is the literal
  key (or key combo) the user presses; `Label` is a single
  whitespace-free verb or noun (use CamelCase for compounds:
  `Esc:ClearSearch`, `Esc:CloseVersions`). `Enter:Save` not
  `Enter: Save` and not `Enter (Save)`.
- **Single-letter keys are UPPERCASE** -- `R:Refresh` not `r:Refresh`,
  `X:Export` not `x:Export`. Keystroke handlers stay case-tolerant
  (`'x' || 'X'`), but the chip displays the canonical uppercase form
  so it reads as a single recognizable shape across every pane.
  Multi-key combos (`↑/↓`, `PgUp/PgDn`) and named keys (`Esc`,
  `Enter`, `Tab`) keep their natural casing; this rule applies to
  single-letter alphabetic keys only.
- **Two-space separator** between chips. Single space would visually
  merge into the labels; commas/pipes have been tried and read worse.
- **Context-aware.** A chip only appears when the action it
  advertises is currently available -- `Enter:Select` only on
  not-already-selected rows, `D:Del` only when the row is deletable,
  `Esc:Clear search` only when a filter is active. Hints that lie
  rot trust.
- **No `Tab:Switch panes` duplication.** Tab cycling is universal
  and lives in the top-of-screen header chrome
  (`cli/ui/header.go`); pane-local hints can still echo
  `Tab:Next pane` when there's room, but don't treat it as
  mandatory or as the first chip.

### Search input

Panes that filter their content use a single, consistent invocation:

- **Trigger key: `:`** (colon). NOT `/`. The colon convention
  threads through the same `Key:Label` grammar action hints use --
  the bottom-band chip reads `:Search`, and pressing colon enters
  the input. `/` is reserved (and not currently bound) so future
  surfaces can repurpose it without colliding with the search
  mental model.
- **Render location: the chrome band, never a strip under the
  title.** When the user presses `:`, the bottom band swaps from
  action hints to a vim-style prompt:

  ```
  :search <query>_
  ```

  rendered in the accent style so it's clear input is being
  captured. `Esc` or `Enter` exits the input; `Esc` on the rows
  pane with a non-empty filter clears the filter (advertised as
  `Esc:Clear search`).
- **Don't reserve a permanent search row.** Painting "/search:"
  below the title before the user has asked to search wastes a
  row, breaks the chrome contract, and trains the user to look
  for search in the wrong place.

Reference implementation: `cli/concepts/view.go`
(`drawBottomHints` + `hintsForRows`).

### When you add a new pane

1. Title shows pane name + counts/positions.
2. Action hints render via `ui.DrawBottom(screen, bounds,
   subtle, 1, hint)` anchored to the last row.
3. If the pane filters content, search is invoked by `:` and the
   prompt renders inline in the chrome band.
4. Add a regression test in `cli/concepts/chrome_contract_test.go`
   style: instantiate the pane, render against a
   `tcell.NewSimulationScreen`, assert hint text lands in the last
   row and search behaves as specified.

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
  - The "active row" marker on `ClustersView` (right-edge, two
    cells from the divider).
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

## SDK-only rule

Every wire call from `cli/**` goes through `memql/sdk/go/` --
`client/` for queries / mutations / subscriptions, `sense/` for
editor language intelligence, and `worker/` once #117 lands.

What that means concretely:

- **No direct `grpc.NewClient` dials** anywhere under `cli/**` or
  `cmd/memql-cockpit/internal/worker/`. The SDK owns transport
  (TLS, auth-token plumbing, request correlation). Worker dials
  go through `sdk/go/worker.Dial` (which carries the TLS config +
  env-var knobs from memql#117); the cockpit's worker run-mode
  keeps only the worker-protocol lifecycle (Register / Heartbeat /
  ToolResult) on its side.
- **No `memqlv1` imports** under `cli/**`. The previous leak set
  (`app.go`, `pool.go`, `settings/view.go`, `concepts/view.go`,
  `concepts/chrome_contract_test.go`, `settings/settings_race_test.go`)
  migrated to the SDK-owned wrapper types in memql#115. The rule is
  now load-bearing -- the next `memqlv1` import has to either belong
  inside the SDK or be added with a justification.
- **No raw DSL strings.** Never `qc.Execute("queryActiveSpaces({})")`
  and never `qc.Execute("concept==v1:cognition:space; ...")`.
  Consumers call the typed generated methods on `QueryClient`
  (`QueryActiveSpaces`, `QueryAllPlans`, etc.). The engine reserves
  the right to evolve internal projection / bundle shapes without
  breaking clients -- that promise only holds if cockpit stays on
  the named-primitive surface. Bypassing it is how
  memql-cockpit#49 happened.

  The lone exception is the Concepts tab's concept-browser surface
  (`BrowseConcept`, `GetRowByConceptAndId`). Those are SDK-owned
  methods, not raw-string escape hatches -- they exist precisely
  because a concept-agnostic browser has no compile-time named
  primitive to call.

**If you can't express something through the SDK, the SDK needs to
grow, not the cockpit.** Open an issue under memql/, add the
typed method (DSL function -> `make sdk-gen` regenerates the
client) or the wrapper type, then come back here.

See `memql/sdk/go/CLAUDE.md` for the full SDK contract (named
primitives, opaque types, generated-code rules, layout).

## Wire Contract

Every gRPC envelope leaves the CLI carrying:

- `message_id` -- correlation id (UUID per request).

Authorization happens per row inside the DSL post-#56; the cockpit
no longer stamps anything tenant-scoping onto the envelope.
