package cluster

// Apply / cut-from-composition for the topology-as-canvas builder
// (memql-cockpit#253, PHASE 2).
//
// The composeBuilder is a DESIRED per-tier topology (node type x replicas x
// optional version). "Apply" cuts a new version for the cluster's env and
// persists the composed per-tier specs against the new deployment. The two
// SDK calls it spans -- DeployControlClient.CutVersion + the typed
// CreateDeploymentNodeSpec mutation -- live in app.go (the SDK-only rule
// keeps wire calls out of cli/cluster), so the View invokes them through the
// ApplyComposition callback and only owns the confirm + result UX here.
//
// The flow is a tiny inline state machine rendered in the builder region (no
// separate modal): Enter opens a confirm, 'Y' fires it async, the result
// lands in place, any key returns to the builder.

import (
	"fmt"

	"github.com/gdamore/tcell/v2"

	"github.com/znasllc-io/memql-cockpit/cli/ui"
)

type composeApplyStage int

const (
	caConfirm composeApplyStage = iota
	caRunning
	caResult
)

// composeApplyState is the inline apply/cut-from-composition flow state.
type composeApplyState struct {
	stage  composeApplyStage
	env    string
	specs  []NodeSpecInfo
	result string
	ok     bool
}

// openComposeApplyLocked starts the apply confirm for the current
// composition. No-op when the builder is empty or no apply callback is wired.
// Caller holds v.mu (write).
func (v *View) openComposeApplyLocked() {
	if v.builder == nil || v.builder.empty() || v.ApplyComposition == nil {
		return
	}
	v.compApply = &composeApplyState{
		stage: caConfirm,
		env:   v.cutEnvLocked(),
		specs: v.builder.toSpecs(),
	}
	v.requestRedrawLocked()
}

// handleComposeApplyKeyLocked routes a key while the apply confirm/result is
// shown. Caller holds v.mu (write). Returns true (it owns the pane).
func (v *View) handleComposeApplyKeyLocked(key *tcell.EventKey) bool {
	m := v.compApply
	switch m.stage {
	case caConfirm:
		switch key.Key() {
		case tcell.KeyEscape:
			v.compApply = nil
		case tcell.KeyRune:
			switch key.Rune() {
			case 'y', 'Y':
				v.runComposeApplyLocked()
			case 'n', 'N':
				v.compApply = nil
			}
		}
	case caRunning:
		// Ignore input while the action is in flight.
	case caResult:
		// Any key dismisses the result and returns to the builder.
		v.compApply = nil
	}
	v.requestRedrawLocked()
	return true
}

// runComposeApplyLocked fires the apply callback off-thread and lands the
// result back on compApply. Caller holds v.mu (write).
func (v *View) runComposeApplyLocked() {
	m := v.compApply
	apply := v.ApplyComposition
	if apply == nil {
		m.stage = caResult
		m.result = "ERROR: no connected cluster"
		m.ok = false
		return
	}
	m.stage = caRunning
	m.result = "Working..."
	env := m.env
	specs := m.specs
	v.requestRedrawLocked()

	go func() {
		line, ok := apply(env, specs)

		v.mu.Lock()
		if v.compApply == m { // could have been dismissed mid-flight
			m.stage = caResult
			m.result = line
			m.ok = ok
		}
		onChanged := v.OnDeploymentsChanged
		v.mu.Unlock()

		if ok && onChanged != nil {
			onChanged()
		}
		v.requestRedraw()
	}()
}

// drawComposeApply renders the apply confirm/running/result inline in the
// builder region. Caller holds v.mu (RLock during Draw).
func (v *View) drawComposeApply(screen *ui.Screen, region ui.Rect) {
	m := v.compApply
	titleStyle := tcell.StyleDefault.Foreground(v.Theme.Accent).Background(v.Theme.BG).Bold(true)
	base := v.Theme.BaseStyle()
	x := region.X + 2
	w := region.Width - 4
	if w <= 0 {
		return
	}
	y := region.Y + 1
	screen.DrawText(x, y, w, "CUT FROM COMPOSITION", titleStyle)
	y += 2

	switch m.stage {
	case caConfirm:
		msg := fmt.Sprintf("Cut a new version to %q and apply %d tier(s)?", m.env, len(m.specs))
		screen.DrawText(x, y, w, clipText(msg, w), base)
		y++
		screen.DrawText(x, y, w, "This creates a pending deployment and writes the per-tier specs.", v.Theme.SubtleStyle())
	case caRunning:
		screen.DrawText(x, y, w, "Working...", base)
	case caResult:
		style := base.Foreground(v.Theme.Success)
		if !m.ok {
			style = base.Foreground(v.Theme.Error)
		}
		for _, ln := range wrapLines(m.result, w) {
			if y >= region.Y+region.Height-1 {
				break
			}
			screen.DrawText(x, y, w, ln, style)
			y++
		}
	}
}

// composeApplyHint returns the chrome hint for the active apply stage.
func (v *View) composeApplyHint() string {
	switch v.compApply.stage {
	case caConfirm:
		return "Y:Cut  N/Esc:Cancel"
	case caRunning:
		return "Working..."
	default:
		return "Any:Back"
	}
}

// wrapLines splits s into lines no wider than w cells (cheap word-agnostic
// hard wrap; the result lines are short status strings).
func wrapLines(s string, w int) []string {
	if w <= 0 {
		return nil
	}
	var out []string
	for len(s) > w {
		out = append(out, s[:w])
		s = s[w:]
	}
	out = append(out, s)
	return out
}
