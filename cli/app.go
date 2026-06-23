// Package cli implements the memQL Cockpit -- a terminal-native IDE
// and operations console for memQL clusters.
package cli

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/znasllc-io/memql-cockpit/cli/auth"
	"github.com/znasllc-io/memql-cockpit/cli/cluster"
	"github.com/znasllc-io/memql-cockpit/cli/concepts"
	"github.com/znasllc-io/memql-cockpit/cli/config"
	"github.com/znasllc-io/memql-cockpit/cli/crash"
	"github.com/znasllc-io/memql-cockpit/cli/settings"
	"github.com/znasllc-io/memql-cockpit/cli/splash"
	"github.com/znasllc-io/memql-cockpit/cli/ui"
	genesiswizard "github.com/znasllc-io/memql-cockpit/cli/wizard/genesis"
	"github.com/znasllc-io/memql-cockpit/cli/wizard/runlocal"
	corgenesis "github.com/znasllc-io/memql/component/genesis"
	"github.com/znasllc-io/memql/component/node"
	nodev1 "github.com/znasllc-io/memql/component/node/gen"
	"github.com/znasllc-io/memql/sdk/go/client"
	"github.com/znasllc-io/memql/sdk/go/sense"
)

// AppConfig holds initialization parameters for the application.
type AppConfig struct {
	Cluster config.ClusterConfig
	Logger  *slog.Logger
	Version string
}

// App is the top-level TUI application.
type App struct {
	config        AppConfig
	screen        *ui.Screen
	header        *ui.Header
	notifications *ui.Notifications
	tabBar        *ui.TabBar
	theme         ui.Theme
	logger        *slog.Logger
	quitCh        chan struct{}
	quitOnce      sync.Once

	// redrawPending coalesces postRedraw bursts. The first caller in a
	// quiet window posts an EventInterrupt and flips the flag true;
	// subsequent callers find it already true and skip the post. The
	// event-loop's EventInterrupt branch clears the flag right before
	// draw() runs, so a state change THAT ARRIVES while draw is in
	// flight still posts a fresh interrupt and gets its own frame.
	//
	// Without this, ~40 postRedraw callsites across the codebase plus
	// the per-cluster subscriber bursts would queue dozens of identical
	// "please redraw" events for tcell to process serially -- each one
	// triggering a full draw() + Sync() / Show(). The user sees that
	// as per-frame flicker even when the underlying data hasn't changed.
	redrawPending atomic.Bool

	// Connection pool. Each cluster the user has opened this session
	// holds its own entry keyed by cluster Name. Switching clusters
	// (arrow keys) is a pool lookup; opening a new cluster adds an
	// entry without touching others. See cli/pool.go.
	poolMu sync.RWMutex
	pool   map[string]*connEntry
	// viewed is the cluster the topology pane is currently rendering
	// (follows arrow-key highlight).
	// selected is the cluster the user has chosen as their "working
	// cluster" via Enter. Drives the Concepts tab.
	// Auto-set to the first cluster that successfully connects on
	// startup (typically "local").
	viewed   string
	selected string
	// pendingSelect is a cluster name to promote to "selected" (working
	// cluster) the moment it first reaches stateConnected. Set when the
	// user adds a cluster: after the sign-in flow completes and the
	// entry dials successfully, onEntryConnected auto-selects it so the
	// new cluster ends up selected + online without a manual Enter.
	pendingSelect string

	// Tab views
	conceptsView *concepts.View
	clustersView *cluster.ClustersView
	settingsView *settings.View

	// getQueries returns a QueryClient bound to the currently active
	// cluster's dispatcher, or nil if none is connected. Cached by
	// wireConcepts so setSelected can re-trigger refreshConcepts
	// on connect.
	getQueries func() *client.QueryClient

	// Overlays
	helpOverlay *ui.HelpOverlay

	// tabCrashes is the sticky per-tab crash state. When a tab's
	// Draw or HandleEvent panics, the crash.Report goes here keyed
	// by the tab's name. Subsequent draws skip the tab's own Draw
	// and render the inline error placeholder instead, so the
	// broken pane doesn't keep re-panicking every frame and
	// flooding the crash log. Cleared when the user switches AWAY
	// and then BACK to the tab (gives them a one-press way to
	// retry without leaving the cockpit).
	//
	// Concurrency: accessed only from the tcell event-loop
	// goroutine (read inside draw(), written inside dispatchEvent).
	// No mutex needed; both run serially on the same goroutine.
	// If a future contributor mutates tabCrashes from a background
	// goroutine (preemptive panic injection for testing, etc.),
	// this assumption breaks and the map needs locking.
	tabCrashes map[string]*crash.Report
}

// NewApp creates a new CLI application instance.
func NewApp(cfg AppConfig) *App {
	theme := ui.DefaultTheme()

	settingsView := settings.NewView(theme, cfg.Version)

	conceptsView := concepts.NewView(theme)
	clustersView := cluster.NewClustersView(theme)

	// DevOps comes first -- it's the starting context for the session
	// (cluster management + live topology + deployments). Concepts
	// follows as the operations-console read surface. Settings stays
	// last. (Planner / Skills / Workers / Safety / Bundles panels were
	// removed in memql-cockpit#216 -- the cockpit is scoped to DevOps
	// + concept browsing + settings for now.)
	tabBar := ui.NewTabBar(theme,
		ui.Tab{Name: "DevOps", Content: clustersView},
		ui.Tab{Name: "Concepts", Content: conceptsView},
		ui.Tab{Name: "Settings", Content: settingsView},
	)
	tabBar.SetActive(0)

	notifications := ui.NewNotifications()
	app := &App{
		config:        cfg,
		theme:         theme,
		notifications: notifications,
		header:        ui.NewHeader(theme, notifications),
		tabBar:        tabBar,
		logger:        cfg.Logger,
		quitCh:        make(chan struct{}),
		pool:          make(map[string]*connEntry),
		conceptsView:  conceptsView,
		clustersView:  clustersView,
		settingsView:  settingsView,
		helpOverlay:   ui.NewHelpOverlay(theme),
		tabCrashes:    make(map[string]*crash.Report),
	}
	// A notifications change (sync from a background goroutine, or a
	// dismiss) should trigger a redraw so the user sees it immediately.
	notifications.OnChange = app.postRedraw

	// Wire cluster management callbacks.
	app.wireClustersCallbacks()

	// Load initial cluster list.
	app.refreshClusterList()

	return app
}

// Run starts the TUI event loop. Blocks until Quit() is called or the user exits.
//
// Launch sequence:
//  1. Pre-flight wizard -- if ~/.memql/genesis.znas is missing,
//     the first-launch wizard runs and seals an envelope. User
//     can cancel out, in which case Run returns without entering
//     the IDE.
//  2. Launch splash -- numbered options to pick the entry mode.
//     '1' = operating console (multi-tab IDE), '2' = run-local
//     placeholder, 'Q' = quit.
//  3. Operating console -- the multi-tab IDE. Connection
//     goroutines start here, not before, so the wizard / splash
//     run on a quiet screen.
func (a *App) Run() error {
	screen, err := ui.NewScreen()
	if err != nil {
		return err
	}
	a.screen = screen
	defer a.screen.Fini()
	a.screen.EnableInteraction()

	if a.shouldRunGenesisWizard() {
		switch genesiswizard.Run(a.screen, a.theme) {
		case genesiswizard.ResultCanceled, genesiswizard.ResultError:
			return nil
		}
	}

	// Splash is the home base for the launch sequence. The user can
	// dip into a wizard and come back; only picking the operating
	// console transitions the session into the multi-tab IDE.
	enterOperatingConsole := false
	for !enterOperatingConsole {
		switch splash.Run(a.screen, a.theme) {
		case splash.ChoiceQuit:
			return nil
		case splash.ChoiceRunLocalCluster:
			switch runlocal.Run(a.screen, a.theme) {
			case runlocal.ChoiceQuit:
				return nil
			case runlocal.ChoiceBack:
				// loop back to splash
			}
		case splash.ChoiceOperatingConsole:
			enterOperatingConsole = true
		}
	}

	// Auto-seed the local cluster from genesis.znas before the
	// operating console mounts. Best-effort: failure to seed (no
	// master key in env, bad envelope, etc.) leaves the local row
	// in its needs-auth state and the user authorizes by hand.
	a.autoSeedLocalFromGenesis()
	a.refreshClusterList()

	a.draw()

	// Force a second draw after a brief delay. tcell's first Sync() after
	// Init() consistently produces incomplete terminal output regardless of
	// content or timing. A second draw always renders correctly.
	go func() {
		time.Sleep(100 * time.Millisecond)
		a.screen.PostEvent(tcell.NewEventInterrupt(nil))
	}()

	// Start the background connection attempts (all clusters in
	// parallel) and the loop that redraws the detail pane's retry
	// countdown while any entry is in backoff.
	go a.connect()
	go a.backoffRedrawLoop()

	// Event loop. Each iteration body runs under a panic catcher
	// (crash.Catch) so a panic ANYWHERE outside the per-tab Draw /
	// HandleEvent wrappers -- chrome rendering, dispatch wiring,
	// redraw scheduling, future additions -- gets logged + surfaced
	// to the notification bar and the loop continues instead of
	// killing the whole app. The outermost defer-recover in main()
	// catches anything that escapes even this (e.g. a panic inside
	// the recover itself, or a panic in the controlled shutdown
	// path on the way out of Run).
	for {
		ev := a.screen.PollEvent()

		select {
		case <-a.quitCh:
			return nil
		default:
		}

		// Wrap the per-iteration switch in crash.Catch so a panic in
		// any non-tab handler (chrome render, dispatch wiring, etc.)
		// gets logged + surfaced as a notification, and the loop
		// keeps running.
		var quit bool
		report := crash.Catch("main-loop", func() {
			quit = a.dispatchEvent(ev)
		})
		if report != nil {
			if a.logger != nil {
				a.logger.Error("main loop iteration panicked",
					"code", report.Code,
					"logPath", report.LogPath,
				)
			}
			if a.notifications != nil {
				msg := fmt.Sprintf(
					"Cockpit hit an unexpected error (code %s). Please contact support.",
					report.Code,
				)
				a.notifications.Sync("crash", ui.SeverityError, msg)
			}
			// Try to redraw so the notification surfaces. Wrapped in
			// its own Catch in case draw() itself is the panic source.
			_ = crash.Catch("post-crash-draw", func() { a.draw() })
		}
		if quit {
			return nil
		}
	}
}

// dispatchEvent handles a single tcell event. Returns true when the
// user has requested quit (Ctrl+Q / Ctrl+C). Hoisted out of Run()
// so the entire dispatch body can be wrapped in crash.Catch as one
// unit; `continue` in the original loop body became `return false`
// here (the loop iteration is already "done with this event" the
// moment dispatchEvent returns).
func (a *App) dispatchEvent(ev tcell.Event) bool {
	switch ev := ev.(type) {
	case *tcell.EventInterrupt:
		// Background goroutines (connect, probe) post interrupts to trigger redraws.
		// Clear the pending flag BEFORE drawing so any postRedraw that
		// arrives while draw is still running queues a fresh interrupt
		// instead of being silently dropped.
		_ = ev
		a.redrawPending.Store(false)
		a.draw()

	case *tcell.EventResize:
		a.screen.Sync()
		a.draw()

	case *tcell.EventKey:
		// Ctrl+Q or Ctrl+C to quit.
		if ev.Key() == tcell.KeyCtrlC || ev.Key() == tcell.KeyCtrlQ {
			return true
		}

		// Help overlay toggle (Ctrl+?). F1 is dedicated to the
		// DevOps tab now; toggling help on Settings-tab F1 would
		// conflict with users who just pressed F1 to switch tabs.
		if ev.Key() == tcell.KeyRune && ev.Rune() == '?' && ev.Modifiers()&tcell.ModCtrl != 0 {
			a.helpOverlay.Toggle()
			a.draw()
			return false
		}

		// Ctrl+K dismisses the currently-shown notification in the
		// header feed (if any). Dismissal suppresses the exact same
		// message from re-appearing; a state change re-surfaces it.
		if ev.Key() == tcell.KeyCtrlK {
			a.notifications.DismissCurrent()
			a.draw()
			return false
		}

		// Ctrl+Y copies the current notification's message to the
		// system clipboard. Uses Y (yank) because Ctrl+C is bound
		// to quit (standard terminal convention). Errors from the
		// copy tool itself (missing pbcopy/xclip, no display, etc.)
		// surface to the feed under id clipboard so the user sees
		// why it didn't work instead of silently failing.
		//
		// Meta-acks (NoCopy) ignore Ctrl+Y -- copying the "Message
		// copied to clipboard." ack back to the clipboard is
		// nonsense; the key no-ops to match the hint strip.
		if ev.Key() == tcell.KeyCtrlY {
			if note, ok := a.notifications.Current(); ok && !note.NoCopy {
				// Fork the copy into a goroutine so the event loop
				// never blocks on the clipboard tool. On Linux/X11
				// xclip's input mode runs a background "selection
				// owner" subprocess; even with clipboard.go's
				// Stdout/Stderr-nil fix, the kernel + X server are
				// involved and an unresponsive X display can stall
				// xclip's parent process briefly. The TUI must keep
				// processing keys (Ctrl+Q, tab switches, scroll)
				// while that happens. The redraw is posted after
				// the goroutine resolves so the success/failure ack
				// shows up the next time the event loop ticks.
				note := note
				go func() {
					if err := ui.CopyToClipboard(note.Message); err != nil {
						// A copy FAILURE is not a meta-ack -- the
						// user may well want to copy the error text
						// to investigate it. Use regular Sync.
						a.notifications.Sync("clipboard", ui.SeverityError,
							"Copy failed: "+err.Error())
					} else {
						// Success ack is meta -- hide the Copy hint
						// on it via SyncMeta. Dismiss (Ctrl+K) still
						// works.
						a.notifications.SyncMeta("clipboard", ui.SeverityInfo,
							"Message copied to clipboard.")
					}
					a.postRedraw()
				}()
			}
			return false
		}

		// Help overlay consumes Escape when visible.
		if a.helpOverlay.Visible {
			if ev.Key() == tcell.KeyEscape {
				a.helpOverlay.Visible = false
			}
			a.draw()
			return false
		}

		// Tab switching. Clears the new tab's sticky crash state
		// (if any) so switching away + back gives the broken tab
		// a fresh try -- the user's only built-in "retry this
		// pane" gesture.
		if newTab := a.tabBar.HandleKey(ev); newTab >= 0 {
			a.tabBar.SetActive(newTab)
			if t := a.tabBar.ActiveTab(); t != nil {
				delete(a.tabCrashes, t.Name)
			}
			a.draw()
			return false
		}

		// Forward to active tab. Wrapped in crash.Catch so a
		// panic in the tab's HandleEvent doesn't kill the loop;
		// the tab's draw will surface the placeholder next frame.
		if tab := a.tabBar.ActiveTab(); tab != nil && tab.Content != nil {
			var handled bool
			if report := crash.Catch("event:"+tab.Name, func() {
				handled = tab.Content.HandleEvent(ev)
			}); report != nil {
				a.tabCrashes[tab.Name] = report
				if a.logger != nil {
					a.logger.Error("tab handle-event panicked",
						"tab", tab.Name,
						"code", report.Code,
						"logPath", report.LogPath,
					)
				}
				handled = true
			}
			if handled {
				a.draw()
			}
		}

	case *tcell.EventMouse:
		// Forward to active tab if needed.
		if tab := a.tabBar.ActiveTab(); tab != nil && tab.Content != nil {
			var handled bool
			if report := crash.Catch("mouse:"+tab.Name, func() {
				handled = tab.Content.HandleEvent(ev)
			}); report != nil {
				a.tabCrashes[tab.Name] = report
				if a.logger != nil {
					a.logger.Error("tab handle-mouse panicked",
						"tab", tab.Name,
						"code", report.Code,
						"logPath", report.LogPath,
					)
				}
				handled = true
			}
			if handled {
				a.draw()
			}
		}
	}
	return false
}

// Quit signals the application to shut down. Closes every pool
// entry; each one's monitor/subscriber goroutines exit via their own
// done channels.
func (a *App) Quit() {
	a.quitOnce.Do(func() {
		close(a.quitCh)
		a.poolMu.Lock()
		entries := make([]*connEntry, 0, len(a.pool))
		for _, e := range a.pool {
			entries = append(entries, e)
		}
		a.pool = nil
		a.poolMu.Unlock()
		for _, e := range entries {
			e.Close()
		}
		if a.screen != nil {
			a.screen.PostEvent(tcell.NewEventInterrupt(nil))
		}
	})
}

// connect is the startup entry point: restores the sticky selection
// from clusters.yaml, then kicks off a dial goroutine for every
// cluster in the saved list (local + all user-added). Each runs its
// own bounded-retry lifecycle in parallel; the UI stays responsive
// throughout.
func (a *App) connect() {
	clusters, err := config.LoadClusters()
	if err != nil && a.logger != nil {
		a.logger.Warn("failed to load clusters", "error", err)
	}
	// Start with the local cluster definition (may be overridden by a
	// user-saved "local" entry in clusters.yaml).
	configs := []config.ClusterConfig{cluster.LocalClusterConfig()}
	if clusters != nil {
		for _, c := range clusters.Clusters {
			if c.Name == "local" {
				configs[0] = c
				continue
			}
			configs = append(configs, c)
		}
	}

	// Restore the sticky "working cluster" from the yaml. If it's
	// empty or points at a cluster that no longer exists, fall back
	// to "local" (which is always present).
	selected := "local"
	if clusters != nil && clusters.SelectedCluster != "" {
		for _, c := range configs {
			if c.Name == clusters.SelectedCluster {
				selected = clusters.SelectedCluster
				break
			}
		}
	}
	a.poolMu.Lock()
	a.selected = selected
	a.viewed = selected
	a.poolMu.Unlock()
	a.clustersView.SelectedCluster = selected
	// Highlight the selected cluster so the initial draw matches.
	// Topology header uses the display name (e.g. "local.znas.io").
	for i, cs := range a.clustersView.Clusters {
		if cs.Config.Name == selected {
			a.clustersView.Selected = i
			a.clustersView.Topology.ClusterName = cs.Config.Display()
			break
		}
	}
	if a.clustersView.Topology.ClusterName == "" {
		a.clustersView.Topology.ClusterName = selected
	}

	// Auto-wire the UI glue so the tabs have their callbacks set even
	// before the first successful dial lands.
	a.wireConcepts()
	a.wireCluster()
	for _, cfg := range configs {
		a.openEntry(cfg)
	}

}

// persistSelected writes a.selected to clusters.yaml so the next
// launch picks the same cluster. Called whenever the user changes
// their working cluster via Enter.
func (a *App) persistSelected(name string) {
	clusters, err := config.LoadClusters()
	if err != nil {
		if a.logger != nil {
			a.logger.Warn("failed to load clusters for persisting selection", "error", err)
		}
		return
	}
	if clusters == nil {
		clusters = &config.ClustersFile{}
	}
	if clusters.SelectedCluster == name {
		return
	}
	clusters.SelectedCluster = name
	if err := config.SaveClusters(clusters); err != nil && a.logger != nil {
		a.logger.Warn("failed to persist selected cluster", "error", err)
	}
}

// runLoginFlow runs the OAuth / magic-link browser flow against an
// already-configured cluster (Issuer + ClientId present in
// clusters.yaml, or PAT). On success the minted token gets cached
// via auth.EnsureValidToken's internal write path and the pool
// entry is restarted so a fresh dial cycle picks up the bearer.
//
// Used by L:Login on the cluster list; distinct from
// runAuthorizeFlow which discovers a brand-new cluster from a URL.
//
// Progress + outcome travel through the notifications center under
// id "cluster:login:<name>" so the user sees what's happening
// without the UI thread blocking on the browser flow.
func (a *App) runLoginFlow(clusterName string) {
	notifId := "cluster:login:" + clusterName

	clusters, err := config.LoadClusters()
	if err != nil {
		a.notifications.SyncMeta(notifId, ui.SeverityError,
			fmt.Sprintf("Load clusters config: %v", err))
		a.postRedraw()
		return
	}
	cfg, ok := clusters.Get(clusterName)
	if !ok {
		a.notifications.SyncMeta(notifId, ui.SeverityError,
			fmt.Sprintf("Cluster %q not found in clusters.yaml.", clusterName))
		a.postRedraw()
		return
	}
	if cfg.NeedsAuth() {
		// Local-row edge case: genesis didn't seed Issuer / ClientId.
		// Other rows would have hit the form path; the only way we
		// land here for them is a race.
		a.notifications.SyncMeta(notifId, ui.SeverityError,
			fmt.Sprintf("%q has no issuer / client_id wired up; re-run the setup wizard or authorize via discovery URL.", cfg.Display()))
		a.postRedraw()
		return
	}

	a.notifications.SyncMeta(notifId, ui.SeverityInfo,
		fmt.Sprintf("Opening browser to authenticate with %q ...", cfg.Display()))
	a.postRedraw()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if _, err := auth.EnsureValidToken(ctx, cfg); err != nil {
		a.notifications.SyncMeta(notifId, ui.SeverityError,
			fmt.Sprintf("Login failed for %q: %v", cfg.Display(), err))
		a.postRedraw()
		return
	}

	a.notifications.SyncMeta(notifId, ui.SeverityInfo,
		fmt.Sprintf("Authenticated with %q -- connecting...", cfg.Display()))
	a.postRedraw()

	// Token cached; restart the pool entry so the fresh dial cycle
	// sees the bearer. replaceEntry will see the now-valid cached
	// token and skip stateNeedsToken -- straight to lifecycle.
	a.replaceEntry(cfg)
}

// replaceEntry closes any existing pool entry for cfg.Name and opens
// a fresh one with the new config. Used by the authorize flow to
// pick up new credentials without leaking the old connection.
func (a *App) replaceEntry(cfg config.ClusterConfig) {
	a.poolMu.Lock()
	old := a.pool[cfg.Name]
	delete(a.pool, cfg.Name)
	a.poolMu.Unlock()
	if old != nil {
		old.Close()
	}
	a.openEntry(cfg)
}

// shouldRunGenesisWizard reports whether the first-launch genesis
// wizard should fire. The cockpit treats absence of the envelope as
// the trigger -- presence (even with an outdated content) is treated
// as "operator has already set up; do not re-prompt".
func (a *App) shouldRunGenesisWizard() bool {
	path := genesisFilePath()
	if path == "" {
		return false
	}
	_, err := os.Stat(path)
	return os.IsNotExist(err)
}

// genesisFilePath returns the absolute path of the operator's
// genesis envelope, mirroring memql's resolution rules.
// $MEMQL_GENESIS_PATH wins; otherwise ~/.memql/genesis.znas. Empty
// string when the home dir can't be resolved (degenerate; treated as
// "no wizard" by callers).
func genesisFilePath() string {
	if p := strings.TrimSpace(os.Getenv("MEMQL_GENESIS_PATH")); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".memql", "genesis.znas")
}

// autoSeedLocalFromGenesis is the bridge between the sealed envelope
// and the operating-console's clusters.yaml. Called once on
// operating-console entry. Decrypts genesis.znas (requires
// MEMQL_MASTER_KEY in the environment), reads
// IDENTITY_BOOTSTRAP_DOMAIN, and writes a fully-configured local
// row to clusters.yaml:
//
//	DisplayName = <domain>                  (e.g. local.znas.io)
//	Endpoint    = https://bff.<domain>      (NGINX LB entry)
//	Issuer      = https://identity.<domain> (OIDC issuer)
//	ClientId    = cockpit                   (registered cockpit client)
//
// The Name slot stays "local" -- it's the config key, used by yaml
// lookup, token cache filenames, etc. DisplayName is the
// human-readable label shown in the row list.
//
// The Issuer / ClientId / DisplayName values rely on the convention
// the local stack ships with (NGINX server_name `identity.${DOMAIN}`
// terminating TLS for the identity service; cockpit client_id
// registered in identity bootstrap). If the user's local stack
// deviates, they can edit clusters.yaml or re-run authorize via
// the TUI on a non-local cluster slot.
//
// Re-runs idempotently: if the persisted local row already carries
// the values genesis would produce, nothing is written. If genesis
// changes (different domain), the row gets refreshed.
//
// Failure is silent on purpose: the user can still authorize a
// cluster by hand from inside the TUI if the auto-seed didn't run.
// The caller refreshes the cluster list after this returns so any
// seed is visible to the first draw.
func (a *App) autoSeedLocalFromGenesis() {
	gpath := genesisFilePath()
	if gpath == "" {
		return
	}
	if _, err := os.Stat(gpath); err != nil {
		return // no envelope -- wizard wasn't completed; nothing to seed from
	}

	entries, err := corgenesis.OpenFile(gpath)
	if err != nil {
		if a.logger != nil {
			a.logger.Warn("auto-seed: open genesis", "error", err)
		}
		return
	}
	domain, ok := corgenesis.LookupEnv(entries, "IDENTITY_BOOTSTRAP_DOMAIN")
	if !ok || strings.TrimSpace(domain) == "" {
		return
	}
	domain = strings.TrimSpace(domain)
	seed := config.ClusterConfig{
		Name:        "local",
		DisplayName: domain,
		Endpoint:    "https://bff." + domain,
		Issuer:      "https://identity." + domain,
		ClientId:    "cockpit",
	}

	clusters, err := config.LoadClusters()
	if err != nil {
		if a.logger != nil {
			a.logger.Warn("auto-seed: load clusters.yaml", "error", err)
		}
		return
	}

	replaced := false
	dirty := false
	for i := range clusters.Clusters {
		if clusters.Clusters[i].Name != "local" {
			continue
		}
		// Merge genesis-derived fields onto whatever was there. Preserve
		// sticky bits (optional PAT, etc.) so re-seeding doesn't wipe
		// operator-set state.
		existing := clusters.Clusters[i]
		merged := existing
		if merged.DisplayName != seed.DisplayName {
			merged.DisplayName = seed.DisplayName
			dirty = true
		}
		if merged.Endpoint != seed.Endpoint {
			merged.Endpoint = seed.Endpoint
			dirty = true
		}
		if merged.Issuer != seed.Issuer {
			merged.Issuer = seed.Issuer
			dirty = true
		}
		if merged.ClientId != seed.ClientId {
			merged.ClientId = seed.ClientId
			dirty = true
		}
		clusters.Clusters[i] = merged
		replaced = true
		break
	}
	if !replaced {
		clusters.Clusters = append(clusters.Clusters, seed)
		dirty = true
	}
	if !dirty {
		return
	}
	if err := config.SaveClusters(clusters); err != nil && a.logger != nil {
		a.logger.Warn("auto-seed: save clusters.yaml", "error", err)
	}
}

// openEntry adds an entry for cfg to the pool if not already present,
// and starts its lifecycle goroutine. Returns immediately -- the dial
// and retries run in the background. If an entry for cfg.Name already
// exists, this is a no-op (let the existing lifecycle own it).
//
// Two states short-circuit the lifecycle:
//
//   - stateNeedsConfig: row has no endpoint, or no PAT + no
//     Issuer+ClientId pair. Nothing to dial against. Row picks up
//     an `L:Authorize` hint.
//   - stateNeedsToken: row is fully configured but no cached OAuth
//     token. Avoids popping a browser the moment cockpit launches;
//     instead the row picks up an `L:Login` hint and the user
//     explicitly initiates the magic-link flow.
//
// PAT-authenticated rows skip the token check (the PAT is itself
// the credential; no minting needed).
func (a *App) openEntry(cfg config.ClusterConfig) {
	a.poolMu.Lock()
	if a.pool == nil {
		a.pool = make(map[string]*connEntry)
	}
	if _, exists := a.pool[cfg.Name]; exists {
		a.poolMu.Unlock()
		return
	}
	entry := newConnEntry(a, cfg)
	a.pool[cfg.Name] = entry
	a.poolMu.Unlock()

	if cfg.NeedsAuth() {
		entry.setStateAttempt(stateNeedsConfig, 0, time.Time{})
		a.postRedraw()
		return
	}
	if cfg.PAT == "" && !hasValidCachedToken(cfg.Name) {
		entry.setStateAttempt(stateNeedsToken, 0, time.Time{})
		a.postRedraw()
		return
	}

	// The lifecycle goroutine drives the state machine: attempts a
	// bounded number of dials, sleeps backoff between them, handles
	// reconnects after an unexpected stream close, and responds to
	// cancel / close signals.
	go entry.runLifecycle()
	// The token refresher silently rolls the cached OIDC token
	// forward before it expires, so a future reconnect never has
	// to fall through to the browser flow. PAT clusters short-
	// circuit inside runTokenRefresher.
	go entry.runTokenRefresher()
	a.postRedraw()
}

// hasValidCachedToken reports whether a usable token is on disk for
// clusterName. "Usable" = present + not expired. Any error reading
// or parsing the token is treated as "no token" so the user lands
// in stateNeedsToken instead of the silent-fail dial cycle.
func hasValidCachedToken(clusterName string) bool {
	stored, err := config.LoadToken(clusterName)
	if err != nil || stored == nil {
		return false
	}
	return !stored.IsExpired()
}

// activeDispatcher returns the gRPC dispatcher of the cluster the
// user has SELECTED (Enter key). Returns nil if nothing is selected
// or the selected cluster isn't currently connected.
func (a *App) activeDispatcher() *client.Dispatcher {
	a.poolMu.RLock()
	name := a.selected
	entry, ok := a.pool[name]
	a.poolMu.RUnlock()
	if !ok || entry == nil {
		return nil
	}
	entry.mu.Lock()
	conn := entry.Conn
	state := entry.State
	entry.mu.Unlock()
	if conn == nil || state != stateConnected {
		return nil
	}
	return conn.Dispatcher()
}

// viewedEntry returns the pool entry the topology pane is currently
// rendering, or nil if none.
func (a *App) viewedEntry() *connEntry {
	a.poolMu.RLock()
	defer a.poolMu.RUnlock()
	if a.viewed == "" {
		return nil
	}
	return a.pool[a.viewed]
}

// setViewed updates which cluster the topology pane renders.
// Called by OnHighlight (arrow keys). No side effects on the
// Concepts workspace -- that follows a.selected, which is a separate
// concept changed by OnEnter, not arrow keys.
func (a *App) setViewed(name string) {
	a.poolMu.Lock()
	a.viewed = name
	entry := a.pool[name]
	a.poolMu.Unlock()

	// Keep the topology header labeled with the viewed cluster so
	// the user always knows which cluster the diagram is for. Prefer
	// the display name (e.g. "local.znas.io") over the slot key.
	display := name
	if entry != nil {
		display = entry.Config.Display()
	}
	a.clustersView.Topology.ClusterName = display

	if entry != nil {
		a.clustersView.Topology.SetNodeTypes(entry.snapshotNodeTypes())
		a.clustersView.Topology.SetNodes(entry.snapshotNodes())
		entry.mu.Lock()
		state := entry.State
		entry.mu.Unlock()
		// "Stale" rendering (red boxes) only makes sense for an entry
		// that was connected and lost its stream -- otherwise empty
		// is the right blank canvas.
		a.clustersView.Topology.SetDisconnected(state == stateBackoff && len(entry.Nodes) > 0)
	} else {
		a.clustersView.Topology.SetNodeTypes(nil)
		a.clustersView.Topology.SetNodes(nil)
		a.clustersView.Topology.SetDisconnected(false)
	}
	a.postRedraw()
}

// setSelected promotes a cluster to "my working cluster" -- drives
// the Concepts tab. Called by OnEnter. If the entry is in
// stateFailed, also kicks a manual Retry so Enter on a dead cluster
// means "I want this to come back". Persists the choice to
// clusters.yaml so the next launch restores it.
func (a *App) setSelected(name string) {
	// Only CONNECTED clusters can become the working cluster.
	// Selecting a still-dialing or unreachable cluster has no useful
	// effect (Concepts is gated on a connected
	// dispatcher anyway), so Enter is a no-op for those rows. The
	// hint strip already hides "Enter:Select" in those states; this
	// is the matching guard at the action layer.
	a.poolMu.RLock()
	entry := a.pool[name]
	a.poolMu.RUnlock()
	if entry == nil {
		// Brand-new row from a current-frame add: no entry yet.
		// Open the lifecycle so a subsequent Enter (once connected)
		// can promote it.
		if name == "local" {
			a.openEntry(cluster.LocalClusterConfig())
		} else if clusters, err := config.LoadClusters(); err == nil {
			if cfg, ok := clusters.Get(name); ok {
				a.openEntry(cfg)
			}
		}
		a.postRedraw()
		return
	}
	entry.mu.Lock()
	state := entry.State
	conn := entry.Conn
	entry.mu.Unlock()
	if state != stateConnected {
		// Silent no-op. Retry uses R, not Enter.
		return
	}

	a.poolMu.Lock()
	a.selected = name
	a.poolMu.Unlock()

	a.clustersView.SelectedCluster = name
	a.persistSelected(name)

	// Refresh the Settings tab's "My Access" block with the new
	// cluster's access record. Async so the UI stays responsive.
	a.refreshMyAccess(name, conn)

	// Refresh the Concepts tab against the newly-connected cluster.
	// wireConcepts fires its first call at app-init when no cluster is
	// connected yet, so without this re-trigger the Concepts tab stays
	// empty even though ListConcepts would return data the moment we ask.
	go a.refreshConcepts(context.Background())

	a.postRedraw()
}

// refreshConcepts fetches the connected cluster's concept registry
// and pushes it into the Concepts view. Safe to call when no cluster
// is connected (it no-ops on a nil getQueries result). Called once at
// app init (mostly a no-op then) + every time the selected cluster
// transitions into stateConnected via setSelected.
func (a *App) refreshConcepts(ctx context.Context) {
	if a.getQueries == nil {
		return
	}
	q := a.getQueries()
	if q == nil {
		return
	}
	concepts, err := q.ListConcepts(ctx)
	if err != nil {
		if a.logger != nil {
			a.logger.Warn("concepts: ListConcepts failed", "error", err)
		}
		return
	}
	a.conceptsView.SetConcepts(concepts)
	if a.screen != nil {
		a.screen.PostEvent(tcell.NewEventInterrupt(nil))
	}
}

// refreshMyAccess fetches the caller's AccessContext from the given
// cluster and pushes it into the Settings view. Runs in a goroutine so
// the call doesn't block the UI thread. Failures are logged and the
// panel falls back to its empty state (no notification spam -- MyAccess
// is informational, not load-bearing for the rest of the UI).
func (a *App) refreshMyAccess(clusterName string, conn *client.Connection) {
	if conn == nil || conn.Dispatcher() == nil {
		a.settingsView.ClearMyAccess()
		a.postRedraw()
		return
	}
	qc := client.NewQueryClient(conn.Dispatcher())
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		access, err := qc.GetMyAccess(ctx)
		if err != nil {
			if a.logger != nil {
				a.logger.Warn("settings: GetMyAccess failed", "cluster", clusterName, "error", err)
			}
			return
		}
		a.settingsView.SetMyAccess(clusterName, access)
		// Mirror the cluster role onto the Topology view so the
		// Deployments section knows which cut/deploy/rollback controls
		// the caller may use (view = any role; cut/deploy =
		// developer/admin/owner; rollback = owner only). Same access
		// record, two consumers.
		if a.clustersView != nil && a.clustersView.Topology != nil && access != nil {
			a.clustersView.Topology.SetClusterRole(access.ClusterRole)
		}
		a.postRedraw()
	}()
}

// wireConcepts connects the Concepts tab's callbacks to the gRPC
// client. Wires the QueryClient closure (fresh on each call so cluster
// switches transparently retarget) and the status callback so the view
// can surface errors via the notification bar.
func (a *App) wireConcepts() {
	getQueries := func() *client.QueryClient {
		d := a.activeDispatcher()
		if d == nil {
			return nil
		}
		return client.NewQueryClient(d)
	}

	a.getQueries = getQueries
	a.conceptsView.QueryClient = getQueries
	// Same dispatcher-binding pattern as QueryClient: every call
	// resolves against whichever cluster is active right now, so a
	// cluster switch transparently retargets Sense too. Returns nil
	// when no cluster is connected; the concepts View handles that.
	a.conceptsView.SenseClient = func() *sense.Client {
		d := a.activeDispatcher()
		if d == nil {
			return nil
		}
		return sense.NewClient(d)
	}
	a.conceptsView.OnStatus = func(msg string) {
		if a.notifications != nil {
			a.notifications.Sync("concepts", ui.SeverityWarning, msg)
		}
	}

	// Fire an initial fetch. Usually a no-op at app-init (no cluster
	// connected yet), but kept for the case where an auto-connected
	// cluster (e.g. local) is already in stateConnected by the time
	// wireConcepts runs.
	go a.refreshConcepts(context.Background())
}

// wireCluster wires the topology pane's OnInitialLoad callback to the
// currently viewed pool entry. The actual subscription + event stream
// is owned per-entry (see connEntry.runSubscriber); this function only
// connects the "refresh on demand" hook used by OnSelect.
func (a *App) wireCluster() {
	a.clustersView.Topology.OnInitialLoad = func() []cluster.NodeInfo {
		entry := a.viewedEntry()
		if entry == nil {
			return nil
		}
		return entry.snapshotNodes()
	}

	// DeployClient resolves a DeployControlClient against whichever
	// cluster is active right now, so a cluster switch transparently
	// retargets -- same closure pattern as the other views' QueryClient.
	// Returns nil when no cluster is connected. Used by the Deployments
	// section's cut/deploy/rollback controls.
	a.clustersView.Topology.DeployClient = func() *client.DeployControlClient {
		a.poolMu.RLock()
		name := a.selected
		entry := a.pool[name]
		a.poolMu.RUnlock()
		if entry == nil {
			return nil
		}
		entry.mu.Lock()
		conn := entry.Conn
		state := entry.State
		entry.mu.Unlock()
		if conn == nil || state != stateConnected {
			return nil
		}
		return client.NewDeployControlClient(conn)
	}

	// OnRedraw lands an in-flight cut/deploy/rollback action's result
	// line on screen the moment the SDK call returns, without waiting for
	// the next poll tick.
	a.clustersView.Topology.OnRedraw = a.postRedraw

	// #207/#221 Deployments section hooks. OnDeploymentsShown loads the
	// deployment history (ExistingCluster -> DeploymentsForCluster);
	// OnSelectDeployment loads the selected deployment's nodes + orphans;
	// OnDeploymentsChanged re-loads the history after a cut/deploy/rollback
	// action so the list reflects the new state right away. The section is
	// always visible in the persistent split, so the history is loaded
	// eagerly here and refreshed on the periodic ticker below rather than
	// on a toggle.
	a.clustersView.Topology.OnDeploymentsShown = func() {
		go a.refreshDeployments()
	}
	a.clustersView.Topology.OnSelectDeployment = func(deploymentID string) {
		go a.loadDeploymentNodes(deploymentID)
	}
	a.clustersView.Topology.OnDeploymentsChanged = func() {
		go a.refreshDeployments()
	}

	// Periodically refresh the deployment history so the always-on
	// Deployments section reflects new cuts/deploys without a manual
	// reload.
	go a.deploymentsRefreshLoop()
}

// deploymentsRefreshLoop periodically refreshes the per-cluster
// deployment history feeding the always-on Deployments section. It runs
// for the life of the app; each tick is a no-op unless a cluster is
// connected. The cadence mirrors the architecture-metrics refresh --
// deploy state changes on human timescales, so a slow tick is correct.
func (a *App) deploymentsRefreshLoop() {
	// One eager refresh so the section populates on first connect without
	// waiting a full tick, then periodic.
	a.refreshDeployments()
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-a.quitCh:
			return
		case <-ticker.C:
			a.refreshDeployments()
		}
	}
}

// activeQueryClient resolves a QueryClient bound to the currently active
// cluster's connection, or nil when none is connected. Mirrors the
// DeployClient closure's pool lookup so a cluster switch retargets.
func (a *App) activeQueryClient() *client.QueryClient {
	a.poolMu.RLock()
	name := a.selected
	entry := a.pool[name]
	a.poolMu.RUnlock()
	if entry == nil {
		return nil
	}
	entry.mu.Lock()
	conn := entry.Conn
	state := entry.State
	entry.mu.Unlock()
	if conn == nil || state != stateConnected {
		return nil
	}
	return client.NewQueryClient(conn.Dispatcher())
}

// refreshDeployments loads the deployment history for the active cluster
// and pushes it onto the Topology view's Deployments section. Resolves
// the clusterId via ExistingCluster first (single-cluster staging),
// then DeploymentsForCluster. Errors leave the existing list intact
// and log at debug. Per memql-cockpit#207.
func (a *App) refreshDeployments() {
	if a.clustersView == nil || a.clustersView.Topology == nil {
		return
	}
	qc := a.activeQueryClient()
	if qc == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	clusterId := a.resolveClusterId(ctx, qc)
	if clusterId == "" {
		return
	}
	res, err := qc.DeploymentsForCluster(ctx, client.DeploymentsForClusterArgs{ClusterId: clusterId})
	if err != nil {
		if a.logger != nil {
			a.logger.Debug("queryDeploymentsForCluster failed", "cluster", clusterId, "error", err)
		}
		return
	}
	a.clustersView.Topology.SetDeployments(parseDeployments(res))
	a.postRedraw()
}

// resolveClusterId returns the active connection's cluster id via
// ExistingCluster, or "" when unknown. Single-cluster staging
// returns one row; the first non-empty id wins.
func (a *App) resolveClusterId(ctx context.Context, qc *client.QueryClient) string {
	res, err := qc.ExistingCluster(ctx, client.ExistingClusterArgs{})
	if err != nil {
		if a.logger != nil {
			a.logger.Debug("queryExistingCluster failed", "error", err)
		}
		return ""
	}
	for _, r := range res.Rows() {
		if id := client.RowString(r, "id"); id != "" {
			return id
		}
	}
	return ""
}

// loadDeploymentNodes loads the selected deployment's node set
// (NodesForDeployment) + orphans (NodesNotInDeployment) and
// pushes them onto the view. Called off-thread from OnSelectDeployment.
func (a *App) loadDeploymentNodes(deploymentID string) {
	if deploymentID == "" || a.clustersView == nil || a.clustersView.Topology == nil {
		return
	}
	qc := a.activeQueryClient()
	if qc == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var nodes, orphans []cluster.NodeInfo
	if res, err := qc.NodesForDeployment(ctx, client.NodesForDeploymentArgs{DeploymentId: deploymentID}); err == nil {
		nodes = parseDeploymentNodes(res)
	} else if a.logger != nil {
		a.logger.Debug("queryNodesForDeployment failed", "deployment", deploymentID, "error", err)
	}
	if res, err := qc.NodesNotInDeployment(ctx, client.NodesNotInDeploymentArgs{DeploymentId: deploymentID}); err == nil {
		orphans = parseDeploymentNodes(res)
	} else if a.logger != nil {
		a.logger.Debug("queryNodesNotInDeployment failed", "deployment", deploymentID, "error", err)
	}
	a.clustersView.Topology.SetDeploymentNodes(deploymentID, nodes, orphans)
	a.postRedraw()
}

// parseDeployments converts a queryDeploymentsForCluster result into the
// view's DeploymentInfo slice, sorted newest-first by createdAt.
func parseDeployments(res *client.Result) []cluster.DeploymentInfo {
	if res == nil {
		return nil
	}
	rows := res.Rows()
	out := make([]cluster.DeploymentInfo, 0, len(rows))
	for _, r := range rows {
		id := firstNonEmpty(client.RowString(r, "deploymentId"), client.RowString(r, "id"))
		if id == "" {
			continue
		}
		out = append(out, cluster.DeploymentInfo{
			ID:                   id,
			Version:              client.RowString(r, "version"),
			ImageDigest:          client.RowString(r, "imageDigest"),
			Provider:             client.RowString(r, "provider"),
			Environment:          client.RowString(r, "environment"),
			Region:               client.RowString(r, "region"),
			Status:               client.RowString(r, "status"),
			TriggeredBy:          client.RowString(r, "triggeredBy"),
			CreatedAt:            firstNonEmpty(client.RowString(r, "createdAt"), client.RowString(r, "created_at")),
			UpdatedAt:            client.RowString(r, "updatedAt"),
			PreviousDeploymentId: client.RowString(r, "previousDeploymentId"),
			Notes:                client.RowString(r, "notes"),
		})
	}
	// Newest-first by createdAt (RFC3339 sorts lexicographically). Stable
	// so equal timestamps keep source order.
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].CreatedAt > out[j].CreatedAt
	})
	return out
}

// parseDeploymentNodes converts a node query result (for-deployment or
// not-in-deployment) into NodeInfo, carrying the deploymentId/parentId
// the Deployments section + orphan highlight rely on.
func parseDeploymentNodes(res *client.Result) []cluster.NodeInfo {
	if res == nil {
		return nil
	}
	rows := res.Rows()
	out := make([]cluster.NodeInfo, 0, len(rows))
	for _, r := range rows {
		nodeType := firstNonEmpty(client.RowString(r, "nodeType"), client.RowString(r, "type"))
		id := client.RowString(r, "id")
		if nodeType == "" && id == "" {
			continue
		}
		out = append(out, cluster.NodeInfo{
			ID:           id,
			Name:         firstNonEmpty(client.RowString(r, "name"), nodeType),
			Type:         nodeType,
			Address:      client.RowString(r, "address"),
			Version:      client.RowString(r, "version"),
			Health:       node.ParseHealthLabel(firstNonEmpty(client.RowString(r, "health"), "healthy")),
			DeploymentId: client.RowString(r, "deploymentId"),
			ParentId:     client.RowString(r, "parentId"),
		})
	}
	return out
}

// parseClusterNodeEvent converts an SDK Event carrying a
// v1:cluster:node CDC payload into a NodeInfo. Returns ok=false when the
// payload is missing required fields (type + either ID).
func parseClusterNodeEvent(ev client.Event) (cluster.NodeInfo, bool) {
	if ev.Payload == nil {
		return cluster.NodeInfo{}, false
	}
	m := ev.Payload

	// Payload fields are flattened by the engine at emission; we still
	// defensively check a nested "payload" in case of shape drift.
	payload := m
	if p, ok := m["payload"].(map[string]any); ok && p != nil {
		payload = p
	}

	nodeType := firstNonEmpty(
		getString(payload, "nodeType"),
		getString(m, "nodeType"),
	)
	nodeId := firstNonEmpty(getString(m, "id"), getString(m, "nodeId"), getString(payload, "nodeId"))
	if nodeType == "" && nodeId == "" {
		return cluster.NodeInfo{}, false
	}

	healthStr := firstNonEmpty(getString(payload, "health"), getString(m, "health"))

	return cluster.NodeInfo{
		ID:           nodeId,
		Name:         firstNonEmpty(getString(payload, "name"), nodeType),
		Type:         nodeType,
		Address:      firstNonEmpty(getString(payload, "address"), getString(m, "address")),
		Version:      firstNonEmpty(getString(payload, "version"), getString(m, "version")),
		Health:       node.ParseHealthLabel(healthStr),
		DeploymentId: firstNonEmpty(getString(payload, "deploymentId"), getString(m, "deploymentId")),
		ParentId:     firstNonEmpty(getString(payload, "parentId"), getString(m, "parentId")),
	}, true
}

// detectNodeType is a LAST-RESORT fallback used only when
// queryClusterNodes and queryClusterSpawnEvents both return empty
// (e.g. brand-new cluster, or a server that hasn't finished its own
// registerNode automation yet). It guesses from the node id string so
// the topology at least shows the one node we're connected to. Order
// matters: "cognition" contains "cog", etc, so check long-prefixes
// first. Every live node type the cluster supports should be listed
// here -- missing a type makes every node of that type render as the
// BFF fallback until the real registration row lands.
func detectNodeType(nodeId string) string {
	lower := strings.ToLower(nodeId)
	switch {
	case strings.Contains(lower, "cognition"):
		return "cognition"
	case strings.Contains(lower, "planner"):
		return "planner"
	case strings.Contains(lower, "voice"):
		return "voice"
	case strings.Contains(lower, "agent"):
		return "agent"
	case strings.Contains(lower, "bff"):
		return "bff"
	default:
		return "bff" // Default: connected node is typically a BFF
	}
}

// parseSpawnEvents extracts node info from v1:cluster:spawnEvent records.
func parseSpawnEvents(res *client.Result) []cluster.NodeInfo {
	rows := res.Rows()
	if len(rows) == 0 {
		return nil
	}

	latest := make(map[string]map[string]any)
	for _, m := range rows {
		payload := m
		if p, ok := m["payload"].(map[string]any); ok {
			payload = p
		}
		nodeId := firstNonEmpty(getString(payload, "nodeId"), getString(m, "nodeId"), getString(payload, "id"))
		if nodeId == "" {
			continue
		}
		latest[nodeId] = payload
	}

	var nodes []cluster.NodeInfo
	for nodeId, m := range latest {
		action := getString(m, "action")
		if action == "stopped" {
			continue
		}
		nodes = append(nodes, cluster.NodeInfo{
			ID:      nodeId,
			Name:    firstNonEmpty(getString(m, "nodeName"), getString(m, "name"), getString(m, "nodeType")),
			Type:    firstNonEmpty(getString(m, "nodeType"), getString(m, "type")),
			Address: getString(m, "address"),
			Version: getString(m, "version"),
			Health:  nodev1.NodeHealthStatus_NODE_HEALTH_HEALTHY,
		})
	}
	return nodes
}

// parseClusterNodes converts a query result into NodeInfo slice.
// Handles multiple response formats from the MemQL engine:
// - Array of maps with flat fields
// - Array of maps with nested "payload" object
// - Map with "nodes" or "items" array
//
// The concept is a time-series so the query returns every historical
// row. We dedupe to one entry per node id by keeping the row with the
// highest createdAt (or, failing that, the last one encountered).
func parseClusterNodes(res *client.Result) []cluster.NodeInfo {
	return nodesFromRows(res.Rows())
}

// nodesFromRows maps already-unwrapped query rows to NodeInfo. Split out
// from parseClusterNodes so the row-shape handling (flat vs nested
// payload) + the dedupe-by-id is unit-testable without constructing an
// opaque *client.Result.
func nodesFromRows(rows []client.Row) []cluster.NodeInfo {
	if len(rows) == 0 {
		return nil
	}

	// Dedupe by id: keep the row with the newest createdAt per id. The
	// concept is a time-series, so a single real node produces one row
	// per registration + one per health transition; without this filter
	// the topology would render every historical version.
	type rowWithTime struct {
		info    cluster.NodeInfo
		created string // RFC3339; lexicographic compare is correct for that format
	}
	latest := make(map[string]rowWithTime)

	for _, m := range rows {
		// Result.Rows() flattens payload into the row for bundle
		// results; a raw []any result keeps it nested -- check both.
		payload := m
		if p, ok := m["payload"].(map[string]any); ok {
			payload = p
		}

		nodeType := firstNonEmpty(
			getString(payload, "nodeType"),
			getString(payload, "type"),
			getString(m, "nodeType"),
			getString(m, "type"),
		)
		if nodeType == "" {
			continue // Skip entries without a node type.
		}

		healthStr := firstNonEmpty(getString(payload, "health"), getString(m, "health"), "healthy")
		id := firstNonEmpty(getString(m, "id"), getString(payload, "id"))
		createdAt := firstNonEmpty(
			getString(m, "createdAt"),
			getString(payload, "createdAt"),
			getString(m, "created_at"),
		)

		n := cluster.NodeInfo{
			ID:           id,
			Name:         firstNonEmpty(getString(payload, "name"), getString(m, "name"), nodeType),
			Type:         nodeType,
			Address:      firstNonEmpty(getString(payload, "address"), getString(m, "address")),
			Version:      firstNonEmpty(getString(payload, "version"), getString(m, "version")),
			Health:       node.ParseHealthLabel(healthStr),
			DeploymentId: firstNonEmpty(getString(payload, "deploymentId"), getString(m, "deploymentId")),
			ParentId:     firstNonEmpty(getString(payload, "parentId"), getString(m, "parentId")),
		}

		// Group by id so every historical version of the same node
		// collapses to one entry. Fall back to grouping by type when id
		// is missing (a degraded payload shape we can still render).
		key := id
		if key == "" {
			key = "type:" + nodeType
		}
		if existing, found := latest[key]; found && createdAt <= existing.created {
			continue
		}
		latest[key] = rowWithTime{info: n, created: createdAt}
	}

	nodes := make([]cluster.NodeInfo, 0, len(latest))
	for _, r := range latest {
		nodes = append(nodes, r.info)
	}
	return nodes
}

// parseClusterNodeTypes converts a queryClusterNodeTypes({}) result
// into a NodeTypeInfo slice. Dedupes by `name` keeping the row with
// the newest createdAt (the concept is a time-series, so re-seeding
// produces multiple rows per name), then sorts ascending by
// createdAt so the topology row order matches seed file order
// (bff -> voice -> cognition -> agent -> planner with the current
// seed).
func parseClusterNodeTypes(res *client.Result) []cluster.NodeTypeInfo {
	rows := res.Rows()
	if len(rows) == 0 {
		return nil
	}

	type rowWithTime struct {
		info    cluster.NodeTypeInfo
		created string
	}
	latest := make(map[string]rowWithTime)
	for _, m := range rows {
		payload := m
		if p, ok := m["payload"].(map[string]any); ok {
			payload = p
		}
		name := firstNonEmpty(getString(payload, "name"), getString(m, "name"))
		if name == "" {
			continue
		}
		createdAt := firstNonEmpty(
			getString(m, "createdAt"),
			getString(payload, "createdAt"),
			getString(m, "created_at"),
		)
		info := cluster.NodeTypeInfo{
			Name:        name,
			Description: firstNonEmpty(getString(payload, "description"), getString(m, "description")),
		}
		if existing, found := latest[name]; found && createdAt <= existing.created {
			continue
		}
		latest[name] = rowWithTime{info: info, created: createdAt}
	}
	out := make([]rowWithTime, 0, len(latest))
	for _, r := range latest {
		out = append(out, r)
	}
	// Ascending by createdAt -- seed.memql inserts in bff/voice/cognition/agent/planner
	// order, so the earliest createdAt renders on the top row.
	sort.Slice(out, func(i, j int) bool { return out[i].created < out[j].created })
	typed := make([]cluster.NodeTypeInfo, len(out))
	for i, r := range out {
		typed[i] = r.info
	}
	return typed
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func getString(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

// wireClustersCallbacks hooks the cluster list's callbacks into the
// app's pool-backed state machine.
func (a *App) wireClustersCallbacks() {
	// OnHighlight fires when arrow keys move the list cursor. Pure
	// view swap -- no connections touched.
	a.clustersView.OnHighlight = func(clusterName string) {
		a.setViewed(clusterName)
	}

	// OnEnter fires when the user presses Enter on a row. Promotes
	// that cluster to "selected" (drives the Concepts tab) and,
	// if its pool entry is in stateFailed, kicks a manual retry.
	a.clustersView.OnEnter = func(clusterName string) {
		a.setSelected(clusterName)
	}

	// OnCancel fires when Esc is pressed on a row. Interrupts any
	// in-flight retry cycle for that cluster; no effect on other
	// clusters or on connected rows.
	a.clustersView.OnCancel = func(clusterName string) {
		a.poolMu.RLock()
		entry, ok := a.pool[clusterName]
		a.poolMu.RUnlock()
		if ok && entry != nil {
			entry.Cancel()
			a.clustersView.SetRowStatus(clusterName, "unreachable")
			a.postRedraw()
		}
	}

	// OnRetry fires when R is pressed on a row. Only kicks a fresh
	// 3-attempt cycle if the entry has already run out of automatic
	// retries (stateFailed); other states ignore it.
	a.clustersView.OnRetry = func(clusterName string) {
		a.poolMu.RLock()
		entry, ok := a.pool[clusterName]
		a.poolMu.RUnlock()
		if ok && entry != nil {
			entry.Retry()
			a.postRedraw()
		}
	}

	// OnEntryState lets the cluster list's detail pane render the
	// entry's current retry progress (attempt N/3, next retry in Xs).
	a.clustersView.OnEntryState = func(name string) (string, int, string, bool) {
		a.poolMu.RLock()
		entry, ok := a.pool[name]
		a.poolMu.RUnlock()
		if !ok || entry == nil {
			return "", 0, "", false
		}
		state, attempt, nextTryAt := entry.stateSnapshot()
		nextStr := ""
		if !nextTryAt.IsZero() {
			wait := time.Until(nextTryAt)
			if wait < 0 {
				wait = 0
			}
			nextStr = fmt.Sprintf("%ds", int(wait.Seconds()+0.5))
		}
		return state.String(), attempt, nextStr, true
	}

	// OnLogin fires when L is pressed on a fully-configured row
	// (or on the local row, regardless of state). Runs OAuth
	// against the row's cached Issuer + ClientId, caches the
	// resulting token, then restarts the pool entry so the dial
	// cycle picks up the bearer.
	a.clustersView.OnLogin = func(clusterName string) {
		go a.runLoginFlow(clusterName)
	}

	// Add a new cluster, save, auto-open its pool entry. No
	// "auto-connect via OnConnect" dance -- openEntry handles it.
	//
	// Duplicates (same name as an existing cluster, OR the reserved
	// "local") are ignored at the data layer + surfaced as a warning
	// notification so the form doesn't appear to silently swallow the
	// user's input. SyncMeta is used because copying the warning text
	// back to the clipboard makes no sense -- it's a UI ack, not data.
	a.clustersView.OnAdd = func(c config.ClusterConfig) {
		if c.Name == "local" {
			a.notifications.SyncMeta("cluster:add", ui.SeverityWarning,
				fmt.Sprintf("%q is reserved -- cluster not added.", c.Name))
			return
		}
		clusters, err := config.LoadClusters()
		if err != nil {
			return
		}
		if _, exists := clusters.Get(c.Name); exists {
			a.notifications.SyncMeta("cluster:add", ui.SeverityWarning,
				fmt.Sprintf("Cluster %q already exists -- nothing added.", c.Name))
			return
		}
		clusters.Clusters = append(clusters.Clusters, c)
		if err := config.SaveClusters(clusters); err != nil {
			return
		}
		// Successful add clears any prior duplicate warning.
		a.notifications.Clear("cluster:add")
		a.refreshClusterList()

		// Open a pool entry for it. A freshly-added cluster has an
		// Issuer + ClientId but no token yet, so openEntry parks it in
		// stateNeedsToken -- it won't dial on its own.
		a.openEntry(c)

		// Highlight the new row + move the "viewed" pointer so the
		// topology pane follows it. Without setViewed here, the
		// highlight cursor visually jumps to the new row but the
		// right-pane topology stays frozen on whatever cluster was
		// viewed before.
		for i, cs := range a.clustersView.Clusters {
			if cs.Config.Name == c.Name {
				a.clustersView.Selected = i
				break
			}
		}
		a.setViewed(c.Name)

		// Arm auto-select: when this cluster first connects (after the
		// sign-in below), onEntryConnected promotes it to the working
		// cluster so the user lands on it selected + online.
		a.setPendingSelect(c.Name)

		// Route straight to sign-in. Adding a cluster IS the intent to
		// use it, so open the browser flow now instead of parking the
		// row in needs-login behind a separate L keypress. A PAT or an
		// already-cached token skips the browser (the lifecycle dials
		// directly); everything else opens the OAuth flow, which caches
		// the token and restarts the entry so it dials + connects.
		if c.PAT == "" && !hasValidCachedToken(c.Name) {
			go a.runLoginFlow(c.Name)
		}
	}

	// Save an edit to an existing cluster. Works for "local" too -- the
	// override gets persisted to clusters.yaml and takes precedence over
	// the hardcoded default on next load.
	a.clustersView.OnSave = func(c config.ClusterConfig) {
		clusters, err := config.LoadClusters()
		if err != nil {
			return
		}
		// Replace in place if already present; otherwise append. For
		// "local" the first edit is typically an insert since the
		// default is hardcoded and not yet persisted.
		replaced := false
		for i := range clusters.Clusters {
			if clusters.Clusters[i].Name == c.Name {
				clusters.Clusters[i] = c
				replaced = true
				break
			}
		}
		if !replaced {
			clusters.Clusters = append(clusters.Clusters, c)
		}
		if err := config.SaveClusters(clusters); err != nil {
			return
		}
		a.refreshClusterList()
		// Keep the edited cluster selected.
		for i, cs := range a.clustersView.Clusters {
			if cs.Config.Name == c.Name {
				a.clustersView.Selected = i
				break
			}
		}
	}

	// Delete a cluster (see deleteCluster -- local cannot be deleted;
	// teardown is forked off the event loop).
	a.clustersView.OnDelete = a.deleteCluster
}

// deleteCluster removes a user-added cluster from the registry and the
// UI. "local" is never deletable.
//
// CRITICAL: this runs on the tcell event-loop goroutine (it's the
// OnDelete keypress callback). Everything here MUST be fast, non-
// blocking, local work -- the loop processes one event at a time, so a
// blocking call freezes the entire TUI. The actual teardown
// (gRPC stream close + OS-keyring token delete) is BLOCKING I/O and is
// forked to a background goroutine below. On macOS in particular the
// keyring delete can pop a system permission dialog that's invisible
// behind the full-screen TUI, hanging the loop indefinitely if run
// inline (memql-cockpit#191). This mirrors the clipboard-copy (Ctrl+Y)
// goroutine-fork pattern.
func (a *App) deleteCluster(clusterName string) {
	if clusterName == "local" {
		return
	}
	clusters, err := config.LoadClusters()
	if err != nil {
		return
	}
	var remaining []config.ClusterConfig
	for _, c := range clusters.Clusters {
		if c.Name != clusterName {
			remaining = append(remaining, c)
		}
	}
	clusters.Clusters = remaining
	_ = config.SaveClusters(clusters)

	// Drop the pool entry + any references to the deleted cluster before
	// the list refresh -- so refreshClusterList sees a clean pool when it
	// stamps row statuses. The entry's blocking Close() is forked below.
	a.poolMu.Lock()
	entry, ok := a.pool[clusterName]
	if ok {
		delete(a.pool, clusterName)
	}
	selectedDeleted := a.selected == clusterName
	viewedDeleted := a.viewed == clusterName
	if selectedDeleted {
		a.selected = ""
	}
	if viewedDeleted {
		a.viewed = ""
	}
	a.poolMu.Unlock()

	// Blocking teardown off the event loop: the gRPC stream/conn close
	// and the OS-keyring token delete (the indefinite-hang culprit on
	// macOS). The cluster is already gone from config + the pool, so the
	// row disappears on this frame regardless of how long teardown takes.
	go func() {
		if entry != nil {
			entry.Close()
		}
		_ = config.DeleteToken(clusterName)
		a.postRedraw()
	}()

	a.clustersView.SetConnected(clusterName, false, "", "")
	a.refreshClusterList()

	// Pick the row the list is now highlighting (clamped to the
	// shrunk list; "local" at index 0 is guaranteed to exist).
	if v := a.clustersView; v != nil {
		if v.Selected >= len(v.Clusters) && v.Selected > 0 {
			v.Selected = len(v.Clusters) - 1
		}
		if len(v.Clusters) == 0 {
			// Shouldn't happen -- local is always present -- but
			// bail gracefully rather than panic.
			v.SelectedCluster = ""
			return
		}
		newName := v.Clusters[v.Selected].Config.Name
		// Rehome the viewed pointer (and its topology side-effect)
		// whenever the viewed cluster went away.
		if viewedDeleted {
			a.setViewed(newName)
		}
		// Rehome the working-cluster pointer too if THAT one was
		// deleted. Independent of viewed -- user may have deleted
		// a cluster that was selected but not highlighted.
		if selectedDeleted {
			a.setSelected(newName)
		}
		v.SelectedCluster = a.selectedName()
	}
}

// updateTabGating sets the GatedMessage on the cluster-dependent
// Concepts tab based on whether the user's selected cluster has a live
// connection. Called from draw() so state changes show up on the next
// repaint.
func (a *App) updateTabGating() {
	setGated := func(msg string) {
		a.conceptsView.GatedMessage = msg
	}
	name := a.selectedName()
	if name == "" {
		setGated("No cluster selected. Switch to the Clusters tab (F1) and press Enter on a cluster.")
		return
	}
	a.poolMu.RLock()
	entry := a.pool[name]
	a.poolMu.RUnlock()
	if entry == nil {
		setGated(fmt.Sprintf("Selected cluster %q is not open. Switch to the Clusters tab (F1) and press Enter on its row.", name))
		return
	}
	state, _, _ := entry.stateSnapshot()
	if state == stateConnected {
		setGated("")
		return
	}
	var why string
	switch state {
	case stateConnecting, stateBackoff:
		why = "connecting"
	case stateFailed:
		why = "unreachable"
	default:
		why = state.String()
	}
	setGated(fmt.Sprintf("Selected cluster %q is %s. Available again once it reaches a connected state.", name, why))
}

// backoffRedrawLoop schedules periodic redraws while any pool entry
// is in stateBackoff, so the "next retry in Xs" countdown ticks in
// the detail pane. Only runs while the Clusters tab is active.
func (a *App) backoffRedrawLoop() {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-a.quitCh:
			return
		case <-ticker.C:
		}

		// Cheap check: any entry in backoff?
		a.poolMu.RLock()
		anyBackoff := false
		for _, e := range a.pool {
			e.mu.Lock()
			if e.State == stateBackoff {
				anyBackoff = true
			}
			e.mu.Unlock()
			if anyBackoff {
				break
			}
		}
		a.poolMu.RUnlock()
		if anyBackoff {
			a.postRedraw()
		}
	}
}

// refreshClusterList reloads clusters from the config file and decorates
// each row's status from the pool: if a cluster has a live pool entry,
// it shows "connected"; if the entry is mid-reconnect, "unreachable";
// otherwise "unknown". Every connected cluster lights up green, not
// just the "active" one.
func (a *App) refreshClusterList() {
	clusters, err := config.LoadClusters()
	if err != nil {
		return
	}

	a.poolMu.RLock()
	poolCopy := make(map[string]*connEntry, len(a.pool))
	for k, v := range a.pool {
		poolCopy[k] = v
	}
	a.poolMu.RUnlock()

	var statuses []cluster.ClusterStatus
	for _, c := range clusters.Clusters {
		status := "unknown"
		var nodeId, nodeVer string
		if entry, ok := poolCopy[c.Name]; ok && entry != nil {
			entry.mu.Lock()
			switch entry.State {
			case stateConnected:
				status = "connected"
				if entry.Conn != nil {
					nodeId = entry.Conn.NodeId
					nodeVer = entry.Conn.Version
				}
			case stateConnecting, stateBackoff:
				status = "connecting"
			case stateFailed:
				status = "unreachable"
			}
			entry.mu.Unlock()
		}
		statuses = append(statuses, cluster.ClusterStatus{
			Config:  c,
			Status:  status,
			NodeId:  nodeId,
			NodeVer: nodeVer,
		})
	}

	a.clustersView.SetClusters(statuses)
}

// syncConnectionNotifications walks every pool entry and publishes its
// lifecycle state into the header's notifications feed. Called from
// draw() on every repaint so the feed always reflects current truth.
//
// Notifications ids are scoped per cluster ("cluster:<name>") so each
// cluster occupies at most one slot; multiple clusters misbehaving at
// once show up as distinct entries the user can cycle through. A
// Connected / Idle cluster clears its slot.
//
// Notifications.Sync de-dupes by exact message text AND respects
// user dismissals, so a dismissed "attempt 2/3" stays hidden until
// the state changes to something else (e.g. a new attempt number, or
// failed -> unreachable).
func (a *App) syncConnectionNotifications() {
	a.poolMu.RLock()
	entries := make([]*connEntry, 0, len(a.pool))
	for _, e := range a.pool {
		entries = append(entries, e)
	}
	a.poolMu.RUnlock()

	for _, entry := range entries {
		state, attempt, _ := entry.stateSnapshot()
		id := "cluster:" + entry.Config.Name
		switch state {
		case stateConnecting:
			a.notifications.Sync(id, ui.SeverityWarning,
				fmt.Sprintf("Reconnecting to %q (attempt %d/%d)...",
					entry.Config.Name, attempt, maxAttempts))
		case stateBackoff:
			a.notifications.Sync(id, ui.SeverityWarning,
				fmt.Sprintf("Retrying %q (attempt %d/%d)...",
					entry.Config.Name, attempt, maxAttempts))
		case stateFailed:
			a.notifications.Sync(id, ui.SeverityError,
				fmt.Sprintf("%q is unreachable -- press R on its row to retry.",
					entry.Config.Name))
		case stateNeedsConfig:
			a.notifications.Sync(id, ui.SeverityWarning,
				fmt.Sprintf("%q is not configured -- press L on its row to authorize.",
					entry.Config.Display()))
		case stateNeedsToken:
			a.notifications.Sync(id, ui.SeverityWarning,
				fmt.Sprintf("%q needs a login -- press L on its row to authenticate.",
					entry.Config.Display()))
		default:
			// Connected / Idle -- the cluster is fine, clear its slot.
			a.notifications.Clear(id)
		}
	}
}

// draw renders the entire screen.
func (a *App) draw() {
	w, h := a.screen.Size()
	a.screen.Clear(a.theme.BaseStyle())

	// Header chrome at the top (1 row): "memQL Cockpit" + universal
	// Tab-pane-switch hint. Mirrors the tab bar's dark band.
	a.header.Draw(a.screen, 0, w)

	// Tab bar at the bottom (1 row).
	tabBarY := h - 1
	a.tabBar.Draw(a.screen, tabBarY, w)

	// Chrome spacer rows -- one below the header, one above the tab
	// bar -- in the same dark band color so the header + content +
	// footer read as three distinct zones with visible breathing
	// room. Without these, the Clusters tab's " CLUSTERS " pane title
	// collides visually with " memQL Cockpit " directly above it.
	barBG := tcell.NewRGBColor(18, 18, 22)
	chromeStyle := tcell.StyleDefault.Background(barBG)
	a.screen.FillRect(0, 1, w, 1, chromeStyle)
	a.screen.FillRect(0, h-2, w, 1, chromeStyle)

	// Sync every pool entry's lifecycle state into the header's
	// notifications feed. Replaces the old full-width red banner that
	// used to live above the tab bar -- transient reconnect messages
	// no longer shove the tab content around. The notifications widget
	// de-dupes by cluster name, so syncing on every draw is safe.
	a.syncConnectionNotifications()

	// Tab content lives between the two chrome spacer rows. Rows 0+1
	// are header + spacer, row h-2 is spacer, row h-1 is the tab
	// bar -- so content occupies rows 2..h-3 (height = h-4).
	contentHeight := h - 4

	contentBounds := ui.Rect{X: 0, Y: 2, Width: w, Height: contentHeight}
	// Gate Concepts on the selected cluster being
	// connected. When it isn't, that view renders a centered
	// "not available" message instead of their usual content.
	a.updateTabGating()

	if tab := a.tabBar.ActiveTab(); tab != nil && tab.Content != nil {
		// Sticky per-tab crash state: if THIS tab previously panicked
		// (either here in Draw or in HandleEvent), render the inline
		// "something went wrong" placeholder instead of re-invoking
		// the broken Draw -- avoids a panic-per-frame loop that
		// would flood the crash log. Clears when the user switches
		// away and back.
		if report := a.tabCrashes[tab.Name]; report != nil {
			crash.DrawInline(a.screen, contentBounds, a.theme, report)
		} else if report := crash.Catch("draw:"+tab.Name, func() {
			tab.Content.Draw(a.screen, contentBounds)
		}); report != nil {
			// Tab's Draw panicked this frame. Stash the report, render
			// the placeholder over whatever partial paint landed.
			a.tabCrashes[tab.Name] = report
			crash.DrawInline(a.screen, contentBounds, a.theme, report)
			if a.logger != nil {
				a.logger.Error("tab draw panicked",
					"tab", tab.Name,
					"code", report.Code,
					"logPath", report.LogPath,
				)
			}
		}
	}

	// Help overlay on top of everything.
	if a.helpOverlay.Visible {
		a.helpOverlay.Draw(a.screen, ui.Rect{X: 0, Y: 0, Width: w, Height: h})
	}

	// Ensure no cursor artifact: hide cursor and position it at origin.
	// Some terminals render a visible cursor even when "hidden" if it's
	// positioned at the bottom-right corner (causing the stray '{').
	a.screen.Inner().ShowCursor(0, 0)
	a.screen.Inner().HideCursor()
	// IMPORTANT: Sync(), not Show(). Three Show() attempts have all
	// reproduced the same 1-row-offset ghost on Pop_OS:
	//
	//   1. Original Show() experiment    -- reverted (no notes).
	//   2. Second Show() experiment      -- reverted; ghost screenshots
	//      Screenshot_2026-05-17_11-32-12.png /
	//      Screenshot_2026-05-17_11-54-02.png.
	//   3. PR #12 Show() experiment      -- reverted by this commit.
	//      The ghost reproduces immediately on cockpit launch the
	//      moment the cluster auto-connects: CLUSTERS header,
	//      local.znas.io row, TOPOLOGY header, the bottom tab strip
	//      and WASD:Pan hint all paint twice (a duplicate frame
	//      shifted one row down). Screenshot 2026-05-17 14:24.
	//
	// Hypothesis going in to PR #12 was that the prior reverts had
	// been masked by races we'd since fixed (state-coherence locks on
	// every racing view, the layout-edge-glyph rule, the postRedraw
	// coalescer). Wrong: the coalescer alone is fine and we keep it,
	// but Show() still produces the same ghost on the same OS even
	// with all three of those landed. Whatever the root cause is, it
	// lives at the tcell-diff-vs-Pop_OS layer, not in our race fixes.
	//
	// Per-frame flicker on Sync() is real, but the ghost is worse:
	// duplicated rows make the UI unreadable. Sync() stays until a
	// reproducer that ISN'T platform-specific gives us something to
	// debug. The coalescer (postRedraw -> redrawPending atomic) keeps
	// flicker bounded by collapsing event bursts into one frame.
	a.screen.Sync()
}
