package cluster

// Interactive topology-as-canvas, PHASE 1 (memql-cockpit#237).
//
// Phase 1 is a LIST-based builder: the operator assembles a desired
// topology as an ordered, editable list of per-node-type entries (node
// type + replica count + optional pinned version) -- a "draft
// composition". It is seeded from the selected deployment's
// deploymentNodeSpec set (Epic 2 / memql#2094) when loaded, else from the
// cluster's known node types + their live replica counts. The clickable-
// node canvas (which the cli/canvas pixel framebuffer exists to render) is
// a later phase; phase 1 deliberately stays on the list representation.
//
// The model (composeBuilder) is PURE -- navigation + edits are state-only,
// no screen, no SDK -- so it is unit-testable in isolation. toSpecs() is
// the hand-off point a later phase (or an apply/cut action) will consume.
//
// As a mode it mirrors the Architecture navigator (the X toggle): while
// active it owns the topology region + all keys; 'N' or Esc closes it.

import (
	"fmt"
	"strings"

	"github.com/gdamore/tcell/v2"

	"github.com/znasllc-io/memql-cockpit/cli/ui"
)

const (
	composeMinReplicas = 0
	composeMaxReplicas = 99
)

// composeRow is one entry in the list-based topology builder.
type composeRow struct {
	NodeType string
	Replicas int
	Version  string // optional pinned version ("" = resolve against the spine)
}

// composeBuilder is the pure model behind the phase-1 list builder.
type composeBuilder struct {
	rows   []composeRow
	cursor int
}

// newComposeBuilderFromSpecs seeds the builder from a deployment's
// deploymentNodeSpec set. Rows with no replica count default to 1.
func newComposeBuilderFromSpecs(specs []NodeSpecInfo) *composeBuilder {
	b := &composeBuilder{}
	for _, s := range specs {
		if strings.TrimSpace(s.NodeType) == "" {
			continue
		}
		r := composeRow{NodeType: s.NodeType, Replicas: s.Replicas, Version: s.Version}
		if r.Replicas <= 0 {
			r.Replicas = 1
		}
		b.rows = append(b.rows, r)
	}
	return b
}

// seedFromTypes seeds the builder from the cluster's known node types,
// taking each type's replica count from the live node count (>=1).
func (b *composeBuilder) seedFromTypes(types []string, liveCounts map[string]int) {
	for _, t := range types {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		n := liveCounts[t]
		if n <= 0 {
			n = 1
		}
		b.rows = append(b.rows, composeRow{NodeType: t, Replicas: n})
	}
}

func (b *composeBuilder) empty() bool { return len(b.rows) == 0 }

func (b *composeBuilder) clampCursor() {
	if b.cursor >= len(b.rows) {
		b.cursor = len(b.rows) - 1
	}
	if b.cursor < 0 {
		b.cursor = 0
	}
}

func (b *composeBuilder) moveUp() {
	if b.cursor > 0 {
		b.cursor--
	}
}

func (b *composeBuilder) moveDown() {
	if b.cursor < len(b.rows)-1 {
		b.cursor++
	}
}

// adjustReplicas changes the selected row's replica count by delta, clamped
// to [composeMinReplicas, composeMaxReplicas].
func (b *composeBuilder) adjustReplicas(delta int) {
	if b.cursor < 0 || b.cursor >= len(b.rows) {
		return
	}
	n := b.rows[b.cursor].Replicas + delta
	if n < composeMinReplicas {
		n = composeMinReplicas
	}
	if n > composeMaxReplicas {
		n = composeMaxReplicas
	}
	b.rows[b.cursor].Replicas = n
}

// addType appends nodeType (replicas 1) and moves the cursor to it. No-op
// when nodeType is empty or already present; returns whether it added.
func (b *composeBuilder) addType(nodeType string) bool {
	nodeType = strings.TrimSpace(nodeType)
	if nodeType == "" {
		return false
	}
	for _, r := range b.rows {
		if r.NodeType == nodeType {
			return false
		}
	}
	b.rows = append(b.rows, composeRow{NodeType: nodeType, Replicas: 1})
	b.cursor = len(b.rows) - 1
	return true
}

// removeSelected drops the row under the cursor.
func (b *composeBuilder) removeSelected() {
	if b.cursor < 0 || b.cursor >= len(b.rows) {
		return
	}
	b.rows = append(b.rows[:b.cursor], b.rows[b.cursor+1:]...)
	b.clampCursor()
}

// nextAddableType returns the first known type not already in the builder,
// or "" when every known type is present. Drives the 'A' (add) key.
func (b *composeBuilder) nextAddableType(known []string) string {
	have := make(map[string]bool, len(b.rows))
	for _, r := range b.rows {
		have[r.NodeType] = true
	}
	for _, t := range known {
		t = strings.TrimSpace(t)
		if t != "" && !have[t] {
			return t
		}
	}
	return ""
}

// toSpecs exports the built composition as a NodeSpecInfo slice -- the
// hand-off point a later phase / apply action consumes.
func (b *composeBuilder) toSpecs() []NodeSpecInfo {
	out := make([]NodeSpecInfo, 0, len(b.rows))
	for _, r := range b.rows {
		out = append(out, NodeSpecInfo{NodeType: r.NodeType, Replicas: r.Replicas, Version: r.Version})
	}
	return out
}

// -----------------------------------------------------------------------------
// View integration
// -----------------------------------------------------------------------------

// knownTypesLocked returns the cluster's node types in display order: the
// seeded nodeType list first, then any extra types present on live nodes.
// Caller MUST hold v.mu.
func (v *View) knownTypesLocked() []string {
	seen := make(map[string]bool)
	var out []string
	for _, t := range v.NodeTypes {
		if t.Name != "" && !seen[t.Name] {
			seen[t.Name] = true
			out = append(out, t.Name)
		}
	}
	for _, n := range v.Nodes {
		if n.Type != "" && !seen[n.Type] {
			seen[n.Type] = true
			out = append(out, n.Type)
		}
	}
	return out
}

// openBuilderLocked enters the topology builder, seeding it from the
// selected deployment's spec set when loaded, else from the cluster's known
// node types + live replica counts. Caller MUST hold v.mu (write).
func (v *View) openBuilderLocked() {
	selID := ""
	if v.deploySelected >= 0 && v.deploySelected < len(v.deployments) {
		selID = v.deployments[v.deploySelected].ID
	}
	if selID != "" && v.deploySpecsLoadedFor == selID && len(v.deploySpecs) > 0 {
		v.builder = newComposeBuilderFromSpecs(v.deploySpecs)
		return
	}
	b := &composeBuilder{}
	counts := make(map[string]int)
	for _, n := range v.Nodes {
		counts[n.Type]++
	}
	b.seedFromTypes(v.knownTypesLocked(), counts)
	v.builder = b
}

// handleBuilderKeyLocked routes a key while the builder mode is active.
// Caller MUST hold v.mu (write). Returns true (the builder owns the pane).
func (v *View) handleBuilderKeyLocked(key *tcell.EventKey) bool {
	b := v.builder
	switch key.Key() {
	case tcell.KeyEscape:
		v.closeBuilderLocked()
		v.requestRedrawLocked()
		return true
	case tcell.KeyEnter:
		// Apply / cut-from-composition (phase 2, #253).
		v.openComposeApplyLocked()
		return true
	case tcell.KeyUp:
		b.moveUp()
		v.requestRedrawLocked()
		return true
	case tcell.KeyDown:
		b.moveDown()
		v.requestRedrawLocked()
		return true
	case tcell.KeyRune:
		switch key.Rune() {
		case 'n', 'N':
			v.closeBuilderLocked()
		case 'v', 'V':
			// Toggle list <-> clickable-node canvas rendering (phase 2, #253).
			v.builderCanvas = !v.builderCanvas
		case '+', '=':
			b.adjustReplicas(1)
		case '-', '_':
			b.adjustReplicas(-1)
		case 'a', 'A':
			if t := b.nextAddableType(v.knownTypesLocked()); t != "" {
				b.addType(t)
			}
		case 'd', 'D', 'x', 'X':
			b.removeSelected()
		}
		v.requestRedrawLocked()
		return true
	}
	return true
}

// closeBuilderLocked exits the builder and resets its phase-2 view + apply
// state. Caller MUST hold v.mu (write).
func (v *View) closeBuilderLocked() {
	v.builder = nil
	v.builderCanvas = false
	v.compApply = nil
	v.composeBoxes = nil
}

// drawBuilder renders the list-based builder into region. Caller holds
// v.mu.RLock (Draw frame).
func (v *View) drawBuilder(screen *ui.Screen, region ui.Rect) {
	titleStyle := tcell.StyleDefault.Foreground(v.Theme.Accent).Background(v.Theme.BG).Bold(true)
	muted := v.Theme.SubtleStyle()
	x := region.X + 2
	w := region.Width - 4
	if w <= 0 {
		return
	}
	y := region.Y + 1
	screen.DrawText(x, y, w, "TOPOLOGY BUILDER (phase 1)", titleStyle)
	y++
	screen.DrawText(x, y, w, "Draft per-tier composition (node type x replicas).", muted)
	y += 2

	if v.builder.empty() {
		screen.DrawText(x, y, w, "No node types yet. Press A to add one.", muted)
		return
	}

	bottom := region.Y + region.Height - 1
	for i, r := range v.builder.rows {
		if y > bottom {
			break
		}
		style := v.Theme.BaseStyle()
		marker := "  "
		if i == v.builder.cursor {
			style = v.Theme.SelectionStyle()
			marker = "> " // ASCII -- edge-glyph rule (cli/CLAUDE.md)
			screen.FillRect(region.X, y, region.Width, 1, style)
		}
		line := fmt.Sprintf("%s%-12s x%-2d", marker, nodeTypeShort(r.NodeType), r.Replicas)
		if r.Version != "" {
			line += "  " + r.Version
		}
		screen.DrawText(x, y, w, clipText(line, w), style)
		y++
	}
}
