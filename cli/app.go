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
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/visionarys-io/memql-cockpit/cli/auth"
	"github.com/visionarys-io/memql-cockpit/cli/client"
	"github.com/visionarys-io/memql-cockpit/cli/cluster"
	"github.com/visionarys-io/memql-cockpit/cli/concepts"
	"github.com/visionarys-io/memql-cockpit/cli/config"
	"github.com/visionarys-io/memql-cockpit/cli/crash"
	"github.com/visionarys-io/memql-cockpit/cli/discovery"
	"github.com/visionarys-io/memql-cockpit/cli/settings"
	"github.com/visionarys-io/memql-cockpit/cli/splash"
	"github.com/visionarys-io/memql-cockpit/cli/ui"
	genesiswizard "github.com/visionarys-io/memql-cockpit/cli/wizard/genesis"
	"github.com/visionarys-io/memql-cockpit/cli/wizard/runlocal"
	corgenesis "github.com/visionarys-io/memql/component/genesis"
	memqlv1 "github.com/visionarys-io/memql/component/grpc/gen"
	"github.com/visionarys-io/memql/component/node"
	nodev1 "github.com/visionarys-io/memql/component/node/gen"
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
	logger   *slog.Logger
	quitCh   chan struct{}
	quitOnce sync.Once

	// Connection pool. Each cluster the user has opened this session
	// holds its own entry keyed by cluster Name. Switching clusters
	// (arrow keys) is a pool lookup; opening a new cluster adds an
	// entry without touching others. See cli/pool.go.
	poolMu sync.RWMutex
	pool   map[string]*connEntry
	// viewed is the cluster the topology pane is currently rendering
	// (follows arrow-key highlight).
	// selected is the cluster the user has chosen as their "working
	// cluster" via Enter. Drives the Explorer / Agents tabs.
	// Auto-set to the first cluster that successfully connects on
	// startup (typically "local").
	viewed   string
	selected string

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
	tabCrashes map[string]*crash.Report
}

// NewApp creates a new CLI application instance.
func NewApp(cfg AppConfig) *App {
	theme := ui.DefaultTheme()

	settingsView := settings.NewView(theme, cfg.Version)

	conceptsView := concepts.NewView(theme)
	clustersView := cluster.NewClustersView(theme)

	// Clusters comes first -- it's the starting context for the session.
	tabBar := ui.NewTabBar(theme,
		ui.Tab{Name: "Clusters", Content: clustersView},
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
	app.wirePartitionsCallbacks()

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
// unit; `continue` in the original loop becomes a no-op return here
// (the loop iteration is already "done with this event" the moment
// dispatchEvent returns).
func (a *App) dispatchEvent(ev tcell.Event) bool {
	{
		_ = ev // placeholder so the original switch can be lifted in

		switch ev := ev.(type) {
		case *tcell.EventInterrupt:
			// Background goroutines (connect, probe) post interrupts to trigger redraws.
			_ = ev
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
			// Clusters tab now; toggling help on Settings-tab F1 would
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
					if err := ui.CopyToClipboard(note.Message); err != nil {
						// A copy FAILURE is not a meta-ack -- the user
						// may well want to copy the error text to
						// investigate it. Use regular Sync.
						a.notifications.Sync("clipboard", ui.SeverityError,
							"Copy failed: "+err.Error())
					} else {
						// Success ack is meta -- hide the Copy hint on
						// it via SyncMeta. Dismiss (Ctrl+K) still works.
						a.notifications.SyncMeta("clipboard", ui.SeverityInfo,
							"Message copied to clipboard.")
					}
					a.draw()
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

	// Paint the partitions pane immediately so the auto-selected cluster
	// (typically "local") shows the "default" fallback row from the
	// first frame -- before any dial has had a chance to land. Once the
	// connection completes, onEntryConnected refreshes again with the
	// real listPartitions result.
	a.refreshPartitionsView()
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

// runAuthorizeFlow is the OnAuthorize goroutine body. Reports
// progress + outcome through the notifications center so the UI
// thread sees status updates without blocking. The notification id
// is keyed on the cluster being authorized so successive runs
// against the same cluster replace each other instead of stacking.
//
// existingName is the row currently being edited (empty for Add).
// Used to resolve "what is the row name now?" when the discovery
// doc disagrees: edit mode keeps the existing name (the user is
// updating credentials for THIS slot, not renaming), Add mode
// trusts the discovery doc.
func (a *App) runAuthorizeFlow(discoveryURL, existingName string) {
	notifId := "cluster:authorize"
	if existingName != "" {
		notifId = "cluster:authorize:" + existingName
	}
	a.notifications.SyncMeta(notifId, ui.SeverityInfo,
		fmt.Sprintf("Fetching discovery from %s ...", discoveryURL))
	a.postRedraw()

	doc, err := discovery.Fetch(discoveryURL)
	if err != nil {
		a.notifications.SyncMeta(notifId, ui.SeverityError,
			fmt.Sprintf("Discovery failed: %v", err))
		a.postRedraw()
		return
	}

	name := strings.TrimSpace(existingName)
	if name == "" {
		name = strings.TrimSpace(doc.ClusterName)
	}
	if name == "" {
		name = discovery.HostFromURL(discoveryURL)
	}
	if name == "" {
		a.notifications.SyncMeta(notifId, ui.SeverityError,
			"Discovery response did not include a cluster name -- cannot continue.")
		a.postRedraw()
		return
	}

	cfg := config.ClusterConfig{
		Name:     name,
		Endpoint: strings.TrimSpace(doc.GRPCEndpoint),
		Issuer:   strings.TrimSpace(doc.IdentityURL),
		ClientId: strings.TrimSpace(doc.ClientId),
	}

	// Persist (replace-in-place for existing rows, append for new ones).
	clusters, err := config.LoadClusters()
	if err != nil {
		a.notifications.SyncMeta(notifId, ui.SeverityError,
			fmt.Sprintf("Load clusters config: %v", err))
		a.postRedraw()
		return
	}
	replaced := false
	for i := range clusters.Clusters {
		if clusters.Clusters[i].Name == cfg.Name {
			// Preserve user-set sticky bits (selected partition, PAT)
			// when overwriting the auth-relevant fields.
			cfg.SelectedPartition = clusters.Clusters[i].SelectedPartition
			cfg.PAT = clusters.Clusters[i].PAT
			clusters.Clusters[i] = cfg
			replaced = true
			break
		}
	}
	if !replaced {
		clusters.Clusters = append(clusters.Clusters, cfg)
	}
	if err := config.SaveClusters(clusters); err != nil {
		a.notifications.SyncMeta(notifId, ui.SeverityError,
			fmt.Sprintf("Save clusters config: %v", err))
		a.postRedraw()
		return
	}

	// Refresh the cluster list view so the user sees the new Endpoint
	// / Issuer / ClientId values immediately. The pool entry is left
	// in stateNeedsConfig (its config is still the pre-authorize one)
	// until after the token is minted -- otherwise the lifecycle
	// would race the browser flow, exhaust its 3 dials with "no
	// authorization header" before the user even sees the magic
	// link, and the cluster would surface as unreachable while the
	// user is still typing their email.
	a.refreshClusterList()

	a.notifications.SyncMeta(notifId, ui.SeverityInfo,
		fmt.Sprintf("Opening browser to authenticate with %q ...", cfg.Name))
	a.postRedraw()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if _, err := auth.EnsureValidToken(ctx, cfg); err != nil {
		a.notifications.SyncMeta(notifId, ui.SeverityError,
			fmt.Sprintf("Authorization failed for %q: %v", cfg.Name, err))
		a.postRedraw()
		return
	}

	a.notifications.SyncMeta(notifId, ui.SeverityInfo,
		fmt.Sprintf("Authorized %q -- connecting...", cfg.Name))
	a.postRedraw()

	// Token is cached; recreate the pool entry with the new config
	// so a fresh dial cycle starts with the bearer in place.
	a.replaceEntry(cfg)
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
//   DisplayName = <domain>                  (e.g. local.znas.io)
//   Endpoint    = https://bff.<domain>      (NGINX LB entry)
//   Issuer      = https://identity.<domain> (OIDC issuer)
//   ClientId    = cockpit                   (registered cockpit client)
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
		// sticky bits (selected partition, optional PAT, etc.) so
		// re-seeding doesn't wipe operator-set state.
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

// setViewed updates which cluster the topology + partitions panes
// render. Called by OnHighlight (arrow keys). No side effects on the
// Explorer / Agents workspace -- those follow a.selected, which
// is a separate concept changed by OnEnter, not arrow keys.
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
	// Partitions pane follows the highlighted cluster on every
	// arrow-key move, same as topology.
	a.refreshPartitionsView()
	a.postRedraw()
}

// setSelected promotes a cluster to "my working cluster" -- drives
// Explorer/Agents. Called by OnEnter. If the entry is in
// stateFailed, also kicks a manual Retry so Enter on a dead cluster
// means "I want this to come back". Persists the choice to
// clusters.yaml so the next launch restores it.
func (a *App) setSelected(name string) {
	// Only CONNECTED clusters can become the working cluster.
	// Selecting a still-dialing or unreachable cluster has no useful
	// effect (Explorer/Agents are gated on a connected
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
	part := entry.SelectedPartition
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
	// Partitions pane follows the VIEWED cluster (highlighted via
	// arrow keys), not the SELECTED one. No refresh from setSelected.

	// Push the cluster's sticky partition onto the live dispatcher
	// the moment it becomes the user's working cluster, so the next
	// query/mutation routes to the right partition.
	if conn != nil && conn.Dispatcher() != nil && part != "" {
		conn.Dispatcher().SetPartition(part)
	}
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
}

// parseClusterNodeEvent converts a gRPC EventNotification carrying a
// v1:cluster:node CDC payload into a NodeInfo. Returns ok=false when the
// payload is missing required fields (type + either ID).
func parseClusterNodeEvent(ev *memqlv1.EventNotification) (cluster.NodeInfo, bool) {
	if ev == nil || ev.GetPayload() == nil {
		return cluster.NodeInfo{}, false
	}
	m := ev.GetPayload().AsMap()

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
		ID:      nodeId,
		Name:    firstNonEmpty(getString(payload, "name"), nodeType),
		Type:    nodeType,
		Address: firstNonEmpty(getString(payload, "address"), getString(m, "address")),
		Version: firstNonEmpty(getString(payload, "version"), getString(m, "version")),
		Health:  node.ParseHealthLabel(healthStr),
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
func parseSpawnEvents(result any) []cluster.NodeInfo {
	if result == nil {
		return nil
	}
	items, ok := result.([]any)
	if !ok {
		if m, ok := result.(map[string]any); ok {
			for _, key := range []string{"items", "results", "data"} {
				if arr, ok := m[key].([]any); ok {
					items = arr
					break
				}
			}
		}
		if items == nil {
			return nil
		}
	}

	latest := make(map[string]map[string]any)
	for _, item := range items {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
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
func parseClusterNodes(result any) []cluster.NodeInfo {
	if result == nil {
		return nil
	}

	var items []any

	switch v := result.(type) {
	case []any:
		items = v
	case map[string]any:
		// MemQL returns { bundle: { nodes: [...] } } or { result: { bundle: { nodes: [...] } } }
		// Dig through nested wrappers to find the array.
		items = extractNodeArray(v)
		if items == nil {
			// Single node as a map.
			items = []any{v}
		}
	default:
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

	for _, item := range items {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}

		// Try flat fields first, then nested payload.
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
			ID:      id,
			Name:    firstNonEmpty(getString(payload, "name"), getString(m, "name"), nodeType),
			Type:    nodeType,
			Address: firstNonEmpty(getString(payload, "address"), getString(m, "address")),
			Version: firstNonEmpty(getString(payload, "version"), getString(m, "version")),
			Health:  node.ParseHealthLabel(healthStr),
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

// parsePartitionEvent converts a CDC EventNotification carrying a
// v1:platform:partition payload into a PartitionInfo. Each partition is
// its own concept-id time-series, so events arrive one-per-mutation
// with the latest field values flattened into the payload.
func parsePartitionEvent(ev *memqlv1.EventNotification) (cluster.PartitionInfo, bool) {
	if ev == nil || ev.GetPayload() == nil {
		return cluster.PartitionInfo{}, false
	}
	m := ev.GetPayload().AsMap()
	payload := m
	if p, ok := m["payload"].(map[string]any); ok && p != nil {
		payload = p
	}
	name := firstNonEmpty(getString(payload, "name"), getString(m, "id"))
	if name == "" {
		return cluster.PartitionInfo{}, false
	}
	return cluster.PartitionInfo{
		Name:          name,
		PartitionType: firstNonEmpty(getString(payload, "partitionType"), "standard"),
		Status:        firstNonEmpty(getString(payload, "status"), "active"),
	}, true
}

// parsePartitions converts a listPartitions query result into a deduped
// PartitionInfo slice. Mirrors parseClusterNodes' time-series dedup --
// concept==v1:platform:partition returns every historical version, so
// we keep the row with the newest createdAt per partition name.
func parsePartitions(result any) []cluster.PartitionInfo {
	if result == nil {
		return nil
	}
	var items []any
	switch v := result.(type) {
	case []any:
		items = v
	case map[string]any:
		// Reuse the cluster-nodes array extractor -- it already handles
		// the nested shapes the MemQL engine can return (bundle.nodes,
		// result.bundle.nodes, items/results/data). Without this the
		// list looked empty even after createPartition succeeded,
		// because we only tried the top-level keys.
		items = extractNodeArray(v)
		if items == nil {
			items = []any{v}
		}
	default:
		return nil
	}

	type rowWithTime struct {
		info    cluster.PartitionInfo
		created string
	}
	latest := make(map[string]rowWithTime)
	for _, item := range items {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		payload := m
		if p, ok := m["payload"].(map[string]any); ok {
			payload = p
		}
		name := firstNonEmpty(getString(payload, "name"), getString(m, "id"))
		if name == "" {
			continue
		}
		createdAt := firstNonEmpty(
			getString(m, "createdAt"),
			getString(payload, "createdAt"),
			getString(m, "created_at"),
		)
		info := cluster.PartitionInfo{
			Name:          name,
			PartitionType: firstNonEmpty(getString(payload, "partitionType"), "standard"),
			Status:        firstNonEmpty(getString(payload, "status"), "active"),
		}
		if existing, found := latest[name]; found && createdAt <= existing.created {
			continue
		}
		latest[name] = rowWithTime{info: info, created: createdAt}
	}
	out := make([]cluster.PartitionInfo, 0, len(latest))
	for _, r := range latest {
		// Filter out soft-deleted partitions at parse time -- drainin
		// rows shouldn't appear in the CLI list.
		if r.info.Status == "draining" {
			continue
		}
		out = append(out, r.info)
	}
	return out
}

// ensureDefaultPartition guarantees the "default" partition appears
// first in the returned slice, followed by the rest sorted
// alphabetically by name. The bootstrap automation seeds default on
// every cluster, so the pin is never a lie; sorting the tail makes
// the list stable across inserts (adding "acme" and "zzz" always
// lands them in the same visual slots instead of append order).
func ensureDefaultPartition(parts []cluster.PartitionInfo) []cluster.PartitionInfo {
	var def cluster.PartitionInfo
	hasDef := false
	rest := make([]cluster.PartitionInfo, 0, len(parts))
	for _, p := range parts {
		if p.Name == "default" && !hasDef {
			def = p
			hasDef = true
			continue
		}
		rest = append(rest, p)
	}
	if !hasDef {
		def = cluster.PartitionInfo{Name: "default", PartitionType: "standard", Status: "active"}
	}
	sort.Slice(rest, func(i, j int) bool { return rest[i].Name < rest[j].Name })
	return append([]cluster.PartitionInfo{def}, rest...)
}

// parseClusterNodeTypes converts a queryClusterNodeTypes({}) result
// into a NodeTypeInfo slice. Dedupes by `name` keeping the row with
// the newest createdAt (the concept is a time-series, so re-seeding
// produces multiple rows per name), then sorts ascending by
// createdAt so the topology row order matches seed file order
// (bff -> voice -> cognition -> agent -> planner with the current
// seed).
func parseClusterNodeTypes(result any) []cluster.NodeTypeInfo {
	if result == nil {
		return nil
	}
	var items []any
	switch v := result.(type) {
	case []any:
		items = v
	case map[string]any:
		items = extractNodeArray(v)
		if items == nil {
			items = []any{v}
		}
	default:
		return nil
	}

	type rowWithTime struct {
		info    cluster.NodeTypeInfo
		created string
	}
	latest := make(map[string]rowWithTime)
	for _, item := range items {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
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

// extractNodeArray digs through nested map wrappers to find an array.
// Handles MemQL response formats: {bundle: {nodes: [...]}} or {result: {bundle: {nodes: [...]}}}
func extractNodeArray(m map[string]any) []any {
	// Direct array keys.
	for _, key := range []string{"nodes", "items", "results", "data"} {
		if arr, ok := m[key].([]any); ok {
			return arr
		}
	}
	// Nested: { bundle: { nodes: [...] } }
	if bundle, ok := m["bundle"].(map[string]any); ok {
		if arr, ok := bundle["nodes"].([]any); ok {
			return arr
		}
	}
	// Nested: { result: { bundle: { nodes: [...] } } }
	if result, ok := m["result"].(map[string]any); ok {
		if bundle, ok := result["bundle"].(map[string]any); ok {
			if arr, ok := bundle["nodes"].([]any); ok {
				return arr
			}
		}
	}
	return nil
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
	// that cluster to "selected" (drives Explorer / Agents) and,
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

	// OnAuthorize fires when the user submits the Add/Edit form with
	// the Discovery URL field filled. The whole pipeline runs off the
	// UI thread so the screen stays responsive while the browser flow
	// is open: discover the well-known doc, persist the resolved
	// cluster row, mint a token via OAuth (browser), cache the token,
	// and finally restart the pool entry so the new credentials get
	// picked up by a fresh dial cycle.
	a.clustersView.OnAuthorize = func(discoveryURL, existingName string) {
		go a.runAuthorizeFlow(discoveryURL, existingName)
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

		// Open a pool entry for it -- the 3-attempt lifecycle starts
		// automatically. User can press Enter (once it connects) to
		// make it their selected cluster.
		a.openEntry(c)

		// Highlight the new row + move the "viewed" pointer so the
		// topology and partitions panes follow it. Without setViewed
		// here, the highlight cursor visually jumps to the new row
		// but the right-pane topology and bottom-left partitions stay
		// frozen on whatever cluster was viewed before -- the same
		// thing the arrow keys would normally do is what this
		// shortcut should do too.
		for i, cs := range a.clustersView.Clusters {
			if cs.Config.Name == c.Name {
				a.clustersView.Selected = i
				break
			}
		}
		a.setViewed(c.Name)
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

	// Delete a cluster (local cannot be deleted). Deleting the cluster
	// the user was viewing OR working against re-homes both the viewed
	// and selected pointers onto the row the cluster-list now
	// highlights -- local in practice, since it's always at index 0.
	// Without this rehoming the topology + partitions panes would stay
	// frozen on the deleted cluster's title/state until the user
	// manually arrowed back.
	a.clustersView.OnDelete = func(clusterName string) {
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
		_ = config.DeleteToken(clusterName)

		// Close the pool entry and drop any references to the deleted
		// cluster before the list refresh -- so refreshClusterList sees
		// a clean pool when it stamps row statuses.
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
		if entry != nil {
			entry.Close()
		}
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
			// Rehome the viewed pointer (and its topology + partitions
			// side-effects) whenever the viewed cluster went away.
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
}

// wirePartitionsCallbacks hooks the partition manager's callbacks into
// the app's pool-backed state machine. All mutations target the
// currently-selected cluster's dispatcher; the partition list is then
// re-rendered from the cluster's local snapshot, with CDC events
// trickling in shortly after to confirm.
func (a *App) wirePartitionsCallbacks() {
	if a.clustersView == nil || a.clustersView.Partitions == nil {
		return
	}
	pv := a.clustersView.Partitions

	// Highlight changes don't touch any state -- partition selection is
	// driven by Enter only. Hook stays empty so future work can add a
	// detail pane without revisiting wiring.
	pv.OnHighlight = func(name string) {}

	// Enter promotes a partition to "selected" for the VIEWED cluster
	// (the one the user is highlighting in the cluster list -- same
	// target as the topology pane). Pushes the partition onto that
	// cluster's dispatcher so subsequent queries against it are
	// partition-stamped, and persists the choice to clusters.yaml.
	pv.OnEnter = func(name string) {
		if name == "" {
			return
		}
		a.poolMu.RLock()
		clusterName := a.viewed
		entry := a.pool[clusterName]
		a.poolMu.RUnlock()
		if clusterName == "" || entry == nil {
			return
		}
		entry.mu.Lock()
		entry.SelectedPartition = name
		conn := entry.Conn
		entry.mu.Unlock()
		if conn != nil && conn.Dispatcher() != nil {
			conn.Dispatcher().SetPartition(name)
		}
		a.persistSelectedPartition(clusterName, name)
		a.refreshPartitionsView()
		a.postRedraw()
	}

	// MemQL's top-level function-call parser requires object keys to
	// be quoted strings (parser.go:576 "function argument keys must be
	// strings"). The automation DSL is more permissive for inline use,
	// but Execute() goes through the strict parser. Keep the quoted
	// form here so mutations round-trip cleanly.
	//
	// Duplicate names are intercepted before the mutation runs.
	// Without this guard, createPartition would happily insert a new
	// time-series version of the same id and silently overwrite the
	// existing partition's type / config -- the user thinks they're
	// adding a new partition but they're actually editing the old
	// one. The check + warning notification matches the cluster
	// duplicate behavior on the top-half pane.
	pv.OnAdd = func(p cluster.PartitionInfo) {
		a.poolMu.RLock()
		entry := a.pool[a.viewed]
		a.poolMu.RUnlock()
		if entry != nil {
			for _, existing := range entry.snapshotPartitions() {
				if existing.Name == p.Name {
					a.notifications.SyncMeta("partition:add", ui.SeverityWarning,
						fmt.Sprintf("Partition %q already exists -- nothing added.", p.Name))
					return
				}
			}
		}
		a.notifications.Clear("partition:add")
		a.runPartitionMutation("create "+p.Name, fmt.Sprintf(
			`mutationCreatePartition({"name": %q, "partitionType": %q, "config": {}})`,
			p.Name, p.PartitionType,
		), p.Name)
	}
	pv.OnSave = func(p cluster.PartitionInfo) {
		// Edit isn't exposed in the CLI any more, but OnSave stays
		// wired for future use (e.g. config-only edits) so the
		// mutation path is still exercised.
		a.runPartitionMutation("update "+p.Name, fmt.Sprintf(
			`mutationUpdatePartition({"name": %q, "partitionType": %q, "config": {}})`,
			p.Name, p.PartitionType,
		))
	}
	pv.OnDelete = func(name string) {
		if name == "default" || name == "" {
			return
		}
		a.runPartitionMutation("delete "+name,
			fmt.Sprintf(`mutationDeletePartition({"name": %q})`, name))
	}
}

// runPartitionMutation executes a partition CRUD MemQL call against
// the VIEWED (arrow-key-highlighted) cluster's dispatcher -- not the
// selected/working cluster. The partitions pane is a management tool
// scoped to whichever cluster the user is looking at, so edits go
// there too. On success it re-pulls the partition list (don't rely
// solely on CDC -- events can lag or arrive with a shape the parser
// doesn't match yet); on failure it surfaces the error to the header
// notifications feed so the user actually sees that the mutation
// failed instead of silently staring at an unchanged list.
//
// If selectOnSuccess is non-empty, the named partition becomes the
// viewed entry's active partition once the list refresh confirms it
// exists -- used by OnAdd so creating "test" also routes subsequent
// queries at it without the user having to press Enter afterwards.
//
// verb is a short human-readable label like "create test" used only
// for the notification message.
func (a *App) runPartitionMutation(verb, call string, selectOnSuccess ...string) {
	a.poolMu.RLock()
	clusterName := a.viewed
	entry := a.pool[clusterName]
	a.poolMu.RUnlock()
	var d *client.Dispatcher
	if entry != nil {
		entry.mu.Lock()
		state := entry.State
		conn := entry.Conn
		entry.mu.Unlock()
		if state == stateConnected && conn != nil {
			d = conn.Dispatcher()
		}
	}
	if d == nil || entry == nil {
		a.notifications.Sync(
			"partition:mutation",
			ui.SeverityError,
			fmt.Sprintf("Cannot %s: %q is not connected.", verb, clusterName),
		)
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		queries := client.NewQueryClient(d)
		if _, err := queries.Execute(ctx, call); err != nil {
			if a.logger != nil {
				a.logger.Warn("partition mutation failed", "call", call, "error", err)
			}
			a.notifications.Sync(
				"partition:mutation",
				ui.SeverityError,
				fmt.Sprintf("Partition %s failed: %v", verb, err),
			)
			return
		}
		// Mutation succeeded. Clear any prior error notification, then
		// re-read the partition list directly -- CDC will also update
		// it shortly, but explicit re-read guarantees the user sees the
		// change immediately regardless of event routing.
		a.notifications.Clear("partition:mutation")
		parts := entry.partitionsInitialLoad(context.Background())

		// Belt-and-braces: the mutation itself just succeeded, so the
		// row DEFINITELY exists server-side. If the parse didn't
		// surface it (shape mismatch, race with CDC, etc.), force-add
		// it here so the user sees the partition they just created.
		// No-op for delete (empty selectOnSuccess).
		if len(selectOnSuccess) > 0 && selectOnSuccess[0] != "" {
			have := false
			for _, p := range parts {
				if p.Name == selectOnSuccess[0] {
					have = true
					break
				}
			}
			if !have {
				parts = append(parts, cluster.PartitionInfo{
					Name:          selectOnSuccess[0],
					PartitionType: "standard",
					Status:        "active",
				})
			}
		}

		entry.mu.Lock()
		entry.Partitions = parts
		// Auto-select the newly-created partition so the user's next
		// query routes at it without an extra Enter press.
		if len(selectOnSuccess) > 0 && selectOnSuccess[0] != "" {
			entry.SelectedPartition = selectOnSuccess[0]
			if entry.Conn != nil && entry.Conn.Dispatcher() != nil {
				entry.Conn.Dispatcher().SetPartition(selectOnSuccess[0])
			}
		}
		part := entry.SelectedPartition
		entry.mu.Unlock()
		if len(selectOnSuccess) > 0 && selectOnSuccess[0] != "" && part == selectOnSuccess[0] {
			a.persistSelectedPartition(clusterName, part)
		}
		if a.isViewed(clusterName) {
			a.refreshPartitionsView()
		}
		a.postRedraw()
	}()
}

// persistSelectedPartition writes the per-cluster sticky partition
// choice to clusters.yaml so the next launch restores it.
func (a *App) persistSelectedPartition(clusterName, partition string) {
	clusters, err := config.LoadClusters()
	if err != nil || clusters == nil {
		return
	}
	changed := false
	for i := range clusters.Clusters {
		if clusters.Clusters[i].Name == clusterName {
			if clusters.Clusters[i].SelectedPartition != partition {
				clusters.Clusters[i].SelectedPartition = partition
				changed = true
			}
			break
		}
	}
	if !changed {
		// First write for a cluster that isn't yet in yaml (e.g. "local"
		// using the hardcoded default). Append a thin row so the
		// selection survives the next load.
		appended := false
		for _, c := range clusters.Clusters {
			if c.Name == clusterName {
				appended = true
				break
			}
		}
		if !appended {
			cfg := config.ClusterConfig{Name: clusterName, SelectedPartition: partition}
			if clusterName == "local" {
				local := cluster.LocalClusterConfig()
				cfg.Endpoint = local.Endpoint
			}
			clusters.Clusters = append(clusters.Clusters, cfg)
			changed = true
		}
	}
	if !changed {
		return
	}
	if err := config.SaveClusters(clusters); err != nil && a.logger != nil {
		a.logger.Warn("failed to persist selected partition", "cluster", clusterName, "error", err)
	}
}

// updateTabGating sets the GatedMessage on the Concepts tab based on
// whether the user's selected cluster has a live connection. Called
// from draw() so state changes show up on the next repaint.
func (a *App) updateTabGating() {
	name := a.selectedName()
	if name == "" {
		a.conceptsView.GatedMessage = "No cluster selected. Switch to the Clusters tab (F1) and press Enter on a cluster."
		return
	}
	a.poolMu.RLock()
	entry := a.pool[name]
	a.poolMu.RUnlock()
	if entry == nil {
		a.conceptsView.GatedMessage = fmt.Sprintf("Selected cluster %q is not open. Switch to the Clusters tab (F1) and press Enter on its row.", name)
		return
	}
	state, _, _ := entry.stateSnapshot()
	if state == stateConnected {
		a.conceptsView.GatedMessage = ""
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
	a.conceptsView.GatedMessage = fmt.Sprintf("Selected cluster %q is %s. Available again once it reaches a connected state.", name, why)
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
	// Gate Explorer / Agents on the selected cluster being
	// connected. When it isn't, those views render a centered
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
	// Show() emits ONLY the cells that changed since the last frame
	// (tcell diffs the back buffer against the visible state).
	// Sync() re-emits every cell unconditionally -- correct after a
	// terminal resize or visible corruption, but called every frame
	// it produces a full-screen repaint that the terminal renders
	// as a visible flash. The post-resize Sync() call in Run() is
	// the legitimate use of that primitive.
	a.screen.Show()
}

