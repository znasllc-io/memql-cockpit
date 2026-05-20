// Package cluster -- partition manager stub.
//
// The partition dimension was retired in #56. The cockpit's
// partition manager UI is dead surface; this file is a minimal stub
// so the existing ClustersView wiring still compiles. Full removal
// of the partition pane + focus cycling + persisted selection lands
// in a follow-up PR.
package cluster

import (
	"github.com/gdamore/tcell/v2"

	"github.com/znasllc-io/memql-cockpit/cli/ui"
)

// PartitionInfo is the legacy partition row type retained as a stub
// so callers compile. No live caller populates a real list anymore.
type PartitionInfo struct {
	Name          string
	PartitionType string
	Status        string
}

// PartitionsView is the empty-state placeholder for the legacy
// partition pane.
type PartitionsView struct {
	Theme   ui.Theme
	Focused bool

	// Legacy callback hooks. Kept on the struct so app.go's wiring
	// compiles unchanged; they are never invoked post-#56.
	OnAdd       func(p PartitionInfo)
	OnDelete    func(name string)
	OnUpdate    func(p PartitionInfo)
	OnSelect    func(name string)
	OnHighlight func(name string)
	OnEnter     func(name string)
	OnSave      func(p PartitionInfo)
}

// NewPartitionsView returns an empty placeholder view.
func NewPartitionsView(theme ui.Theme) *PartitionsView {
	return &PartitionsView{Theme: theme}
}

// Draw renders a single-line "Partitions retired" placeholder.
func (v *PartitionsView) Draw(screen *ui.Screen, bounds ui.Rect) {
	if v == nil {
		return
	}
	screen.FillRect(bounds.X, bounds.Y, bounds.Width, bounds.Height, v.Theme.BaseStyle())
	screen.DrawText(bounds.X+1, bounds.Y, bounds.Width-2, " PARTITIONS ", v.Theme.SubtleStyle().Bold(true))
	if bounds.Height >= 3 {
		screen.DrawText(bounds.X+2, bounds.Y+2, bounds.Width-3,
			"Partition manager retired (#56).",
			v.Theme.SubtleStyle())
	}
}

// FormOpen always returns false -- there's no form here anymore.
func (v *PartitionsView) FormOpen() bool { return false }

// HandleEvent swallows every key press; returns false (unhandled).
func (v *PartitionsView) HandleEvent(_ tcell.Event) bool { return false }

// SetPartitions is a no-op: nothing to display.
func (v *PartitionsView) SetPartitions(_ []PartitionInfo) {}

// Reset is a no-op: nothing to clear.
func (v *PartitionsView) Reset(_ string) {}

// SetSelected is a no-op.
func (v *PartitionsView) SetSelected(_ string) {}

// Selected always returns the empty string.
func (v *PartitionsView) Selected() string { return "" }

// Active always returns the empty string.
func (v *PartitionsView) Active() string { return "" }
