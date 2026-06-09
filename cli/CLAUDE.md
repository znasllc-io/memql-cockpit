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
2. **Chat (F2)** -- single-chat-per-space utterance viewer. Today's
   daily space for the connected user (`v1:cognition:space.kind=="daily"`,
   id `daily-{userShortId}-{dateKey}`) is pinned at the top of the
   space list, auto-selected on first paint, and ensured to exist
   on connect via the dailyspace integration's `ensureForUser`
   capability. Archived/saved spaces are filtered out; archived
   rows expire and hard-delete via the existing
   `purgeExpiredArchivedSpaces` cron. `v` toggles push-to-talk:
   first press starts microphone capture (requires the `voice`
   build tag -- `make cockpit-voice`), second press releases the
   mic and the SDK finalizes the transcript via `memql-sdk-go`'s
   `voice.PushToTalk`. The status strip above the chrome shows the
   partial transcript while recording and the final text once
   resolved. Without the `voice` tag the key surfaces a clear
   "voice support not compiled in" message. Gated on a connected,
   selected cluster.
3. **Concepts (F3)** -- generic browser for every registered concept.
   Renders rows + detail uniformly per concept by consuming the
   `@displayCard(...)` hints memql core publishes on
   `ConceptInfo.display_card` (memql#160). v1:agents:agent rows live
   here too -- the dedicated Agents tab was retired in
   memql-cockpit#126 once Concepts could render them well.
4. **Planner (F4)** -- observe v1:planner:plan + child v1:planner:task
   rows. Read-only: goal submission moved to the Chat tab (the user
   talks to the assistant, which decides when to escalate to the
   planner). The one mutation that remains is R:Run on a queued
   plan -- flips it to running via mutationStartPlan. Gated on a
   connected, selected cluster.
5. **Skills (F5)** -- read-only catalog browser for `v1:agents:skill`
   rows, grouped by category + tier. Agents themselves live under the
   Concepts tab (post the memql-cockpit#126 retire); Skills earns its
   own surface because the catalog is dense enough that the generic
   browser muddles the picture. Polls `queryActiveSkillsFull` every
   30s -- the catalog is small and re-seeded only at cluster startup
   + via planner mints. Gated on a connected, selected cluster.
6. **Workers (F6)** -- computer-use consent dashboard. Long-lived
   `watch` connection to `~/.memql/worker.sock` renders the active
   consent window state + a live audit tail of every worker tool
   dispatch (newest first, capped at 256). `G` opens the duration
   picker, `R` revokes immediately. **Strict-mode per-action
   approval** (64-B): when the granted window has `strict=true` and
   the agent dispatches `workerComputer.key_type` or `.mouse_click`,
   the worker blocks awaiting an Allow/Deny in this tab. A modal
   overlay names the tool + action; `A` allows once, `D` denies.
   Pending approvals queue FIFO and the modal cycles as the operator
   responds. The 30-second default timeout (worker side) denies
   unanswered approvals; Revoke cancels every pending approval too.
   **Region exemption** (#131): a strict grant can carry a
   screen-coordinate region rect. Toggling strict on in the Grant
   modal makes `Enter` advance to a region picker (schematic of the
   screen + an arrow-key-movable / Shift+Arrow-resizable box);
   `Enter` grants with the region, `N` skips it, `Esc` steps back.
   A `mouse_click` whose cursor is inside the region skips the
   approval gate; out-of-region clicks + all `key_type` still gate.
   The region is a static rect in a fixed 1920x1080 reference space
   -- the worker has no window-bounds API yet (window_list /
   window_focus are stubs). NOT gated on a cluster connection --
   the IPC is local-host between cockpit processes. Per
   memql-cockpit#64. A global `Ctrl+E` kill switch in
   `dispatchEvent` calls into `workersView.Revoke()` from any tab
   without making the operator switch tabs first.
7. **Safety (F7)** -- Command Safety view: paginated decision list
   over `v1:safety:classification` rows, with filter chips
   (decision / source / tier / surface / mode), a drill-down detail
   pane (redacted args + full reason + rule id), and an aggregate
   strip (totals + breakdown by decision / source / mode). Backs the
   classifier rollout (memql#235) by letting operators decide when to
   flip `MEMQL_COMMAND_CLASSIFIER_MODE=enforce` per surface with
   FP/FN data instead of by gut feel. Polls
   `queryAllSafetyClassifications` every 5s. Per memql-cockpit#134.
   Gated on a connected, selected cluster.
8. **Bundles (F8)** -- read-only browser for the planner-authored
   automation bundles (`v1:authoring:bundle` + `v1:authoring:construct`)
   the authoring capture spine (memql#1160 / #1161) produces -- one
   reproducible, inspectable bundle per completed everyday task. Left
   pane lists the caller's bundles (newest first); the detail pane shows
   the bundle's lifecycle metadata + the raw authored `.memql` source of
   every member construct. `X` exports the selected bundle's source to
   `~/.memql/bundles/<bundleId>/` (one `.memql` per construct + a
   MANIFEST). Polls `queryAuthoringBundlesForOwner` every 15s; member
   constructs load lazily per selected bundle via
   `queryAuthoringConstructsForBundle`. Gated on a connected, selected
   cluster. Per memql#1162.
9. **Settings (F9)** -- credentials, theme, version

Concepts, Planner, Skills, Chat, Safety, and Bundles are all gated on a
connected, selected cluster -- they show a placeholder message
until the user presses Enter on a cluster row in the Clusters tab.
Workers is NOT gated on a cluster -- it speaks to the local worker
daemon over a Unix socket.

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
- **Right pane**: topology diagram (current cluster's nodes +
  health), or the architecture-model drill-down navigator when
  toggled with `X`.

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

## Key Bindings

### Global
| Key      | Action                                                         |
|----------|----------------------------------------------------------------|
| F1..F8   | Switch tab                                                     |
| Ctrl+Q   | Quit                                                           |
| Ctrl+T   | Cycle theme                                                    |
| Ctrl+E   | Computer-use kill switch (revoke worker consent from any tab)  |

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
| Tab   | Cycle: Cluster Manager → Topology                   |

### Add/Edit forms
| Key       | Action                              |
|-----------|-------------------------------------|
| Tab       | Next field                          |
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
| `cluster/topology.go`    | Live cluster topology grid (right pane) + Architecture toggle |
| `cluster/architecture.go`| `ArchView`: drill-down navigator over `topology.model.json`   |
| `cluster/metrics_fetcher.go` | `QueryClientMetricsFetcher`: codeMetric overlay fetch      |
| _gRPC client_            | Lives in `memql/sdk/go/client/` -- `Connection`, `Dispatcher`, `QueryClient`, `SubscriptionManager`. Cockpit imports the SDK rather than reimplementing the wire layer. |
| `sense/`                 | `SenseClient` over the SDK Dispatcher -- editor-side wrappers for MemQL Sense (Tokenize / Diagnose / Complete / Hover). |
| `audio/`                 | Microphone capture for push-to-talk (malgo / miniaudio). Build-tag-gated (`voice`); stub returns a clear error on default headless builds. |
| `config/clusters.go`     | `~/.memql/clusters.yaml` load/save                            |
| `concepts/`              | Concepts tab (concept picker + row list + generic renderer)   |
| `planner/`               | Planner tab (read-only plan list + task list + detail; R:Run)  |
| `chat/`                  | Chat tab (space list + utterance scroll + `v`:PTT via memql-sdk-go voice.PushToTalk) |
| `workers/`               | Workers tab (computer-use consent dashboard + live audit tail; subscribes to `~/.memql/worker.sock`). Hosts the global Ctrl+E kill switch path. |
| `safety/`                | Safety tab (`v1:safety:classification` decision list + filters + drill-down; polls `queryAllSafetyClassifications`). |
| `bundles/`               | Bundles tab (read-only `v1:authoring:bundle` list + per-construct authored `.memql` source viewer + `X`:export-to-files; polls `queryAuthoringBundlesForOwner`, lazy `queryAuthoringConstructsForBundle`). |
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
  `V:PTT` not `v:PTT`. Keystroke handlers stay case-tolerant
  (`'v' || 'V'`), but the chip displays the canonical uppercase form
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
`client/` for queries / mutations / subscriptions, `voice/` for
push-to-talk, `sense/` for editor language intelligence, and
`worker/` once #117 lands.

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
