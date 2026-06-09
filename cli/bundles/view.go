// Package bundles renders the Bundles tab: a read-only operator
// surface over the planner-authored automation bundles
// (v1:authoring:bundle + v1:authoring:construct) that the authoring
// capture spine (memql#1160 / #1161) produces -- one reproducible,
// inspectable bundle per everyday task. The user picks a bundle from
// the left pane and sees its lifecycle metadata + the actual authored
// `.memql` source of every member construct in the detail pane on the
// right. `X` exports the selected bundle's source to files.
//
// This is the visibility half of epic memql#1160 (#1162): the backend
// stores + versions the `.memql`; this tab is the operator's window
// into "the MQL that ran". Editing -> re-author-as-new-version
// (supersedesBundleId) is a follow-up slice.
//
// Data plane: queryAuthoringBundlesForOwner lists the caller's bundles;
// queryAuthoringConstructsForBundle pulls a selected bundle's member
// constructs (lazily, cached per bundle id). Both live in
// memql/dsl/authoring/queries.memql and reach the cockpit through the
// generated SDK methods on QueryClient.
//
// Conventions: follows the standard list+detail layout + keybinding
// chrome contract laid out in cli/CLAUDE.md (mirrors cli/skills).
package bundles

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/znasllc-io/memql-cockpit/cli/ui"
	"github.com/znasllc-io/memql/sdk/go/client"
)

// FocusPane identifies which of the two keyboard-focus regions is
// active. Tab cycles between them.
type FocusPane int

const (
	FocusList   FocusPane = 0
	FocusDetail FocusPane = 1
)

const focusPaneCount = 2

// View is the Bundles tab. Mirrors skills/view.go's locking +
// refresh-loop conventions; adds a lazily-populated per-bundle
// construct cache because the member source is fetched on demand.
type View struct {
	ui.BaseView

	bundles []map[string]any
	// constructs caches the member-construct rows per bundle id, fetched
	// lazily when a bundle is first selected. constructFetching guards a
	// per-bundle in-flight fetch so selection churn can't fan out.
	constructs        map[string][]map[string]any
	constructFetching map[string]bool

	Focus FocusPane

	fetching    bool
	QueryClient func() *client.QueryClient

	bundleList ui.ListPane
	detailPane ui.DetailPane
}

// NewView creates a fresh Bundles tab focused on the list pane.
func NewView(theme ui.Theme) *View {
	v := &View{
		Focus:             FocusList,
		constructs:        map[string][]map[string]any{},
		constructFetching: map[string]bool{},
	}
	v.Theme = theme
	v.bundleList.RowsPerItem = 2
	v.bundleList.Render = v.renderBundleRow
	return v
}

// StartRefreshLoop polls queryAuthoringBundlesForOwner on the given
// interval until stop closes.
func (v *View) StartRefreshLoop(stop <-chan struct{}, interval time.Duration) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-stop
		cancel()
	}()
	v.BaseView.StartRefreshLoop(ctx, interval, v.Refresh)
}

// Refresh re-pulls the bundle list. Best-effort: query failures surface
// to the status bar but leave the existing list in place. After a
// successful pull it kicks off a construct fetch for the selected bundle
// so the detail pane fills in without an extra keystroke.
func (v *View) Refresh() {
	if v.QueryClient == nil {
		return
	}
	qc := v.QueryClient()
	if qc == nil {
		return
	}
	v.Mu.Lock()
	if v.fetching {
		v.Mu.Unlock()
		return
	}
	v.fetching = true
	v.Mu.Unlock()
	defer func() {
		v.Mu.Lock()
		v.fetching = false
		v.Mu.Unlock()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()

	res, err := qc.QueryAuthoringBundlesForOwner(ctx, client.QueryAuthoringBundlesForOwnerArgs{})
	if err != nil {
		if v.OnStatus != nil {
			v.OnStatus(fmt.Sprintf("bundles: queryAuthoringBundlesForOwner failed: %v", err))
		}
		return
	}
	rows := res.Rows()

	// Newest first: a captured bundle is most interesting right after the
	// task that produced it ran. Stable so re-renders don't shuffle rows
	// under the cursor.
	sort.SliceStable(rows, func(i, j int) bool {
		return getString(rows[i], "createdAt") > getString(rows[j], "createdAt")
	})

	v.Mu.Lock()
	v.bundles = rows
	v.bundleList.Count = len(v.bundles)
	if v.bundleList.Selected >= len(v.bundles) {
		v.bundleList.Selected = 0
		v.bundleList.ScrollY = 0
		v.detailPane.ScrollY = 0
	}
	selID := selectedBundleID(v.bundles, v.bundleList.Selected)
	v.Mu.Unlock()

	v.ensureConstructs(selID)
}

// ensureConstructs lazily fetches + caches a bundle's member constructs.
// No-op when the id is empty, already cached, or a fetch is in flight.
func (v *View) ensureConstructs(bundleID string) {
	if bundleID == "" || v.QueryClient == nil {
		return
	}
	v.Mu.Lock()
	if _, ok := v.constructs[bundleID]; ok {
		v.Mu.Unlock()
		return
	}
	if v.constructFetching[bundleID] {
		v.Mu.Unlock()
		return
	}
	v.constructFetching[bundleID] = true
	v.Mu.Unlock()

	go func() {
		defer func() {
			v.Mu.Lock()
			delete(v.constructFetching, bundleID)
			v.Mu.Unlock()
		}()
		qc := v.QueryClient()
		if qc == nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
		defer cancel()
		res, err := qc.QueryAuthoringConstructsForBundle(ctx, client.QueryAuthoringConstructsForBundleArgs{BundleId: bundleID})
		if err != nil {
			if v.OnStatus != nil {
				v.OnStatus(fmt.Sprintf("bundles: queryAuthoringConstructsForBundle failed: %v", err))
			}
			return
		}
		rows := res.Rows()
		// Headline automation first, then the dependency closure by kind +
		// name -- a stable, readable reading order.
		sort.SliceStable(rows, func(i, j int) bool {
			ai := getString(rows[i], "kind") == "automation"
			aj := getString(rows[j], "kind") == "automation"
			if ai != aj {
				return ai
			}
			ki, kj := getString(rows[i], "kind"), getString(rows[j], "kind")
			if ki != kj {
				return ki < kj
			}
			return getString(rows[i], "name") < getString(rows[j], "name")
		})
		v.Mu.Lock()
		v.constructs[bundleID] = rows
		v.Mu.Unlock()
		v.Redraw()
	}()
}

// ---------------------------------------------------------------------------
// Drawing
// ---------------------------------------------------------------------------

// Draw paints the tab. Holds the read lock so concurrent refreshes can't
// tear the underlying slices mid-paint.
func (v *View) Draw(screen *ui.Screen, bounds ui.Rect) {
	v.Mu.RLock()
	defer v.Mu.RUnlock()

	if v.GatedMessage != "" {
		v.drawGated(screen, bounds, v.GatedMessage)
		return
	}

	panes := ui.FlexColumn(bounds, []ui.FlexItem{
		{Flex: 0.38, MinSize: 30},
		{Flex: 0.62, MinSize: 36},
	})
	listBounds := panes[0]
	detailBounds := panes[1]

	divStyle := v.Theme.SubtleStyle()
	for y := bounds.Y; y < bounds.Y+bounds.Height; y++ {
		screen.SetCell(listBounds.X+listBounds.Width-1, y, '│', divStyle)
	}
	listBounds.Width--

	v.drawList(screen, listBounds)
	v.drawDetail(screen, detailBounds)
}

func (v *View) drawGated(screen *ui.Screen, bounds ui.Rect, msg string) {
	screen.FillRect(bounds.X, bounds.Y, bounds.Width, bounds.Height, v.Theme.BaseStyle())
	ui.PaneTitle{Title: "BUNDLES"}.Draw(screen, bounds, v.Theme)
	drawCentered(screen, v.Theme, bounds, msg)
}

func paneChromeBounds(bounds ui.Rect) ui.Rect {
	const chromeH = 1
	listTop := bounds.Y + 2
	listH := bounds.Height - 2 - chromeH
	if listH < 1 {
		listH = 1
	}
	return ui.Rect{X: bounds.X, Y: listTop, Width: bounds.Width, Height: listH}
}

func (v *View) drawList(screen *ui.Screen, bounds ui.Rect) {
	screen.FillRect(bounds.X, bounds.Y, bounds.Width, bounds.Height, v.Theme.BaseStyle())

	ui.PaneTitle{
		Title:   "BUNDLES",
		Counter: ui.FormatCursor(v.bundleList.Selected, len(v.bundles)),
		Focused: v.Focus == FocusList,
	}.Draw(screen, bounds, v.Theme)

	if len(v.bundles) == 0 {
		drawCentered(screen, v.Theme, bounds,
			"No authored bundles yet. The authoring capture spine (memql#1161) writes one per completed task; run a task (e.g. ask the assistant to produce an artifact) and refresh.")
		ui.DrawBottom(screen, bounds, v.Theme.SubtleStyle(), 1, hintsForListEmpty())
		return
	}

	v.bundleList.Count = len(v.bundles)
	v.bundleList.Focused = v.Focus == FocusList
	v.bundleList.Draw(screen, paneChromeBounds(bounds), v.Theme)

	ui.DrawBottom(screen, bounds, v.Theme.SubtleStyle(), 1, hintsForList())
}

// renderBundleRow paints a 2-row bundle entry. Primary: status marker +
// title + "[vN]". Subtitle: status · construct-count · source.
func (v *View) renderBundleRow(screen *ui.Screen, bounds ui.Rect, idx int, sel bool, theme ui.Theme) {
	if idx < 0 || idx >= len(v.bundles) {
		return
	}
	b := v.bundles[idx]
	primary := theme.BaseStyle()
	if sel {
		primary = theme.SelectionStyle()
	}
	sub := primary.Foreground(theme.Subtle)

	title := getString(b, "title")
	if title == "" {
		title = getString(b, "id")
	}
	// Active-row marker is strictly single-width ASCII per the layout-edge
	// glyph rule (cli/CLAUDE.md). '!' flags a failed bundle so it stands out.
	marker := " "
	if getString(b, "status") == "failed" {
		marker = "!"
	}
	headline := fmt.Sprintf("%s %s [v%d]", marker, title, intFrom(b, "version"))
	screen.DrawText(bounds.X+1, bounds.Y, bounds.Width-2, headline, primary)

	status := getString(b, "status")
	if status == "" {
		status = "draft"
	}
	subStr := status
	if src := getString(b, "sourcePlanId"); src != "" {
		subStr = fmt.Sprintf("%s · task %s", subStr, src)
	} else if resp := getString(b, "responsibilityId"); resp != "" {
		subStr = fmt.Sprintf("%s · resp %s", subStr, resp)
	}
	screen.DrawText(bounds.X+3, bounds.Y+1, bounds.Width-4, subStr, sub)
}

func (v *View) drawDetail(screen *ui.Screen, bounds ui.Rect) {
	screen.FillRect(bounds.X, bounds.Y, bounds.Width, bounds.Height, v.Theme.BaseStyle())

	ui.PaneTitle{
		Title:   "AUTHORED MQL",
		Focused: v.Focus == FocusDetail,
	}.Draw(screen, bounds, v.Theme)

	if len(v.bundles) == 0 || v.bundleList.Selected < 0 || v.bundleList.Selected >= len(v.bundles) {
		drawCentered(screen, v.Theme, bounds,
			"Select a bundle from the list to see its lifecycle + the authored .memql source of every member construct.")
		ui.DrawBottom(screen, bounds, v.Theme.SubtleStyle(), 1, hintsForDetailEmpty())
		return
	}

	b := v.bundles[v.bundleList.Selected]
	v.detailPane.Lines = v.buildDetailLines(b)
	v.detailPane.Focused = v.Focus == FocusDetail

	inner := paneChromeBounds(bounds)
	inner.X += 1
	inner.Width -= 1
	if inner.Width < 1 {
		inner.Width = 1
	}
	v.detailPane.Draw(screen, inner, v.Theme)

	ui.DrawBottom(screen, bounds, v.Theme.SubtleStyle(), 1, hintsForDetail())
}

// buildDetailLines flattens the bundle record + its member constructs
// into the detail pane's section-based layout: lifecycle metadata first,
// then each construct's raw `.memql` source under a section header.
func (v *View) buildDetailLines(b map[string]any) []ui.DetailLine {
	var lines []ui.DetailLine
	plain := func(s string) ui.DetailLine { return ui.DetailLine{Kind: ui.LinePlain, Text: s} }
	section := func(s string) ui.DetailLine { return ui.DetailLine{Kind: ui.LineSection, Text: s} }
	kv := func(k, val string) ui.DetailLine { return ui.DetailLine{Kind: ui.LineKV, Key: k, Value: val} }

	lines = append(lines, section("bundle"))
	lines = append(lines, kv("title", getString(b, "title")))
	lines = append(lines, kv("status", getString(b, "status")))
	lines = append(lines, kv("version", fmt.Sprintf("%d", intFrom(b, "version"))))
	if src := getString(b, "sourcePlanId"); src != "" {
		lines = append(lines, kv("from task", src))
	}
	if resp := getString(b, "responsibilityId"); resp != "" {
		lines = append(lines, kv("from resp", resp))
	}
	if sup := getString(b, "supersedesBundleId"); sup != "" {
		lines = append(lines, kv("supersedes", sup))
	}
	lines = append(lines, kv("created", shortenTimestamp(getString(b, "createdAt"))))
	if fr := getString(b, "failureReason"); fr != "" {
		lines = append(lines, plain(""))
		lines = append(lines, kv("failure", fr))
	}
	if summary := getString(b, "summary"); summary != "" {
		lines = append(lines, plain(""))
		lines = append(lines, plain(summary))
	}

	bundleID := getString(b, "id")
	constructs, loaded := v.constructs[bundleID]
	if !loaded {
		lines = append(lines, section("authored source"))
		lines = append(lines, plain("  (loading authored source…)"))
		return lines
	}
	if len(constructs) == 0 {
		lines = append(lines, section("authored source"))
		lines = append(lines, plain("  (no authored constructs recorded for this bundle)"))
		return lines
	}

	for _, c := range constructs {
		kind := getString(c, "kind")
		name := getString(c, "name")
		lines = append(lines, section(fmt.Sprintf("%s %s", kind, name)))
		src := getString(c, "source")
		if strings.TrimSpace(src) == "" {
			lines = append(lines, plain("  (empty source)"))
			continue
		}
		for _, ln := range strings.Split(strings.TrimRight(src, "\n"), "\n") {
			lines = append(lines, plain(ln))
		}
	}
	return lines
}

// ---------------------------------------------------------------------------
// Export
// ---------------------------------------------------------------------------

// exportSelected writes the selected bundle's member constructs to
// `~/.memql/bundles/<bundleId>/` -- one `.memql` file per construct plus a
// MANIFEST.txt with the lifecycle metadata. Surfaces the path (or the
// reason it couldn't) via OnStatus. Best-effort; never blocks the UI.
func (v *View) exportSelected() {
	v.Mu.RLock()
	if v.bundleList.Selected < 0 || v.bundleList.Selected >= len(v.bundles) {
		v.Mu.RUnlock()
		return
	}
	b := v.bundles[v.bundleList.Selected]
	bundleID := getString(b, "id")
	constructs, loaded := v.constructs[bundleID]
	v.Mu.RUnlock()

	if !loaded {
		v.ensureConstructs(bundleID)
		if v.OnStatus != nil {
			v.OnStatus("bundles: source still loading -- press X again in a moment")
		}
		return
	}
	if len(constructs) == 0 {
		if v.OnStatus != nil {
			v.OnStatus("bundles: nothing to export (no authored constructs)")
		}
		return
	}

	home, err := os.UserHomeDir()
	if err != nil {
		if v.OnStatus != nil {
			v.OnStatus(fmt.Sprintf("bundles: export failed (home dir): %v", err))
		}
		return
	}
	dir := filepath.Join(home, ".memql", "bundles", sanitize(bundleID))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		if v.OnStatus != nil {
			v.OnStatus(fmt.Sprintf("bundles: export failed (mkdir): %v", err))
		}
		return
	}

	for _, c := range constructs {
		fname := fmt.Sprintf("%s__%s.memql", sanitize(getString(c, "kind")), sanitize(getString(c, "name")))
		if err := os.WriteFile(filepath.Join(dir, fname), []byte(getString(c, "source")), 0o644); err != nil {
			if v.OnStatus != nil {
				v.OnStatus(fmt.Sprintf("bundles: export failed (%s): %v", fname, err))
			}
			return
		}
	}

	manifest := fmt.Sprintf("title: %s\nstatus: %s\nversion: %d\nsourcePlanId: %s\nresponsibilityId: %s\ncreatedAt: %s\nconstructs: %d\n",
		getString(b, "title"), getString(b, "status"), intFrom(b, "version"),
		getString(b, "sourcePlanId"), getString(b, "responsibilityId"),
		getString(b, "createdAt"), len(constructs))
	_ = os.WriteFile(filepath.Join(dir, "MANIFEST.txt"), []byte(manifest), 0o644)

	if v.OnStatus != nil {
		v.OnStatus(fmt.Sprintf("bundles: exported %d construct(s) to %s", len(constructs), dir))
	}
}

// ---------------------------------------------------------------------------
// Input
// ---------------------------------------------------------------------------

// HandleEvent processes a key event. Returns true if consumed.
func (v *View) HandleEvent(ev tcell.Event) bool {
	keyEv, ok := ev.(*tcell.EventKey)
	if !ok {
		return false
	}

	if keyEv.Key() == tcell.KeyTab {
		v.Mu.Lock()
		v.Focus = (v.Focus + 1) % FocusPane(focusPaneCount)
		v.Mu.Unlock()
		return true
	}

	if keyEv.Key() == tcell.KeyRune && (keyEv.Rune() == 'r' || keyEv.Rune() == 'R') {
		go func() {
			v.Refresh()
			v.Redraw()
		}()
		return true
	}

	if keyEv.Key() == tcell.KeyRune && (keyEv.Rune() == 'x' || keyEv.Rune() == 'X') {
		go func() {
			v.exportSelected()
			v.Redraw()
		}()
		return true
	}

	switch v.Focus {
	case FocusList:
		return v.handleListKey(keyEv)
	case FocusDetail:
		return v.handleDetailKey(keyEv)
	}
	return false
}

func (v *View) handleListKey(ev *tcell.EventKey) bool {
	v.Mu.Lock()
	v.bundleList.Focused = true
	prev := v.bundleList.Selected
	consumed := v.bundleList.HandleEvent(ev)
	changed := consumed && v.bundleList.Selected != prev
	if changed {
		v.detailPane.ScrollY = 0
	}
	selID := selectedBundleID(v.bundles, v.bundleList.Selected)
	v.Mu.Unlock()

	if consumed {
		if changed {
			v.ensureConstructs(selID)
		}
		return true
	}
	if ev.Key() == tcell.KeyEnter {
		v.Mu.Lock()
		v.Focus = FocusDetail
		v.Mu.Unlock()
		v.ensureConstructs(selID)
		return true
	}
	return false
}

func (v *View) handleDetailKey(ev *tcell.EventKey) bool {
	v.Mu.Lock()
	defer v.Mu.Unlock()
	v.detailPane.Focused = true
	if v.detailPane.HandleEvent(ev) {
		return true
	}
	if ev.Key() == tcell.KeyEsc {
		v.Focus = FocusList
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// Hints
// ---------------------------------------------------------------------------

func hintsForList() string {
	// Tab-cycling is advertised in the header chrome (cli/CLAUDE.md: "No
	// Tab:Switch panes duplication"), so the narrow list pane spends its
	// width on the pane-local actions instead.
	bar := ui.HintBar{Chips: []ui.HintChip{
		{Key: "↑/↓", Label: "Move"},
		{Key: "Enter", Label: "Open"},
		{Key: "X", Label: "Export"},
		{Key: "R", Label: "Refresh"},
	}}
	return bar.String()
}

func hintsForListEmpty() string {
	bar := ui.HintBar{Chips: []ui.HintChip{
		{Key: "R", Label: "Refresh"},
		{Key: "Tab", Label: "Cycle"},
	}}
	return bar.String()
}

func hintsForDetail() string {
	bar := ui.HintBar{Chips: []ui.HintChip{
		{Key: "↑/↓", Label: "Scroll"},
		{Key: "PgUp/PgDn", Label: "Page"},
		{Key: "X", Label: "Export"},
		{Key: "Esc", Label: "Back"},
		{Key: "Tab", Label: "Cycle"},
	}}
	return bar.String()
}

func hintsForDetailEmpty() string {
	bar := ui.HintBar{Chips: []ui.HintChip{{Key: "Tab", Label: "Cycle"}}}
	return bar.String()
}

// ---------------------------------------------------------------------------
// Helpers (kept local; mirror the skills view's tolerant readers)
// ---------------------------------------------------------------------------

func selectedBundleID(bundles []map[string]any, sel int) string {
	if sel < 0 || sel >= len(bundles) {
		return ""
	}
	return getString(bundles[sel], "id")
}

func getString(row map[string]any, key string) string {
	if v, ok := row[key].(string); ok {
		return v
	}
	return ""
}

// intFrom reads an integer field tolerantly: JSON numbers materialize as
// float64, but int / int64 are accepted too.
func intFrom(row map[string]any, key string) int {
	switch x := row[key].(type) {
	case float64:
		return int(x)
	case int:
		return x
	case int64:
		return int(x)
	}
	return 0
}

func shortenTimestamp(ts string) string {
	if len(ts) >= 19 {
		return ts[:19]
	}
	return ts
}

// sanitize makes a graph id / construct name safe as a single path
// segment: anything outside [A-Za-z0-9._-] becomes '_'.
func sanitize(s string) string {
	if s == "" {
		return "_"
	}
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	return b.String()
}

func drawCentered(screen *ui.Screen, theme ui.Theme, bounds ui.Rect, msg string) {
	innerW := bounds.Width - 2
	if innerW < 1 {
		innerW = 1
	}
	var lines []string
	for _, paragraph := range strings.Split(msg, "\n") {
		if paragraph == "" {
			lines = append(lines, "")
			continue
		}
		lines = append(lines, ui.WrapText(paragraph, innerW)...)
	}
	startY := bounds.Y + (bounds.Height-len(lines))/2
	if startY < bounds.Y+1 {
		startY = bounds.Y + 1
	}
	for i, line := range lines {
		y := startY + i
		if y >= bounds.Y+bounds.Height {
			break
		}
		lineX := bounds.X + (bounds.Width-len(line))/2
		if lineX < bounds.X+1 {
			lineX = bounds.X + 1
		}
		screen.DrawText(lineX, y, bounds.Width-1, line, theme.SubtleStyle())
	}
}
