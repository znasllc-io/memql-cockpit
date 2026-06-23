package config

import (
	"fmt"
	"strings"

	"github.com/gdamore/tcell/v2"
	"github.com/znasllc-io/memql-cockpit/cli/ui"
	"github.com/znasllc-io/memql/component/secret"
)

const maskGlyph = "********"

// draw paints the screen for the current step. Single bordered panel
// centered in the available space, matching the genesis / runlocal
// wizard chrome.
func (s *state) draw() {
	screen := s.screen
	theme := s.theme
	screen.Clear(theme.BaseStyle())
	sw, sh := screen.Size()

	panelW := 90
	panelH := 30
	if panelW > sw-4 {
		panelW = sw - 4
	}
	if panelH > sh-4 {
		panelH = sh - 4
	}
	px := (sw - panelW) / 2
	py := (sh - panelH) / 2
	screen.DrawBox(px, py, panelW, panelH, theme.SubtleStyle())

	title := " memQL Cockpit -- Configuration "
	screen.DrawText(px+(panelW-len(title))/2, py+1, len(title), title, theme.AccentStyle().Bold(true))

	switch s.step {
	case stepMasterKey, stepConfirmRegenerate:
		s.drawMasterKey(px, py, panelW, panelH)
	default:
		s.drawList(px, py, panelW, panelH)
	}

	screen.Show()
}

// drawList renders the registry rows, the selected row's detail block,
// the (optional) inline edit field, the status line, and the hint bar.
func (s *state) drawList(px, py, panelW, panelH int) {
	theme := s.theme
	screen := s.screen

	subtitle := "memQL env-var registry -- view + edit values, Ctrl+S re-seals the genesis envelope."
	screen.DrawText(px+4, py+3, panelW-8, subtitle, theme.SubtleStyle())

	// Layout: header row, scrollable list, a reserved detail block, a
	// status line, and the hint bar, all in fixed bands at the bottom.
	listTop := py + 5
	hintRow := py + panelH - 2
	statusRow := hintRow - 1
	detailTop := statusRow - 4 // 3-row detail block + 1 gap
	listBottom := detailTop - 1
	viewport := listBottom - listTop
	if viewport < 1 {
		viewport = 1
	}

	s.ensureVisible(viewport)

	// Column header.
	header := fmt.Sprintf("  %-1s %-38s %-14s %s", "", "NAME", "COMPONENT", "VALUE")
	screen.DrawText(px+4, listTop-1, panelW-8, header, theme.SubtleStyle().Bold(true))

	for i := 0; i < viewport; i++ {
		idx := s.scroll + i
		if idx >= len(s.rows) {
			break
		}
		s.drawRow(px+4, listTop+i, panelW-8, idx, idx == s.selected)
	}

	// Detail block for the selected row.
	if len(s.rows) > 0 {
		s.drawDetail(px+4, detailTop, panelW-8, s.selected)
	}

	// Status line.
	if s.status != "" {
		style := theme.BaseStyle().Foreground(theme.Success)
		if s.statusError {
			style = theme.ErrorStyle()
		}
		screen.DrawText(px+4, statusRow, panelW-8, s.status, style)
	}

	// Edit field overlays the status row when editing.
	if s.step == stepEdit {
		s.drawEditField(px+4, statusRow, panelW-8)
	}

	// Hint bar.
	hint := s.hint()
	screen.DrawText(px+4, hintRow, panelW-8, hint, theme.SubtleStyle())
}

func (s *state) drawRow(x, y, w, idx int, selected bool) {
	theme := s.theme
	screen := s.screen
	r := s.rows[idx]

	marker := " " // required/optional marker; ASCII-only (edge-glyph rule)
	if r.required {
		marker = "*"
	}

	// Display value: mask secrets unless revealed.
	val := s.effectiveValue(idx)
	if r.secret && !s.revealed[idx] {
		if val == "" {
			val = "(unset)"
		} else {
			val = maskGlyph
		}
	} else if val == "" {
		val = "(unset)"
	}

	// Pending-edit marker.
	editMark := " "
	if _, ok := s.edits[r.entry.Name]; ok {
		editMark = "~"
	}

	name := r.entry.Name
	if len(name) > 38 {
		name = name[:37] + "_"
	}
	comp := r.entry.Component
	if comp == "" {
		comp = r.entry.Scope
	}
	if len(comp) > 14 {
		comp = comp[:13] + "_"
	}

	line := fmt.Sprintf("%s %-1s %-38s %-14s %s", editMark, marker, name, comp, val)

	style := theme.BaseStyle()
	if selected {
		style = theme.SelectionStyle()
		screen.FillRect(x, y, w, 1, style)
	}
	// The required marker (a leading "*") is carried inline in `line`, so
	// required and optional rows share the same draw style.
	screen.DrawText(x, y, w, line, style)
}

func (s *state) drawDetail(x, y, w, idx int) {
	theme := s.theme
	screen := s.screen
	r := s.rows[idx]

	kindLabel := "variable"
	if r.secret {
		kindLabel = "secret"
	}
	reqLabel := "optional"
	if r.required {
		reqLabel = "required for: " + strings.Join(r.entry.Required, ", ")
	}
	src := "manifest default"
	if r.fromEnv {
		src = "envelope"
	}
	if _, ok := s.edits[r.entry.Name]; ok {
		src += " (edited, unsaved)"
	}

	meta := fmt.Sprintf("%s  -  %s  -  scope: %s  -  source: %s", kindLabel, reqLabel, r.entry.Scope, src)
	screen.DrawText(x, y, w, meta, theme.SubtleStyle())

	desc := r.entry.Description
	if desc == "" {
		desc = "(no description)"
	}
	lines := ui.WrapText(desc, w)
	for i := 0; i < 2 && i < len(lines); i++ {
		screen.DrawText(x, y+1+i, w, lines[i], theme.BaseStyle())
	}
}

func (s *state) drawEditField(x, y, w int) {
	theme := s.theme
	screen := s.screen
	label := "Edit " + s.editName + ": "
	screen.DrawText(x, y, len(label), label, theme.AccentStyle().Bold(true))
	fieldX := x + len(label)
	fieldW := w - len(label)
	if fieldW < 1 {
		return
	}
	bgStyle := tcell.StyleDefault.Background(tcell.NewRGBColor(50, 55, 65)).Foreground(theme.FG)
	screen.FillRect(fieldX, y, fieldW, 1, bgStyle)
	shown := s.editBuf
	if len(shown) > fieldW-2 {
		shown = shown[len(shown)-(fieldW-2):]
	}
	screen.DrawText(fieldX+1, y, fieldW-2, shown, bgStyle)
	cursorX := fieldX + 1 + len(shown)
	if cursorX < fieldX+fieldW-1 {
		screen.SetCell(cursorX, y, ' ', tcell.StyleDefault.Background(theme.FG))
	}
}

// drawMasterKey renders the read-only master-key panel (7.8 / #2111):
// the current MEMQL_MASTER_KEY (masked unless revealed) with a
// provenance note, and the regenerate action. There is intentionally
// no editable field -- the key is display + regenerate only.
func (s *state) drawMasterKey(px, py, panelW, panelH int) {
	theme := s.theme
	screen := s.screen

	subtitle := "MEMQL_MASTER_KEY -- read-only. Regenerate to rotate + re-seal the envelope."
	screen.DrawText(px+4, py+3, panelW-8, subtitle, theme.SubtleStyle())

	key := s.currentMasterKey()
	y := py + 5

	// The key value (masked by default; reveal toggle via R).
	keyLabel := "Master key:"
	screen.DrawText(px+4, y, panelW-8, keyLabel, theme.BaseStyle().Bold(true))
	y++
	var shown string
	switch {
	case key == "":
		shown = "(not set -- " + secret.EnvMasterKey + " is absent from this shell)"
	case s.keyRevealed:
		shown = key
	default:
		shown = maskGlyph + maskGlyph + "  (R to reveal)"
	}
	screen.DrawText(px+6, y, panelW-10, shown, theme.AccentStyle())
	y += 2

	// Provenance note.
	provLabel := "Source:"
	screen.DrawText(px+4, y, panelW-8, provLabel, theme.BaseStyle().Bold(true))
	y++
	screen.DrawText(px+6, y, panelW-10, s.provenanceNote(), theme.SubtleStyle())
	y += 2

	// Regenerate description / confirm prompt.
	if s.step == stepConfirmRegenerate {
		warn := []string{
			"Regenerate the master key?",
			"",
			"This generates a NEW key, re-encrypts the WHOLE genesis envelope",
			"under it, and rewrites the MEMQL_MASTER_KEY line in ~/.bashrc and",
			"~/.zshrc. The old key stops decrypting the envelope. Make sure no",
			"other shell or process is mid-read.",
		}
		for _, ln := range warn {
			screen.DrawText(px+4, y, panelW-8, ln, theme.BaseStyle())
			y++
		}
	} else {
		info := []string{
			"Regenerate (G) rotates the key: a fresh key is generated, the whole",
			"envelope is re-sealed under it, and your shell rc files are re-synced.",
			"The key is never typed or edited -- generate-only.",
		}
		for _, ln := range info {
			screen.DrawText(px+4, y, panelW-8, ln, theme.SubtleStyle())
			y++
		}
	}

	// Status line (regenerate progress / result), one row above the hint.
	statusRow := py + panelH - 3
	if s.status != "" {
		style := theme.BaseStyle().Foreground(theme.Success)
		if s.statusError {
			style = theme.ErrorStyle()
		}
		screen.DrawText(px+4, statusRow, panelW-8, s.status, style)
	}

	// Hint bar.
	screen.DrawText(px+4, py+panelH-2, panelW-8, s.masterKeyHint(), theme.SubtleStyle())
}

// provenanceNote returns the human-readable provenance line for the
// current master key.
func (s *state) provenanceNote() string {
	if s.currentMasterKey() == "" {
		return "no master key available"
	}
	switch s.keyProvenance {
	case provenanceShellRC:
		return "loaded from shell rc (present in the environment at launch)"
	case provenanceGenerated:
		return "generated this session"
	default:
		return "unknown"
	}
}

// masterKeyHint returns the context-aware hint bar for the master-key
// panel and its confirm-regenerate gate.
func (s *state) masterKeyHint() string {
	if s.keyBusy {
		return "Working..."
	}
	if s.step == stepConfirmRegenerate {
		return "Y:Confirm regenerate  N/Esc:Cancel"
	}
	chips := []string{}
	if s.currentMasterKey() != "" {
		if s.keyRevealed {
			chips = append(chips, "R:Hide")
		} else {
			chips = append(chips, "R:Reveal")
		}
		if s.envelopeReady() {
			chips = append(chips, "G:Regenerate")
		}
	}
	chips = append(chips, "B:Back")
	return strings.Join(chips, "  ")
}

// hint returns the context-aware hint bar for the current step.
func (s *state) hint() string {
	switch s.step {
	case stepEdit:
		return "Enter:Commit  Esc:Cancel"
	case stepConfirmKey:
		return "Y:Confirm key  N/Esc:Cancel"
	}
	chips := []string{"Up/Down:Move", "Enter:Edit"}
	if len(s.rows) > 0 && s.rows[s.selected].secret {
		if s.revealed[s.selected] {
			chips = append(chips, "R:Hide")
		} else {
			chips = append(chips, "R:Reveal")
		}
	}
	if len(s.edits) > 0 {
		chips = append(chips, "Ctrl+S:Save")
	}
	chips = append(chips, "K:MasterKey", "B:Back", "Q:Quit")
	return strings.Join(chips, "  ")
}

// ensureVisible scrolls so the selected row is within the viewport.
func (s *state) ensureVisible(viewport int) {
	if s.selected < s.scroll {
		s.scroll = s.selected
	}
	if s.selected >= s.scroll+viewport {
		s.scroll = s.selected - viewport + 1
	}
	if s.scroll < 0 {
		s.scroll = 0
	}
}
