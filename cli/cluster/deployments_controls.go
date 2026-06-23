package cluster

// Cut/deploy/rollback modal for the Deployments section (memql-cockpit
// #207). Concept-driven: it fires the deployment-concept lifecycle
// wrappers landed in memql#1886 (this is the single deployment surface
// now -- the older deployment-v2 overlay modal was removed in #221):
//
//	CutVersion(env, bump, version)  -- create a pending deployment
//	Deploy(deploymentId)            -- ship a pending deployment
//	RollbackDeployment(toDeploymentId) -- re-promote a historical digest
//	SuggestNextVersion(env)         -- propose the next semver (read-only)
//
// Role gating (locked matrix, epic #1871): cut + deploy require
// developer/admin/owner; rollback is owner-only. The key handler in
// deployments.go only opens the modal for an authorized role, and the
// server re-checks -- a PermissionDenied surfaces as the owner/admin (or
// owner-only) hint via formatActionOutcome.
//
// Thread-safety: state lives in v.dctrl under v.mu (the same lock that
// serializes Draw against the per-cluster subscriber goroutine).
// Async fires run in a goroutine that re-acquires the lock to store the
// result, then triggers OnDeploymentsChanged so the history refreshes.

import (
	"context"
	"fmt"
	"strings"

	"github.com/gdamore/tcell/v2"

	"github.com/znasllc-io/memql-cockpit/cli/ui"
	"github.com/znasllc-io/memql/sdk/go/client"
)

// deployConceptActions is the slice of DeployControlClient the concept
// modal fires against, as an interface so tests can inject a fake. The
// concrete *client.DeployControlClient satisfies it.
type deployConceptActions interface {
	CutVersion(ctx context.Context, env, bump, version string) (client.ActionResult, error)
	Deploy(ctx context.Context, deploymentId string) (client.ActionResult, error)
	RollbackDeployment(ctx context.Context, toDeploymentId string) (client.ActionResult, error)
	SuggestNextVersion(ctx context.Context, env string) (client.NextVersionSuggestion, error)
}

// SetDeployConceptActor overrides the concept-modal action executor (for
// tests). Production leaves it nil; the modal resolves the real client
// from DeployClient() per-fire so a cluster switch retargets.
func (v *View) SetDeployConceptActor(a deployConceptActions) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.deployConceptActor = a
}

// deployConceptStage is the modal's state-machine cursor.
type deployConceptStage int

const (
	dcStageCutEnv          deployConceptStage = iota // pick env
	dcStageCutBump                                   // pick bump (patch/minor/major)
	dcStageCutConfirm                                // confirm cut
	dcStageDeployConfirm                             // confirm deploy of selected pending deployment
	dcStageRollbackConfirm                           // type-to-confirm rollback to selected
	dcStageResult                                    // terminal: result line shown
)

// cutEnvs / cutBumps are the pickers the cut flow offers. Least-impactful
// first so a blind Enter lands on staging + patch.
var cutEnvs = []string{"staging", "production", "development"}
var cutBumps = []string{"patch", "minor", "major"}

// deployConceptState is the transient modal state.
type deployConceptState struct {
	stage    deployConceptStage
	action   string // "cut" | "deploy" | "rollback"
	selected int    // index into the active picker (env/bump)

	env  string // chosen env (cut)
	bump string // chosen bump (cut)

	targetID  string // deploymentId for deploy/rollback
	targetVer string // version label of the target, for the confirm prompt
	confirm   string // type-to-confirm buffer (rollback)

	suggestion string // SuggestNextVersion summary, shown on the bump picker

	running  bool
	result   string
	resultOK bool
}

// openCutModalLocked opens the cut-version flow at the env picker. Caller
// MUST hold v.mu (write); the caller already gated on canCutDeployLocked.
func (v *View) openCutModalLocked() {
	v.dctrl = &deployConceptState{stage: dcStageCutEnv, action: "cut"}
}

// openDeployModalConceptLocked opens the deploy-confirm for the selected
// pending deployment. Caller MUST hold v.mu and have verified the
// selection is pending + the role may deploy.
func (v *View) openDeployModalConceptLocked() {
	d := v.deployments[v.deploySelected]
	v.dctrl = &deployConceptState{
		stage:     dcStageDeployConfirm,
		action:    "deploy",
		targetID:  d.ID,
		targetVer: d.Version,
	}
}

// openRollbackModalLocked opens the type-to-confirm rollback for the
// selected (succeeded) deployment. Caller MUST hold v.mu and have verified
// the role is owner + the selection reached succeeded.
func (v *View) openRollbackModalLocked() {
	d := v.deployments[v.deploySelected]
	v.dctrl = &deployConceptState{
		stage:     dcStageRollbackConfirm,
		action:    "rollback",
		targetID:  d.ID,
		targetVer: d.Version,
	}
}

// handleDeployConceptKeyLocked routes a key into the open concept modal.
// Caller MUST hold v.mu (write).
func (v *View) handleDeployConceptKeyLocked(key *tcell.EventKey) bool {
	m := v.dctrl
	if m == nil {
		return false
	}
	if key.Key() == tcell.KeyEscape {
		v.deployConceptEscapeLocked()
		v.requestRedrawLocked()
		return true
	}
	if m.running {
		return true // swallow input while a fire is in flight
	}
	switch m.stage {
	case dcStageCutEnv:
		return v.handleConceptPickLocked(key, cutEnvs, func() {
			m.env = cutEnvs[m.selected]
			m.selected = 0
			m.stage = dcStageCutBump
			v.fireSuggestLocked() // best-effort next-version hint
		})
	case dcStageCutBump:
		return v.handleConceptPickLocked(key, cutBumps, func() {
			m.bump = cutBumps[m.selected]
			m.selected = 0
			m.stage = dcStageCutConfirm
		})
	case dcStageCutConfirm:
		if key.Key() == tcell.KeyEnter {
			v.fireCutLocked()
			v.requestRedrawLocked()
			return true
		}
		return true
	case dcStageDeployConfirm:
		if key.Key() == tcell.KeyEnter {
			v.fireDeployLocked()
			v.requestRedrawLocked()
			return true
		}
		return true
	case dcStageRollbackConfirm:
		return v.handleConceptTextLocked(key, &m.confirm, func() {
			if v.rollbackConceptConfirmedLocked() {
				v.fireRollbackConceptLocked()
			}
		})
	case dcStageResult:
		if key.Key() == tcell.KeyEnter {
			v.dctrl = nil
			v.requestRedrawLocked()
		}
		return true
	}
	return false
}

// deployConceptEscapeLocked steps the modal back one stage or closes it.
func (v *View) deployConceptEscapeLocked() {
	m := v.dctrl
	switch m.stage {
	case dcStageCutBump:
		m.stage = dcStageCutEnv
		m.selected = 0
	case dcStageCutConfirm:
		m.stage = dcStageCutBump
		m.selected = 0
	default:
		// Env picker, deploy/rollback confirm, and the result stage all
		// close the modal outright.
		v.dctrl = nil
	}
}

// handleConceptPickLocked drives an Up/Down/Enter picker over items,
// running onEnter when Enter is pressed. Caller holds v.mu.
func (v *View) handleConceptPickLocked(key *tcell.EventKey, items []string, onEnter func()) bool {
	m := v.dctrl
	switch key.Key() {
	case tcell.KeyUp:
		if m.selected > 0 {
			m.selected--
		}
		v.requestRedrawLocked()
		return true
	case tcell.KeyDown:
		if m.selected < len(items)-1 {
			m.selected++
		}
		v.requestRedrawLocked()
		return true
	case tcell.KeyEnter:
		onEnter()
		v.requestRedrawLocked()
		return true
	}
	return false
}

// handleConceptTextLocked accumulates printable runes into buf and runs
// onEnter on Enter. Mirrors handleTextKeyLocked. Caller holds v.mu.
func (v *View) handleConceptTextLocked(key *tcell.EventKey, buf *string, onEnter func()) bool {
	switch key.Key() {
	case tcell.KeyEnter:
		onEnter()
		v.requestRedrawLocked()
		return true
	case tcell.KeyBackspace, tcell.KeyBackspace2:
		if len(*buf) > 0 {
			r := []rune(*buf)
			*buf = string(r[:len(r)-1])
		}
		v.requestRedrawLocked()
		return true
	case tcell.KeyRune:
		r := key.Rune()
		if r >= 0x20 && r != 0x7f && len([]rune(*buf)) < 200 {
			*buf += string(r)
		}
		v.requestRedrawLocked()
		return true
	}
	return false
}

// rollbackConceptConfirmedLocked reports whether the confirm buffer is the
// accepted phrase: the target version, the short deploymentId, or the
// literal "rollback". Caller holds v.mu.
func (v *View) rollbackConceptConfirmedLocked() bool {
	m := v.dctrl
	c := strings.TrimSpace(m.confirm)
	if c == "" {
		return false
	}
	return strings.EqualFold(c, "rollback") ||
		c == strings.TrimSpace(m.targetVer) ||
		c == shortID(m.targetID) ||
		c == strings.TrimSpace(m.targetID)
}

// -----------------------------------------------------------------------------
// Action firing
// -----------------------------------------------------------------------------

// resolveConceptActorLocked returns the executor: the injected fake if
// set, else a fresh DeployControlClient from DeployClient(). Caller holds
// v.mu.
func (v *View) resolveConceptActorLocked() deployConceptActions {
	if v.deployConceptActor != nil {
		return v.deployConceptActor
	}
	if v.DeployClient == nil {
		return nil
	}
	if dc := v.DeployClient(); dc != nil {
		return dc
	}
	return nil
}

// runConceptAction is the common async-fire wrapper for the concept modal,
// mirroring runAction. Caller holds v.mu.
func (v *View) runConceptAction(fn func(ctx context.Context, a deployConceptActions) (client.ActionResult, error)) {
	m := v.dctrl
	actor := v.resolveConceptActorLocked()
	if actor == nil {
		m.stage = dcStageResult
		m.result = "ERROR: no connected cluster"
		m.resultOK = false
		v.requestRedrawLocked()
		return
	}
	m.running = true
	m.stage = dcStageResult
	m.result = "Working..."
	v.requestRedrawLocked()

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), deployActionTimeout)
		defer cancel()
		res, err := fn(ctx, actor)
		line, ok := formatActionOutcome(res, err)

		v.mu.Lock()
		if v.dctrl == m { // modal could have been closed mid-flight
			m.running = false
			m.result = line
			m.resultOK = ok
		}
		onChanged := v.OnDeploymentsChanged
		v.mu.Unlock()

		if ok && onChanged != nil {
			onChanged()
		}
		v.requestRedraw()
	}()
}

// fireSuggestLocked fetches SuggestNextVersion for the chosen env and
// stores a one-line summary on the modal (best-effort: an error just
// leaves the hint blank). Caller holds v.mu.
func (v *View) fireSuggestLocked() {
	m := v.dctrl
	env := m.env
	actor := v.resolveConceptActorLocked()
	if actor == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), deployActionTimeout)
		defer cancel()
		s, err := actor.SuggestNextVersion(ctx, env)
		if err != nil {
			return
		}
		summary := fmt.Sprintf("current=%s  next: patch=%s minor=%s major=%s",
			orDash(s.CurrentVersion), orDash(s.NextPatch), orDash(s.NextMinor), orDash(s.NextMajor))
		v.mu.Lock()
		if v.dctrl == m {
			m.suggestion = summary
		}
		v.mu.Unlock()
		v.requestRedraw()
	}()
}

func (v *View) fireCutLocked() {
	m := v.dctrl
	env, bump := m.env, m.bump
	v.runConceptAction(func(ctx context.Context, a deployConceptActions) (client.ActionResult, error) {
		// Empty explicit version: the server computes the next semver from
		// the bump. SuggestNextVersion already previewed it.
		return a.CutVersion(ctx, env, bump, "")
	})
}

func (v *View) fireDeployLocked() {
	id := v.dctrl.targetID
	v.runConceptAction(func(ctx context.Context, a deployConceptActions) (client.ActionResult, error) {
		return a.Deploy(ctx, id)
	})
}

func (v *View) fireRollbackConceptLocked() {
	id := v.dctrl.targetID
	v.runConceptAction(func(ctx context.Context, a deployConceptActions) (client.ActionResult, error) {
		return a.RollbackDeployment(ctx, id)
	})
}

// -----------------------------------------------------------------------------
// Rendering
// -----------------------------------------------------------------------------

// drawDeployConceptModal renders the cut/deploy/rollback modal centered
// over the pane. Caller holds v.mu.RLock (Draw frame).
func (v *View) drawDeployConceptModal(screen *ui.Screen, bounds ui.Rect) {
	m := v.dctrl
	if m == nil {
		return
	}
	modalW := 62
	modalH := 14
	if modalW > bounds.Width-4 {
		modalW = bounds.Width - 4
	}
	if modalH > bounds.Height-4 {
		modalH = bounds.Height - 4
	}
	if modalW < 10 || modalH < 6 {
		return
	}
	x := bounds.X + (bounds.Width-modalW)/2
	y := bounds.Y + (bounds.Height-modalH)/2
	if y < bounds.Y+1 {
		y = bounds.Y + 1
	}

	accent := tcell.StyleDefault.Foreground(v.Theme.Accent).Background(v.Theme.BG)
	base := v.Theme.BaseStyle()
	subtle := v.Theme.SubtleStyle()

	screen.FillRect(x, y, modalW, modalH, base)
	screen.DrawBox(x, y, modalW, modalH, accent)
	title := " CUT VERSION "
	switch m.action {
	case "deploy":
		title = " DEPLOY "
	case "rollback":
		title = " ROLLBACK DEPLOYMENT "
	}
	screen.DrawText(x+2, y, modalW-4, title, accent.Bold(true))

	bodyX := x + 2
	bodyY := y + 2
	bodyW := modalW - 4
	v.drawDeployConceptBody(screen, bodyX, bodyY, bodyW, modalH-4, base, subtle)

	screen.DrawText(bodyX, y+modalH-2, bodyW, v.deployConceptHint(), subtle)
}

// drawDeployConceptBody paints the stage-specific body. Caller holds RLock.
func (v *View) drawDeployConceptBody(screen *ui.Screen, x, y, w, h int, base, subtle tcell.Style) {
	m := v.dctrl
	sel := v.Theme.SelectionStyle()

	drawList := func(title string, items []string, selected int) {
		screen.DrawText(x, y, w, title, subtle)
		for i, it := range items {
			ry := y + 1 + i
			if ry >= y+h {
				break
			}
			style := base
			marker := "  "
			if i == selected {
				style = sel
				marker = "> "
			}
			screen.FillRect(x, ry, w, 1, style)
			screen.DrawText(x, ry, w, marker+it, style)
		}
	}

	switch m.stage {
	case dcStageCutEnv:
		drawList("Cut a new version -- pick environment:", cutEnvs, m.selected)
	case dcStageCutBump:
		drawList(fmt.Sprintf("Cut for %s -- pick semver bump:", strings.ToUpper(m.env)), cutBumps, m.selected)
		if m.suggestion != "" {
			screen.DrawText(x, y+len(cutBumps)+2, w, m.suggestion, subtle)
		}
	case dcStageCutConfirm:
		screen.DrawText(x, y, w, fmt.Sprintf("Cut a %s version for %s?", m.bump, strings.ToUpper(m.env)), subtle)
		screen.DrawText(x, y+1, w, "This creates a pending deployment (does not ship it).", subtle)
		screen.DrawText(x, y+3, w, "Enter to cut, Esc to go back.", base.Bold(true))
	case dcStageDeployConfirm:
		screen.DrawText(x, y, w, fmt.Sprintf("Deploy pending deployment %s?", shortID(m.targetID)), subtle)
		if m.targetVer != "" {
			screen.DrawText(x, y+1, w, "version "+m.targetVer, subtle)
		}
		screen.DrawText(x, y+3, w, "Enter to ship, Esc to cancel.", base.Bold(true))
	case dcStageRollbackConfirm:
		screen.DrawText(x, y, w, fmt.Sprintf("Roll back to deployment %s (re-promote its digest).", shortID(m.targetID)), subtle)
		screen.DrawText(x, y+1, w, "Owner-only, destructive.", v.warningStyle())
		screen.DrawText(x, y+2, w, "Type the version, the id, or \"rollback\":", subtle)
		screen.DrawText(x, y+4, w, "> "+m.confirm+"_", base.Bold(true))
	case dcStageResult:
		style := base.Bold(true)
		if !m.resultOK && !m.running {
			style = tcell.StyleDefault.Foreground(v.Theme.Error).Background(v.Theme.BG)
		} else if m.resultOK {
			style = tcell.StyleDefault.Foreground(v.Theme.Success).Background(v.Theme.BG)
		}
		v.drawWrapped(screen, x, y, w, h, m.result, style)
	}
}

// deployConceptHint returns the modal's bottom hint line per stage.
func (v *View) deployConceptHint() string {
	m := v.dctrl
	switch m.stage {
	case dcStageCutEnv, dcStageCutBump:
		return "Up/Dn:Move  Enter:Next  Esc:Back"
	case dcStageCutConfirm, dcStageDeployConfirm:
		return "Enter:Confirm  Esc:Back"
	case dcStageRollbackConfirm:
		return "Type to confirm  Enter:Rollback  Esc:Cancel"
	case dcStageResult:
		if m.running {
			return "Working..."
		}
		return "Enter:Close  Esc:Close"
	}
	return "Esc:Close"
}
