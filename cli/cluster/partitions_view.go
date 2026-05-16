package cluster

import (
	"fmt"

	"github.com/gdamore/tcell/v2"
	"github.com/visionarys-io/memql-cockpit/cli/ui"
	"github.com/visionarys-io/memql/core/id"
)

// PartitionInfo describes one v1:platform:partition row for the
// partitions list. Loaded by the app from the connected cluster's
// pool entry; the view itself is presentation-only.
type PartitionInfo struct {
	Name          string
	PartitionType string // "standard" / "dedicated" / "personal"
	Status        string // "active" / "draining" -- draining rows are filtered out at app layer
}

// PartitionsView renders the partition list that lives in the bottom
// half of the Clusters tab's left pane. Mirrors ClustersView's row
// rendering: status icon, name, `*` marker for the selected partition,
// detail block under the highlighted row, footer hints. No state
// machine -- partitions don't dial anything.
type PartitionsView struct {
	Theme    ui.Theme
	Focused  bool // true when this pane has keyboard focus
	Empty    bool // true when no cluster connected -- show "Waiting for cluster..."
	EmptyMsg string

	Partitions   []PartitionInfo
	Selected     int    // highlight index
	Active       string // "selected" partition name (Enter-pressed) -- gets the `*` marker
	scrollOffset int    // first visible row index inside the scrollable viewport

	// Add/edit form state.
	addForm     partitionFormState
	showAddForm bool

	// Delete confirmation.
	confirmDelete bool

	// Callbacks set by the app layer.
	OnAdd       func(p PartitionInfo)
	OnSave      func(p PartitionInfo)
	OnDelete    func(name string)
	OnEnter     func(name string)
	OnHighlight func(name string)
}

const partitionFieldCount = 2

const (
	partFieldName = iota
	partFieldType
)

type partitionFormState struct {
	fields    [partitionFieldCount]string
	cursor    int
	editMode  bool
	editName  string
	typeIndex int // 0=standard, 1=dedicated, 2=personal
	// formError is set when Enter validation fails; cleared on the
	// next keystroke so the user can retry without an explicit dismiss.
	formError string
}

var partitionTypes = []string{"standard", "dedicated", "personal"}

// NewPartitionsView creates an empty partitions list.
func NewPartitionsView(theme ui.Theme) *PartitionsView {
	return &PartitionsView{
		Theme:    theme,
		Empty:    true,
		EmptyMsg: "Waiting for cluster...",
	}
}

// SetPartitions replaces the rendered list.
func (v *PartitionsView) SetPartitions(parts []PartitionInfo) {
	v.Partitions = parts
	if v.Selected >= len(parts) {
		v.Selected = 0
	}
	v.Empty = false
}

// Reset clears the list and shows the empty-state message.
func (v *PartitionsView) Reset(emptyMsg string) {
	v.Partitions = nil
	v.Selected = 0
	v.Active = ""
	v.Empty = true
	if emptyMsg != "" {
		v.EmptyMsg = emptyMsg
	} else {
		v.EmptyMsg = "Waiting for cluster..."
	}
}

// FormOpen reports whether the add/edit form is currently active.
// Used by the parent view to gate Tab (pane cycling should be
// disabled while the user is typing into a form -- see the check
// in ClustersView.HandleEvent). Mirrored on ClustersView so every
// pane that hosts an input form exposes the same predicate.
func (v *PartitionsView) FormOpen() bool {
	return v.showAddForm
}

// Draw renders the partitions pane within bounds. Caller is
// responsible for sizing/positioning relative to the cluster list.
func (v *PartitionsView) Draw(screen *ui.Screen, bounds ui.Rect) {
	x := bounds.X + 1
	maxW := bounds.Width - 2
	y := bounds.Y

	// Pane title -- rewrites to 'ADD/EDIT PARTITION' while the form
	// is open, so there's one title strip per pane regardless of mode.
	titleStyle := v.Theme.SubtleStyle().Bold(true)
	if v.Focused {
		titleStyle = v.Theme.AccentStyle().Bold(true)
	}
	title := " PARTITIONS "
	if v.showAddForm {
		if v.addForm.editMode {
			title = " EDIT PARTITION "
		} else {
			title = " ADD PARTITION "
		}
	}
	screen.DrawText(x, y, maxW, title, titleStyle)
	y++

	if v.showAddForm {
		v.drawForm(screen, x, y, maxW)
		return
	}
	// Pane title (PARTITIONS) above is the only header -- a "PARTITION
	// MANAGER" subtitle was redundant. Rows start one line below.

	// Content region excludes the title row on top. ui.DrawBottom
	// anchors text to the last row of this region and wraps as needed,
	// so long empty-state messages like
	// '"visionarys" is unreachable. Press Enter on the cluster row to
	// retry.' stack onto a second row instead of getting truncated.
	contentBounds := ui.Rect{X: bounds.X, Y: bounds.Y + 1, Width: bounds.Width, Height: bounds.Height - 1}

	if v.Empty {
		ui.DrawBottom(screen, contentBounds, v.Theme.SubtleStyle(), 2, v.EmptyMsg)
		return
	}

	// Layout below the PARTITIONS title:
	//
	//   [list]      -- scrollable partition list (whatever remains)
	//   [gap]       -- 1 blank row so chrome has breathing room
	//   [chrome]    -- action hints, or delete-confirmation prompt
	//
	// Fixed height reserved for gap + chrome so the list never eats
	// their space. When the list overflows its viewport an
	// accent-colored indicator renders via ui.DrawScrollbar.
	const chromeGapH = 1
	chromeH := 1
	if v.confirmDelete && v.Selected >= 0 && v.Selected < len(v.Partitions) && v.Partitions[v.Selected].Name != "default" {
		chromeH = 2
	}
	viewportTop := y + 1 // blank row between title and list
	viewportH := bounds.Y + bounds.Height - viewportTop - chromeGapH - chromeH
	if viewportH < 1 {
		viewportH = 1
	}

	// "default" is pinned at index 0 as a sticky header when there's
	// at least one other partition; a subtle ── divider separates it
	// from the sorted rest so the list visually groups the invariant
	// fallback apart from user-created partitions. The pinned row
	// stays put while the rest scrolls.
	hasPinned := len(v.Partitions) > 0 && v.Partitions[0].Name == "default"
	hasRest := len(v.Partitions) > 1
	headerH := 0
	if hasPinned && hasRest {
		headerH = 2
	}
	scrollH := viewportH - headerH
	if scrollH < 1 {
		scrollH = 1
	}

	restTotal := len(v.Partitions)
	dataOffset := 0
	if headerH > 0 {
		restTotal = len(v.Partitions) - 1
		dataOffset = 1
	}
	restSelected := v.Selected - dataOffset
	if restSelected < 0 {
		v.scrollOffset = ui.ScrollTo(v.scrollOffset, 0, restTotal, scrollH)
	} else {
		v.scrollOffset = ui.ScrollTo(v.scrollOffset, restSelected, restTotal, scrollH)
	}

	// Each row lays out as:
	//
	//   ▸ name                 [type]  *
	//
	// Partitions don't have an online/offline lifecycle the way
	// cluster nodes do, so no status circle -- the row is just the
	// name, the type label right-aligned in Subtle, and the `*`
	// marker for the active partition. Draining rows filter out at
	// snapshot time; nothing that shows up here needs a color.
	const nameCol = 2 // one blank after the selection arrow
	drawPartitionRow := func(p PartitionInfo, dataIdx int, rowY int) {
		rowStyle := v.Theme.BaseStyle()
		if dataIdx == v.Selected {
			rowStyle = tcell.StyleDefault.Foreground(v.Theme.FG).Background(tcell.NewRGBColor(40, 44, 52))
		}
		screen.FillRect(bounds.X, rowY, bounds.Width, 1, rowStyle)

		if dataIdx == v.Selected {
			screen.SetCell(x, rowY, '▸', rowStyle.Foreground(v.Theme.Accent))
		}

		nameStyle := rowStyle
		if p.Name == v.Active {
			nameStyle = rowStyle.Foreground(v.Theme.Accent).Bold(true)
		}

		// Reserve the rightmost columns for the type label, diamond
		// marker, and scrollbar so the name column doesn't overflow.
		markerCol := bounds.X + bounds.Width - 2
		typeEnd := markerCol - 2 // 1 space gap, type column ends here
		typeText := p.PartitionType
		nameStart := x + nameCol
		if typeText != "" && typeEnd-nameStart > 4 {
			typeStart := typeEnd - len(typeText)
			if typeStart < nameStart+len(p.Name)+1 {
				typeText = ""
			} else {
				typeStyle := rowStyle.Foreground(v.Theme.Subtle)
				screen.DrawText(typeStart, rowY, len(typeText), typeText, typeStyle)
				nameMaxW := typeStart - nameStart - 1
				screen.DrawText(nameStart, rowY, nameMaxW, p.Name, nameStyle)
			}
		}
		if typeText == "" {
			screen.DrawText(nameStart, rowY, maxW-(nameStart-x)-1, p.Name, nameStyle)
		}

		if p.Name == v.Active {
			// Single-cell ASCII marker -- see cli/CLAUDE.md "Layout-
			// edge glyph rule". `◆` here would render as 2 cells on
			// some terminals and push the right-pane divider over.
			screen.SetCell(markerCol, rowY, '*', rowStyle.Foreground(v.Theme.Accent).Bold(true))
		}
	}

	// Sticky header.
	if headerH > 0 {
		drawPartitionRow(v.Partitions[0], 0, viewportTop)
		screen.DrawHLine(bounds.X+1, viewportTop+1, bounds.Width-2, '─', v.Theme.SubtleStyle())
	}

	// Scrollable rest (or the full list when there's no pinned row).
	restStart, restEnd := ui.VisibleRange(v.scrollOffset, restTotal, scrollH)
	for j := restStart; j < restEnd; j++ {
		dataIdx := j + dataOffset
		rowY := viewportTop + headerH + (j - restStart)
		drawPartitionRow(v.Partitions[dataIdx], dataIdx, rowY)
	}

	// Accent-colored scrollbar scoped to the scrollable region.
	ui.DrawScrollbar(screen, v.Theme,
		ui.Rect{X: bounds.X, Y: viewportTop + headerH, Width: bounds.Width, Height: scrollH},
		v.scrollOffset, restTotal,
	)

	// Footer section -- anchored to the bottom of the content region
	// via ui.DrawBottomBlocks, which wraps long lines and stacks
	// colored sections (warning prompt + subtle hint) without the
	// caller doing manual row math.
	subtle := v.Theme.SubtleStyle()
	warn := tcell.StyleDefault.Foreground(v.Theme.Warning).Background(v.Theme.BG)
	canDelete := v.Selected >= 0 && v.Selected < len(v.Partitions) && v.Partitions[v.Selected].Name != "default"

	if v.confirmDelete && canDelete {
		name := v.Partitions[v.Selected].Name
		ui.DrawBottomBlocks(screen, contentBounds, 1,
			ui.BottomBlock{Lines: []string{fmt.Sprintf("Soft-delete %q?", name)}, Style: warn},
			ui.BottomBlock{Lines: []string{"Y:Confirm  Esc:Cancel"}, Style: subtle},
		)
		return
	}

	// Enter:Select only shows when the highlighted row is NOT already
	// the active partition. Pressing Enter on the active row would
	// be a no-op, so the hint shouldn't claim it does anything.
	// Same convention used by ClustersView -- see its drawManagement
	// for the matching predicate.
	hint := "A:Add"
	if v.Selected >= 0 && v.Selected < len(v.Partitions) && v.Partitions[v.Selected].Name != v.Active {
		hint += "  Enter:Select"
	}
	if canDelete {
		hint += "  D:Del"
	}
	ui.DrawBottom(screen, contentBounds, subtle, 1, hint)
}

// HandleEvent processes keys when the partitions pane has focus.
func (v *PartitionsView) HandleEvent(ev tcell.Event) bool {
	keyEv, ok := ev.(*tcell.EventKey)
	if !ok {
		return false
	}

	if v.showAddForm {
		return v.handleFormEvent(keyEv)
	}
	if v.confirmDelete {
		return v.handleDeleteConfirm(keyEv)
	}

	switch keyEv.Key() {
	case tcell.KeyUp:
		if v.Selected > 0 {
			v.Selected--
			v.fireOnHighlight()
		}
		return true
	case tcell.KeyDown:
		if v.Selected < len(v.Partitions)-1 {
			v.Selected++
			v.fireOnHighlight()
		}
		return true
	case tcell.KeyEnter:
		if v.Selected >= 0 && v.Selected < len(v.Partitions) && v.OnEnter != nil {
			v.OnEnter(v.Partitions[v.Selected].Name)
		}
		return true
	case tcell.KeyRune:
		switch keyEv.Rune() {
		case 'a', 'A':
			v.showAddForm = true
			v.addForm = partitionFormState{}
			return true
		// 'E' (edit) intentionally omitted -- partitions have no
		// mutable fields from the CLI today. Create + soft-delete
		// are the only operations exposed.
		case 'd', 'D':
			if v.Selected >= 0 && v.Selected < len(v.Partitions) && v.Partitions[v.Selected].Name != "default" {
				v.confirmDelete = true
			}
			return true
		}
	}
	return false
}

func (v *PartitionsView) fireOnHighlight() {
	if v.OnHighlight == nil || v.Selected < 0 || v.Selected >= len(v.Partitions) {
		return
	}
	v.OnHighlight(v.Partitions[v.Selected].Name)
}

func (v *PartitionsView) handleDeleteConfirm(ev *tcell.EventKey) bool {
	switch {
	case ev.Key() == tcell.KeyEscape:
		v.confirmDelete = false
		return true
	case ev.Key() == tcell.KeyRune && (ev.Rune() == 'y' || ev.Rune() == 'Y'):
		if v.Selected >= 0 && v.Selected < len(v.Partitions) && v.OnDelete != nil {
			v.OnDelete(v.Partitions[v.Selected].Name)
		}
		v.confirmDelete = false
		return true
	default:
		v.confirmDelete = false
		return true
	}
}

func (v *PartitionsView) drawForm(screen *ui.Screen, x, y, maxW int) {
	// Pane title already reads 'ADD PARTITION' / 'EDIT PARTITION' in
	// Draw, so no inline title here -- avoid stacking two copies.
	// One blank line of breathing room before the first field.
	y++

	labels := [partitionFieldCount]string{partFieldName: "Name", partFieldType: "Type"}
	for i := 0; i < partitionFieldCount; i++ {
		labelStyle := v.Theme.BaseStyle()
		if i == v.addForm.cursor {
			labelStyle = v.Theme.AccentStyle()
		}
		// Name is read-only when editing existing rows so the concept id stays stable.
		readOnly := v.addForm.editMode && i == partFieldName
		if readOnly {
			labelStyle = v.Theme.SubtleStyle()
		}
		screen.DrawText(x+1, y, 10, labels[i], labelStyle)

		fieldX := x + 12
		fieldW := minInt(maxW-13, 25)
		fieldBG := tcell.NewRGBColor(35, 38, 45)
		if i == v.addForm.cursor && !readOnly {
			fieldBG = tcell.NewRGBColor(50, 55, 65)
		}
		fieldStyle := tcell.StyleDefault.Foreground(v.Theme.FG).Background(fieldBG)
		screen.FillRect(fieldX, y, fieldW, 1, fieldStyle)

		var text string
		switch i {
		case partFieldName:
			text = v.addForm.fields[partFieldName]
		case partFieldType:
			text = partitionTypes[v.addForm.typeIndex]
		}
		screen.DrawText(fieldX+1, y, fieldW-2, text, fieldStyle)
		if i == v.addForm.cursor && i == partFieldName && !readOnly {
			cursorX := fieldX + 1 + len(text)
			if cursorX < fieldX+fieldW-1 {
				screen.SetCell(cursorX, y, ' ', tcell.StyleDefault.Background(v.Theme.FG))
			}
		}
		y += 2
	}

	// Inline validation error (if any) just above the hint block.
	// Wrapped via ui.WrapText so it never truncates in narrow panes.
	if v.addForm.formError != "" {
		errStyle := tcell.StyleDefault.Foreground(v.Theme.Error).Background(v.Theme.BG)
		for _, ln := range ui.WrapText(v.addForm.formError, maxW) {
			screen.DrawText(x, y, maxW, ln, errStyle)
			y++
		}
		y++
	}

	// Hints split across two rows so the narrow left-pane width
	// doesn't truncate "Esc:Cancel". Navigation on top, save/exit on
	// the bottom.
	subtle := v.Theme.SubtleStyle()
	screen.DrawText(x, y, maxW, "↑/↓:Next Field   ←/→:Cycle Type", subtle)
	y++
	screen.DrawText(x, y, maxW, "Enter:Save       Esc:Cancel", subtle)
}

func (v *PartitionsView) handleFormEvent(ev *tcell.EventKey) bool {
	switch ev.Key() {
	case tcell.KeyEscape:
		v.showAddForm = false
		return true
	// Tab is reserved globally for pane navigation; explicitly
	// swallow it here so it doesn't accidentally drive anything while
	// a form is open. Forms move between fields with ↑/↓ only.
	case tcell.KeyTab:
		return true
	case tcell.KeyDown:
		v.addForm.formError = ""
		v.addForm.cursor = (v.addForm.cursor + 1) % partitionFieldCount
		// Skip Name in edit mode (read-only).
		if v.addForm.editMode && v.addForm.cursor == partFieldName {
			v.addForm.cursor = (v.addForm.cursor + 1) % partitionFieldCount
		}
		return true
	case tcell.KeyUp:
		v.addForm.formError = ""
		v.addForm.cursor--
		if v.addForm.cursor < 0 {
			v.addForm.cursor = partitionFieldCount - 1
		}
		if v.addForm.editMode && v.addForm.cursor == partFieldName {
			v.addForm.cursor--
			if v.addForm.cursor < 0 {
				v.addForm.cursor = partitionFieldCount - 1
			}
		}
		return true
	case tcell.KeyLeft:
		// Cycle type backwards while on the Type field (more intuitive
		// than Space for a discrete enum with only 3 values).
		if v.addForm.cursor == partFieldType {
			v.addForm.typeIndex = (v.addForm.typeIndex - 1 + len(partitionTypes)) % len(partitionTypes)
		}
		return true
	case tcell.KeyRight:
		if v.addForm.cursor == partFieldType {
			v.addForm.typeIndex = (v.addForm.typeIndex + 1) % len(partitionTypes)
		}
		return true
	case tcell.KeyEnter:
		// Edit mode keeps the id stable -- no re-validation of Name.
		// Add mode validates: alphanumeric + inner dashes, lowercase,
		// <= MaxNameLen. Same rules the server applies to the concept
		// id, so the user's choice matches what gets persisted.
		var name string
		if v.addForm.editMode {
			name = v.addForm.editName
		} else {
			n, err := id.ValidatePartitionName(v.addForm.fields[partFieldName])
			if err != nil {
				v.addForm.formError = "Name: " + err.Error()
				return true
			}
			name = n
		}
		p := PartitionInfo{
			Name:          name,
			PartitionType: partitionTypes[v.addForm.typeIndex],
			Status:        "active",
		}
		switch {
		case v.addForm.editMode && v.OnSave != nil:
			v.OnSave(p)
		case !v.addForm.editMode && v.OnAdd != nil:
			v.OnAdd(p)
		}
		v.showAddForm = false
		return true
	case tcell.KeyBackspace, tcell.KeyBackspace2:
		v.addForm.formError = ""
		if v.addForm.cursor == partFieldName && !v.addForm.editMode {
			if len(v.addForm.fields[partFieldName]) > 0 {
				v.addForm.fields[partFieldName] = v.addForm.fields[partFieldName][:len(v.addForm.fields[partFieldName])-1]
			}
		}
		return true
	case tcell.KeyRune:
		v.addForm.formError = ""
		// Type field cycles via ←/→ (see KeyLeft/KeyRight above);
		// typing anything else on it is a no-op. The Name field
		// filters to alphanumeric + dash at keystroke time and
		// auto-lowercases so the stored form matches the id.
		if v.addForm.cursor == partFieldName && !v.addForm.editMode {
			r := ev.Rune()
			if !ui.IsNameChar(r) {
				return true
			}
			if len(v.addForm.fields[partFieldName]) >= ui.MaxNameLen {
				return true
			}
			if r >= 'A' && r <= 'Z' {
				r = r + ('a' - 'A')
			}
			v.addForm.fields[partFieldName] += string(r)
		}
		return true
	}
	return false
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

