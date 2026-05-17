// Package concepts renders the Concepts tab: the unified browser
// that replaced the Explorer + Agents tabs. Three panes -- concept
// picker (left), row list with search (middle), generic detail
// renderer (right). Backed by ListConcepts + ExecuteQuery; no
// per-concept renderer (Hybrid C: walk the payload + metadata
// generically so new concepts work the day they're declared).
package concepts

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/gdamore/tcell/v2"
	memqlv1 "github.com/visionarys-io/memql/component/grpc/gen"

	"github.com/visionarys-io/memql-cockpit/cli/client"
	"github.com/visionarys-io/memql-cockpit/cli/ui"
)

// FocusPane identifies which pane has keyboard focus.
type FocusPane int

const (
	FocusConcepts FocusPane = 0
	FocusRows     FocusPane = 1
	FocusDetail   FocusPane = 2
)

// View renders the Concepts tab.
type View struct {
	Theme ui.Theme

	// Concept registry (left pane).
	Concepts        []*memqlv1.ConceptInfo
	conceptSelected int
	conceptScrollY  int

	// Loaded rows for the selected concept (middle pane).
	Rows      []map[string]any
	rowFilter string // text from the search box
	rowMatches []int // indices into Rows that match rowFilter (cached)
	rowSelected int
	rowScrollY  int

	// Detail (right pane).
	detailLines  []string
	detailScroll int

	// Version overlay -- when versionsOpen is true the detail pane
	// shows the time-series of versions for the selected row instead
	// of the current snapshot.
	versionsOpen   bool
	versionRows    []map[string]any
	versionSelected int
	versionScrollY int

	Focus    FocusPane
	searchOn bool

	// Plumbing: how to talk to the active cluster's QueryClient.
	// Set by the app layer; reset to nil when no cluster is connected
	// (which also flips GatedMessage on for the placeholder).
	QueryClient func() *client.QueryClient

	// GatedMessage, when non-empty, replaces the layout with a
	// centered "not available" message. Set by the app layer when
	// the selected cluster has no live connection.
	GatedMessage string

	// onStatus is set by the app layer so the view can push
	// transient errors to the notification bar without importing
	// the app package.
	OnStatus func(msg string)
}

// NewView creates an empty Concepts view.
func NewView(theme ui.Theme) *View {
	return &View{
		Theme: theme,
		Focus: FocusConcepts,
	}
}

// SetConcepts replaces the concept registry. Sorting is `domain:entity`
// alphabetical so cognition/agents/etc. stay grouped.
func (v *View) SetConcepts(concepts []*memqlv1.ConceptInfo) {
	v.Concepts = make([]*memqlv1.ConceptInfo, 0, len(concepts))
	for _, c := range concepts {
		if c == nil {
			continue
		}
		v.Concepts = append(v.Concepts, c)
	}
	sort.Slice(v.Concepts, func(i, j int) bool {
		return v.Concepts[i].GetId() < v.Concepts[j].GetId()
	})
	if v.conceptSelected >= len(v.Concepts) {
		v.conceptSelected = 0
	}
	v.refreshRowsFromCurrent()
}

// Draw renders the Concepts tab.
func (v *View) Draw(screen *ui.Screen, bounds ui.Rect) {
	if v.GatedMessage != "" {
		v.drawGated(screen, bounds)
		return
	}

	panes := ui.FlexColumn(bounds, []ui.FlexItem{
		{Flex: 0.25, MinSize: 24},
		{Flex: 0.30, MinSize: 28},
		{Flex: 0.45, MinSize: 32},
	})
	conceptBounds := panes[0]
	rowBounds := panes[1]
	detailBounds := panes[2]

	// Dividers (single-cell box-drawing chars; safe at pane edges).
	divStyle := v.Theme.SubtleStyle()
	for _, x := range []int{conceptBounds.X + conceptBounds.Width - 1, rowBounds.X + rowBounds.Width - 1} {
		for y := bounds.Y; y < bounds.Y+bounds.Height; y++ {
			screen.SetCell(x, y, '│', divStyle)
		}
	}
	conceptBounds.Width--
	rowBounds.Width--

	v.drawConceptList(screen, conceptBounds)
	v.drawRowList(screen, rowBounds)
	v.drawDetail(screen, detailBounds)
}

func (v *View) drawGated(screen *ui.Screen, bounds ui.Rect) {
	screen.FillRect(bounds.X, bounds.Y, bounds.Width, bounds.Height, v.Theme.BaseStyle())
	title := " CONCEPTS "
	screen.DrawText(bounds.X+1, bounds.Y, bounds.Width-2, title, v.Theme.SubtleStyle().Bold(true))
	midY := bounds.Y + bounds.Height/2
	lineX := bounds.X + (bounds.Width-len(v.GatedMessage))/2
	if lineX < bounds.X+1 {
		lineX = bounds.X + 1
	}
	screen.DrawText(lineX, midY, bounds.Width-1, v.GatedMessage, v.Theme.SubtleStyle())
}

func (v *View) drawConceptList(screen *ui.Screen, bounds ui.Rect) {
	screen.FillRect(bounds.X, bounds.Y, bounds.Width, bounds.Height, v.Theme.BaseStyle())

	titleStyle := v.Theme.SubtleStyle().Bold(true)
	if v.Focus == FocusConcepts {
		titleStyle = v.Theme.AccentStyle().Bold(true)
	}
	screen.DrawText(bounds.X+1, bounds.Y, bounds.Width-2, " CONCEPTS ", titleStyle)

	if len(v.Concepts) == 0 {
		v.drawCentered(screen, bounds, "No concepts loaded.")
		return
	}

	listTop := bounds.Y + 2
	listHeight := bounds.Height - 3
	if listHeight < 1 {
		return
	}

	v.clampConceptScroll(listHeight)

	for i := 0; i < listHeight && v.conceptScrollY+i < len(v.Concepts); i++ {
		idx := v.conceptScrollY + i
		c := v.Concepts[idx]
		y := listTop + i

		style := v.Theme.BaseStyle()
		if idx == v.conceptSelected {
			style = tcell.StyleDefault.Foreground(v.Theme.FG).Background(tcell.NewRGBColor(40, 44, 52))
		}
		screen.FillRect(bounds.X, y, bounds.Width, 1, style)
		label := c.GetId()
		screen.DrawText(bounds.X+2, y, bounds.Width-3, label, style)
	}

	footer := footerCount(v.conceptSelected, len(v.Concepts))
	screen.DrawText(bounds.X+1, bounds.Y+bounds.Height-1, bounds.Width-2, footer, v.Theme.SubtleStyle())
}

func (v *View) drawRowList(screen *ui.Screen, bounds ui.Rect) {
	screen.FillRect(bounds.X, bounds.Y, bounds.Width, bounds.Height, v.Theme.BaseStyle())

	titleStyle := v.Theme.SubtleStyle().Bold(true)
	if v.Focus == FocusRows {
		titleStyle = v.Theme.AccentStyle().Bold(true)
	}
	conceptId := ""
	if v.conceptSelected >= 0 && v.conceptSelected < len(v.Concepts) {
		conceptId = v.Concepts[v.conceptSelected].GetId()
	}
	title := " ROWS "
	if conceptId != "" {
		title = " ROWS: " + conceptId + " "
	}
	screen.DrawText(bounds.X+1, bounds.Y, bounds.Width-2, title, titleStyle)

	// Search row.
	searchY := bounds.Y + 1
	searchStyle := v.Theme.SubtleStyle()
	prefix := "/search: "
	display := prefix + v.rowFilter
	if v.searchOn {
		display = "[search] " + v.rowFilter + "_"
		searchStyle = v.Theme.AccentStyle()
	}
	screen.DrawText(bounds.X+1, searchY, bounds.Width-2, display, searchStyle)

	if len(v.Rows) == 0 {
		v.drawCentered(screen, ui.Rect{X: bounds.X, Y: bounds.Y + 3, Width: bounds.Width, Height: bounds.Height - 3}, "No rows.")
		return
	}

	listTop := bounds.Y + 3
	listHeight := bounds.Height - 4
	if listHeight < 1 {
		return
	}

	matches := v.rowMatches
	v.clampRowScroll(listHeight)

	for i := 0; i < listHeight && v.rowScrollY+i < len(matches); i++ {
		idx := matches[v.rowScrollY+i]
		row := v.Rows[idx]
		y := listTop + i

		style := v.Theme.BaseStyle()
		if v.rowScrollY+i == v.rowSelected {
			style = tcell.StyleDefault.Foreground(v.Theme.FG).Background(tcell.NewRGBColor(40, 44, 52))
		}
		screen.FillRect(bounds.X, y, bounds.Width, 1, style)
		label := rowDisplayLabel(row)
		screen.DrawText(bounds.X+2, y, bounds.Width-3, label, style)
	}

	footer := footerCount(v.rowSelected, len(matches))
	if len(matches) < len(v.Rows) {
		footer = fmt.Sprintf(" %d/%d (filtered from %d) ", v.rowSelected+1, len(matches), len(v.Rows))
	}
	screen.DrawText(bounds.X+1, bounds.Y+bounds.Height-1, bounds.Width-2, footer, v.Theme.SubtleStyle())
}

func (v *View) drawDetail(screen *ui.Screen, bounds ui.Rect) {
	screen.FillRect(bounds.X, bounds.Y, bounds.Width, bounds.Height, v.Theme.BaseStyle())

	titleStyle := v.Theme.SubtleStyle().Bold(true)
	if v.Focus == FocusDetail {
		titleStyle = v.Theme.AccentStyle().Bold(true)
	}
	title := " DETAIL "
	if v.versionsOpen {
		title = " VERSIONS (Esc to close) "
	}
	screen.DrawText(bounds.X+1, bounds.Y, bounds.Width-2, title, titleStyle)

	if v.versionsOpen {
		v.drawVersionsList(screen, bounds)
		return
	}

	if len(v.detailLines) == 0 {
		v.drawCentered(screen, bounds, "Select a row to see its rendered detail.")
		return
	}

	contentTop := bounds.Y + 2
	contentH := bounds.Height - 3
	if contentH < 1 {
		return
	}
	v.clampDetailScroll(contentH)
	for i := 0; i < contentH && v.detailScroll+i < len(v.detailLines); i++ {
		screen.DrawText(bounds.X+2, contentTop+i, bounds.Width-3, v.detailLines[v.detailScroll+i], v.Theme.BaseStyle())
	}
	footer := fmt.Sprintf(" line %d/%d ", v.detailScroll+1, len(v.detailLines))
	screen.DrawText(bounds.X+1, bounds.Y+bounds.Height-1, bounds.Width-2, footer, v.Theme.SubtleStyle())
}

func (v *View) drawVersionsList(screen *ui.Screen, bounds ui.Rect) {
	if len(v.versionRows) == 0 {
		v.drawCentered(screen, bounds, "No version history for this row.")
		return
	}
	contentTop := bounds.Y + 2
	contentH := bounds.Height - 3
	if contentH < 1 {
		return
	}
	for i := 0; i < contentH && v.versionScrollY+i < len(v.versionRows); i++ {
		idx := v.versionScrollY + i
		ver := v.versionRows[idx]
		y := contentTop + i
		style := v.Theme.BaseStyle()
		if idx == v.versionSelected {
			style = tcell.StyleDefault.Foreground(v.Theme.FG).Background(tcell.NewRGBColor(40, 44, 52))
		}
		screen.FillRect(bounds.X, y, bounds.Width, 1, style)
		when := getString(ver, "createdAt")
		who := getString(ver, "createdBy")
		prov := getString(getMap(ver, "provenance"), "kind")
		label := fmt.Sprintf("%s  by %s  (%s)", shortenTimestamp(when), who, prov)
		screen.DrawText(bounds.X+2, y, bounds.Width-3, label, style)
	}
	footer := footerCount(v.versionSelected, len(v.versionRows))
	screen.DrawText(bounds.X+1, bounds.Y+bounds.Height-1, bounds.Width-2, footer, v.Theme.SubtleStyle())
}

func (v *View) drawCentered(screen *ui.Screen, bounds ui.Rect, msg string) {
	midY := bounds.Y + bounds.Height/2
	lineX := bounds.X + (bounds.Width-len(msg))/2
	if lineX < bounds.X+1 {
		lineX = bounds.X + 1
	}
	screen.DrawText(lineX, midY, bounds.Width-1, msg, v.Theme.SubtleStyle())
}

// HandleEvent processes keyboard input for the Concepts tab.
func (v *View) HandleEvent(ev tcell.Event) bool {
	keyEv, ok := ev.(*tcell.EventKey)
	if !ok {
		return false
	}

	// When the search box is active, keys go to the search box first.
	if v.searchOn {
		return v.handleSearchKey(keyEv)
	}

	if keyEv.Key() == tcell.KeyTab {
		v.Focus = (v.Focus + 1) % 3
		return true
	}

	// Slash anywhere -> jump to search.
	if keyEv.Key() == tcell.KeyRune && keyEv.Rune() == '/' {
		v.Focus = FocusRows
		v.searchOn = true
		return true
	}
	// `v` toggles version history when a row is selected.
	if keyEv.Key() == tcell.KeyRune && (keyEv.Rune() == 'v' || keyEv.Rune() == 'V') {
		if v.versionsOpen {
			v.versionsOpen = false
		} else {
			v.openVersions()
		}
		return true
	}

	switch v.Focus {
	case FocusConcepts:
		return v.handleConceptListKey(keyEv)
	case FocusRows:
		return v.handleRowListKey(keyEv)
	case FocusDetail:
		return v.handleDetailKey(keyEv)
	}
	return false
}

func (v *View) handleConceptListKey(ev *tcell.EventKey) bool {
	switch ev.Key() {
	case tcell.KeyUp:
		if v.conceptSelected > 0 {
			v.conceptSelected--
			v.refreshRowsFromCurrent()
		}
		return true
	case tcell.KeyDown:
		if v.conceptSelected < len(v.Concepts)-1 {
			v.conceptSelected++
			v.refreshRowsFromCurrent()
		}
		return true
	case tcell.KeyEnter:
		v.refreshRowsFromCurrent()
		v.Focus = FocusRows
		return true
	}
	return false
}

func (v *View) handleRowListKey(ev *tcell.EventKey) bool {
	matches := v.rowMatches
	switch ev.Key() {
	case tcell.KeyUp:
		if v.rowSelected > 0 {
			v.rowSelected--
			v.refreshDetailFromCurrent()
		}
		return true
	case tcell.KeyDown:
		if v.rowSelected < len(matches)-1 {
			v.rowSelected++
			v.refreshDetailFromCurrent()
		}
		return true
	case tcell.KeyEnter:
		v.refreshDetailFromCurrent()
		v.Focus = FocusDetail
		return true
	}
	return false
}

func (v *View) handleDetailKey(ev *tcell.EventKey) bool {
	switch ev.Key() {
	case tcell.KeyUp:
		if v.detailScroll > 0 {
			v.detailScroll--
		}
		return true
	case tcell.KeyDown:
		v.detailScroll++
		return true
	case tcell.KeyPgUp:
		v.detailScroll -= 10
		if v.detailScroll < 0 {
			v.detailScroll = 0
		}
		return true
	case tcell.KeyPgDn:
		v.detailScroll += 10
		return true
	case tcell.KeyHome:
		v.detailScroll = 0
		return true
	case tcell.KeyEsc:
		if v.versionsOpen {
			v.versionsOpen = false
			return true
		}
		v.Focus = FocusRows
		return true
	}
	return false
}

func (v *View) handleSearchKey(ev *tcell.EventKey) bool {
	switch ev.Key() {
	case tcell.KeyEsc, tcell.KeyEnter:
		v.searchOn = false
		return true
	case tcell.KeyBackspace, tcell.KeyBackspace2:
		if len(v.rowFilter) > 0 {
			v.rowFilter = v.rowFilter[:len(v.rowFilter)-1]
			v.recomputeRowMatches()
		}
		return true
	case tcell.KeyRune:
		v.rowFilter += string(ev.Rune())
		v.recomputeRowMatches()
		return true
	}
	return false
}

// refreshRowsFromCurrent reloads rows for the currently-selected
// concept via the active QueryClient. Runs synchronously -- the view's
// keystroke handlers don't return control until rows are loaded, so a
// slow node feels like a slow keystroke. Acceptable for v1; switch to
// a background fetch + spinner once we have one.
func (v *View) refreshRowsFromCurrent() {
	v.Rows = nil
	v.detailLines = nil
	v.rowSelected = 0
	v.rowScrollY = 0
	v.versionsOpen = false
	if v.QueryClient == nil {
		return
	}
	if v.conceptSelected < 0 || v.conceptSelected >= len(v.Concepts) {
		return
	}
	qc := v.QueryClient()
	if qc == nil {
		return
	}
	conceptId := v.Concepts[v.conceptSelected].GetId()
	if conceptId == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	res, err := qc.Execute(ctx, fmt.Sprintf("node(concept==%s)", conceptId))
	if err != nil {
		if v.OnStatus != nil {
			v.OnStatus(fmt.Sprintf("rows load failed: %v", err))
		}
		v.recomputeRowMatches()
		return
	}
	v.Rows = extractRows(res)
	v.recomputeRowMatches()
	v.refreshDetailFromCurrent()
}

func (v *View) refreshDetailFromCurrent() {
	v.detailScroll = 0
	matches := v.rowMatches
	if v.rowSelected < 0 || v.rowSelected >= len(matches) {
		v.detailLines = nil
		return
	}
	row := v.Rows[matches[v.rowSelected]]
	v.detailLines = renderRowDetail(row)
}

// openVersions fetches the time-series history for the selected row
// (concept + id) and swaps the detail pane into the version-list view.
func (v *View) openVersions() {
	if v.QueryClient == nil {
		return
	}
	matches := v.rowMatches
	if v.rowSelected < 0 || v.rowSelected >= len(matches) {
		return
	}
	row := v.Rows[matches[v.rowSelected]]
	rowId := getString(row, "id")
	conceptId := getString(row, "concept")
	if conceptId == "" && v.conceptSelected >= 0 && v.conceptSelected < len(v.Concepts) {
		conceptId = v.Concepts[v.conceptSelected].GetId()
	}
	if rowId == "" || conceptId == "" {
		return
	}
	qc := v.QueryClient()
	if qc == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	res, err := qc.Execute(ctx, fmt.Sprintf("node(concept==%s;id==%s)", conceptId, rowId))
	if err != nil {
		if v.OnStatus != nil {
			v.OnStatus(fmt.Sprintf("version history failed: %v", err))
		}
		return
	}
	v.versionRows = extractRows(res)
	sort.Slice(v.versionRows, func(i, j int) bool {
		return getString(v.versionRows[i], "createdAt") > getString(v.versionRows[j], "createdAt")
	})
	v.versionSelected = 0
	v.versionScrollY = 0
	v.versionsOpen = true
}

func (v *View) recomputeRowMatches() {
	filter := strings.TrimSpace(strings.ToLower(v.rowFilter))
	v.rowMatches = v.rowMatches[:0]
	for idx, row := range v.Rows {
		if filter == "" || rowMatchesFilter(row, filter) {
			v.rowMatches = append(v.rowMatches, idx)
		}
	}
	if v.rowSelected >= len(v.rowMatches) {
		v.rowSelected = 0
	}
	v.refreshDetailFromCurrent()
}

func (v *View) clampConceptScroll(visibleRows int) {
	if v.conceptSelected < v.conceptScrollY {
		v.conceptScrollY = v.conceptSelected
	}
	if v.conceptSelected >= v.conceptScrollY+visibleRows {
		v.conceptScrollY = v.conceptSelected - visibleRows + 1
	}
	if v.conceptScrollY < 0 {
		v.conceptScrollY = 0
	}
}

func (v *View) clampRowScroll(visibleRows int) {
	if v.rowSelected < v.rowScrollY {
		v.rowScrollY = v.rowSelected
	}
	if v.rowSelected >= v.rowScrollY+visibleRows {
		v.rowScrollY = v.rowSelected - visibleRows + 1
	}
	if v.rowScrollY < 0 {
		v.rowScrollY = 0
	}
}

func (v *View) clampDetailScroll(visibleRows int) {
	maxScroll := len(v.detailLines) - visibleRows
	if maxScroll < 0 {
		maxScroll = 0
	}
	if v.detailScroll > maxScroll {
		v.detailScroll = maxScroll
	}
	if v.detailScroll < 0 {
		v.detailScroll = 0
	}
}

// --- pure helpers --------------------------------------------------

func rowMatchesFilter(row map[string]any, filter string) bool {
	if id := getString(row, "id"); strings.Contains(strings.ToLower(id), filter) {
		return true
	}
	payload := getMap(row, "payload")
	for _, key := range []string{"name", "displayName", "slug", "id", "title"} {
		if v := getString(payload, key); strings.Contains(strings.ToLower(v), filter) {
			return true
		}
	}
	return false
}

func rowDisplayLabel(row map[string]any) string {
	payload := getMap(row, "payload")
	for _, key := range []string{"name", "displayName", "slug", "title"} {
		if v := getString(payload, key); v != "" {
			return v
		}
	}
	return getString(row, "id")
}

func footerCount(sel, total int) string {
	if total == 0 {
		return ""
	}
	return fmt.Sprintf(" %d/%d ", sel+1, total)
}

func shortenTimestamp(ts string) string {
	if t, err := time.Parse(time.RFC3339, ts); err == nil {
		return t.Format("2006-01-02 15:04:05Z")
	}
	return ts
}

// extractRows pulls a flat slice of row maps out of a MemQL query
// response. Mirrors the unwinding logic in app.go's extractAgentRows
// but kept inline so this package stays self-contained.
func extractRows(result any) []map[string]any {
	if result == nil {
		return nil
	}
	switch v := result.(type) {
	case []any:
		out := make([]map[string]any, 0, len(v))
		for _, item := range v {
			if m, ok := item.(map[string]any); ok {
				out = append(out, m)
			}
		}
		return out
	case map[string]any:
		if arr, ok := v["nodes"].([]any); ok {
			out := make([]map[string]any, 0, len(arr))
			for _, item := range arr {
				if m, ok := item.(map[string]any); ok {
					out = append(out, m)
				}
			}
			return out
		}
		// Single row -- wrap.
		return []map[string]any{v}
	}
	return nil
}

func getString(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	v, ok := m[key]
	if !ok {
		return ""
	}
	s, _ := v.(string)
	return s
}

func getMap(m map[string]any, key string) map[string]any {
	if m == nil {
		return nil
	}
	v, ok := m[key]
	if !ok {
		return nil
	}
	out, _ := v.(map[string]any)
	return out
}

// --- generic renderer (Hybrid C) -----------------------------------

// renderRowDetail walks the row map and emits a human-readable
// declaration-style block. No concept-specific knowledge: every field
// renders the same way, so a brand-new concept is browsable the day
// it's declared. Sections: intrinsics (id, concept, createdBy,
// createdAt), payload (with nested object/array indentation),
// provenance (the engine intrinsic).
func renderRowDetail(row map[string]any) []string {
	if row == nil {
		return nil
	}
	var lines []string

	lines = append(lines, "INTRINSICS")
	for _, key := range []string{"id", "concept", "type", "createdBy", "createdAt", "partition"} {
		if v := getString(row, key); v != "" {
			lines = append(lines, fmt.Sprintf("  %-12s %s", key, v))
		}
	}

	if payload := getMap(row, "payload"); payload != nil {
		lines = append(lines, "")
		lines = append(lines, "PAYLOAD")
		lines = append(lines, renderObject(payload, 1)...)
	}

	if meta := getMap(row, "metadata"); meta != nil {
		if prov := getMap(meta, "provenance"); prov != nil {
			lines = append(lines, "")
			lines = append(lines, "PROVENANCE")
			lines = append(lines, renderObject(prov, 1)...)
		}
	}
	if prov := getMap(row, "provenance"); prov != nil {
		lines = append(lines, "")
		lines = append(lines, "PROVENANCE")
		lines = append(lines, renderObject(prov, 1)...)
	}

	return lines
}

func renderObject(obj map[string]any, depth int) []string {
	if obj == nil {
		return nil
	}
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	indent := strings.Repeat("  ", depth)
	var lines []string
	for _, k := range keys {
		v := obj[k]
		switch val := v.(type) {
		case map[string]any:
			lines = append(lines, fmt.Sprintf("%s%s:", indent, k))
			lines = append(lines, renderObject(val, depth+1)...)
		case []any:
			lines = append(lines, fmt.Sprintf("%s%s: (%d items)", indent, k, len(val)))
			lines = append(lines, renderArray(val, depth+1)...)
		default:
			lines = append(lines, fmt.Sprintf("%s%-20s %s", indent, k+":", renderScalar(val)))
		}
	}
	return lines
}

func renderArray(arr []any, depth int) []string {
	indent := strings.Repeat("  ", depth)
	var lines []string
	limit := 10
	for i, v := range arr {
		if i >= limit {
			lines = append(lines, fmt.Sprintf("%s... (%d more)", indent, len(arr)-limit))
			break
		}
		switch val := v.(type) {
		case map[string]any:
			lines = append(lines, fmt.Sprintf("%s[%d]:", indent, i))
			lines = append(lines, renderObject(val, depth+1)...)
		default:
			lines = append(lines, fmt.Sprintf("%s[%d] %s", indent, i, renderScalar(val)))
		}
	}
	return lines
}

func renderScalar(v any) string {
	if v == nil {
		return "null"
	}
	switch t := v.(type) {
	case string:
		return t
	case bool:
		if t {
			return "true"
		}
		return "false"
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return fmt.Sprintf("%v", v)
		}
		return string(b)
	}
}
