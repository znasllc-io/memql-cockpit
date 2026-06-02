package cluster

// Deploy-control modal for the Topology pane (#145). Builds on the
// read-only deployment-v2 status strip from #144: where #144 renders
// per-env Argo/Rollouts state, #145 adds the owner/admin write
// actions behind a key-bound modal -- DeployStaging, Promote,
// Rollback, and RolloutAction (promote|abort) -- each fired through
// the SDK DeployControlClient and surfaced with its AuditEventId.
//
// Gating. The 'D' key only opens the modal for owner/admin callers
// (canViewDeploymentsLocked, same gate the read strip uses); the
// hint is hidden otherwise. The actions are also owner/admin-gated
// server-side, so a non-admin who somehow fired one gets
// PermissionDenied, which we surface as "ERROR: requires owner/admin".
//
// Thread-safety. All modal state (v.ctrl) lives under the same v.mu
// the rest of the View uses. Key handling runs inside HandleEvent's
// write lock; the SDK call fires in a goroutine (so the event loop
// never blocks) and stores its result back under v.mu before asking
// for a repaint via OnRedraw. A successful action also triggers
// OnActionComplete so the status strip refreshes.

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/gdamore/tcell/v2"

	"github.com/znasllc-io/memql-cockpit/cli/ui"
	"github.com/znasllc-io/memql/sdk/go/client"
)

// deployActionTimeout bounds each SDK write action. Generous relative
// to the read poll (5s) because a deploy / promote / rollback kicks a
// pipeline server-side; 30s is enough for the synchronous ack without
// hanging the modal forever on a wedged backend.
const deployActionTimeout = 30 * time.Second

// deployActions is the slice of DeployControlClient the deploy-control
// modal fires against, expressed as an interface so tests can inject a
// fake without a live gRPC connection. The concrete
// *client.DeployControlClient satisfies it.
type deployActions interface {
	DeployStaging(ctx context.Context, version string) (client.ActionResult, error)
	Promote(ctx context.Context, version string) (client.ActionResult, error)
	Rollback(ctx context.Context, env, commitSha string) (client.ActionResult, error)
	RolloutAction(ctx context.Context, env, rollout, action string) (client.ActionResult, error)
}

// deployStage is the deploy-control modal's state-machine cursor.
type deployStage int

const (
	// Top-level action menu: pick one of the four actions.
	deployStageMenu deployStage = iota
	// Deploy-to-staging: type a version string.
	deployStageStagingVersion
	// Promote: type a version string, then type-to-confirm.
	deployStagePromoteVersion
	deployStagePromoteConfirm
	// Rollback: pick env, type commit sha, type-to-confirm.
	deployStageRollbackEnv
	deployStageRollbackSha
	deployStageRollbackConfirm
	// Rollout action: pick env, pick rollout, pick promote|abort,
	// then confirm (abort is destructive).
	deployStageRolloutEnv
	deployStageRolloutPick
	deployStageRolloutAction
	deployStageRolloutConfirm
	// Terminal: action fired, result line shown.
	deployStageResult
)

// deployMenuItem is one row of the top-level action menu.
type deployMenuItem struct {
	label string
	stage deployStage // the stage entering this action transitions to
}

// deployMenu is the fixed top-level menu. Order is least-destructive
// first so a blind Enter lands on the safest action (deploy staging).
var deployMenu = []deployMenuItem{
	{label: "Deploy to staging", stage: deployStageStagingVersion},
	{label: "Promote staging -> prod", stage: deployStagePromoteVersion},
	{label: "Rollback", stage: deployStageRollbackEnv},
	{label: "Rollout promote / abort", stage: deployStageRolloutAction},
}

// rolloutActions are the two RolloutAction verbs the picker offers.
var rolloutActions = []string{"promote", "abort"}

// deployControlState is the transient state of the open modal. selected
// indexes the active list (menu / env / rollout / action); version + sha
// + confirm are the typed-text buffers; result/resultOK hold the
// terminal outcome line.
type deployControlState struct {
	stage    deployStage
	selected int // index into whatever list the current stage shows

	version string // staging / promote version buffer
	sha     string // rollback commit-sha buffer
	confirm string // type-to-confirm buffer

	env         string   // chosen env for rollback / rollout
	rolloutName string   // chosen rollout name
	rolloutOpts []string // rollout names available for the chosen env (snapshot)
	action      string   // chosen rollout action (promote|abort)

	running  bool   // an SDK call is in flight
	result   string // terminal result line (SUCCESS: ... / ERROR: ...)
	resultOK bool
}

// openDeployModalLocked opens the deploy-control modal on the top-level
// menu. Caller MUST hold v.mu (write).
func (v *View) openDeployModalLocked() {
	v.ctrl = &deployControlState{stage: deployStageMenu}
}

// rolloutNamesForEnvLocked returns the rollout names known for env from
// the last-polled status, so the rollout picker can offer them instead
// of forcing the operator to type. Caller MUST hold v.mu.
func (v *View) rolloutNamesForEnvLocked(env string) []string {
	st, ok := v.deployStatus[env]
	if !ok {
		return nil
	}
	names := make([]string, 0, len(st.Rollouts))
	for _, r := range st.Rollouts {
		if n := strings.TrimSpace(r.Name); n != "" {
			names = append(names, n)
		}
	}
	return names
}

// handleDeployKeyLocked routes a key into the open modal. Caller MUST
// hold v.mu (write); this function returns with the lock still held
// (HandleEvent's deferred Unlock releases it). Async fires are launched
// AFTER computing what to do but the goroutine takes its own lock.
func (v *View) handleDeployKeyLocked(key *tcell.EventKey) bool {
	m := v.ctrl
	if m == nil {
		return false
	}
	// Esc steps back one stage (or closes from the menu / result).
	if key.Key() == tcell.KeyEscape {
		v.deployEscapeLocked()
		v.requestRedrawLocked()
		return true
	}
	// While an action is in flight every key is swallowed -- no input
	// races against the result write.
	if m.running {
		return true
	}
	switch m.stage {
	case deployStageMenu:
		return v.handleMenuKeyLocked(key)
	case deployStageStagingVersion:
		return v.handleTextKeyLocked(key, &m.version, func() {
			v.fireStagingLocked()
		})
	case deployStagePromoteVersion:
		return v.handleTextKeyLocked(key, &m.version, func() {
			if strings.TrimSpace(m.version) != "" {
				m.stage = deployStagePromoteConfirm
				m.confirm = ""
			}
		})
	case deployStagePromoteConfirm:
		return v.handleTextKeyLocked(key, &m.confirm, func() {
			if v.promoteConfirmedLocked() {
				v.firePromoteLocked()
			}
		})
	case deployStageRollbackEnv:
		return v.handleEnvPickKeyLocked(key, deployStageRollbackSha)
	case deployStageRollbackSha:
		return v.handleTextKeyLocked(key, &m.sha, func() {
			if strings.TrimSpace(m.sha) != "" {
				m.stage = deployStageRollbackConfirm
				m.confirm = ""
			}
		})
	case deployStageRollbackConfirm:
		return v.handleTextKeyLocked(key, &m.confirm, func() {
			if v.rollbackConfirmedLocked() {
				v.fireRollbackLocked()
			}
		})
	case deployStageRolloutEnv:
		return v.handleEnvPickKeyLocked(key, deployStageRolloutPick)
	case deployStageRolloutPick:
		return v.handleRolloutPickKeyLocked(key)
	case deployStageRolloutAction:
		// First env, then action, then (abort) confirm. We reuse the
		// env-pick stage as the entry point; this stage is the action
		// chooser reached after env is set.
		return v.handleRolloutActionKeyLocked(key)
	case deployStageRolloutConfirm:
		return v.handleTextKeyLocked(key, &m.confirm, func() {
			if v.rolloutAbortConfirmedLocked() {
				v.fireRolloutLocked()
			}
		})
	case deployStageResult:
		// Any key (other than Esc, handled above) closes the modal.
		if key.Key() == tcell.KeyEnter {
			v.ctrl = nil
			v.requestRedrawLocked()
			return true
		}
		return true
	}
	return false
}

// deployEscapeLocked steps the modal back one stage, or closes it from
// the menu / result terminal. Caller holds v.mu.
func (v *View) deployEscapeLocked() {
	m := v.ctrl
	switch m.stage {
	case deployStageMenu, deployStageResult:
		v.ctrl = nil
	case deployStageStagingVersion, deployStagePromoteVersion,
		deployStageRollbackEnv, deployStageRolloutEnv:
		// Back to the menu.
		m.stage = deployStageMenu
		m.selected = 0
		m.version, m.sha, m.confirm = "", "", ""
	case deployStagePromoteConfirm:
		m.stage = deployStagePromoteVersion
		m.confirm = ""
	case deployStageRollbackSha:
		m.stage = deployStageRollbackEnv
	case deployStageRollbackConfirm:
		m.stage = deployStageRollbackSha
		m.confirm = ""
	case deployStageRolloutPick:
		m.stage = deployStageRolloutEnv
	case deployStageRolloutAction:
		m.stage = deployStageRolloutPick
	case deployStageRolloutConfirm:
		m.stage = deployStageRolloutAction
		m.confirm = ""
	}
}

// handleMenuKeyLocked drives the top-level action menu.
func (v *View) handleMenuKeyLocked(key *tcell.EventKey) bool {
	m := v.ctrl
	switch key.Key() {
	case tcell.KeyUp:
		if m.selected > 0 {
			m.selected--
		}
		v.requestRedrawLocked()
		return true
	case tcell.KeyDown:
		if m.selected < len(deployMenu)-1 {
			m.selected++
		}
		v.requestRedrawLocked()
		return true
	case tcell.KeyEnter:
		item := deployMenu[m.selected]
		v.enterMenuItemLocked(item)
		v.requestRedrawLocked()
		return true
	}
	return false
}

// enterMenuItemLocked transitions into the chosen action, seeding any
// per-action state. Caller holds v.mu.
func (v *View) enterMenuItemLocked(item deployMenuItem) {
	m := v.ctrl
	m.version, m.sha, m.confirm = "", "", ""
	m.env, m.rolloutName, m.action = "", "", ""
	m.selected = 0
	switch item.stage {
	case deployStageRolloutAction:
		// Rollout action starts at env selection; the action chooser
		// stage is reached after env + rollout are set.
		m.stage = deployStageRolloutEnv
	default:
		m.stage = item.stage
	}
}

// handleTextKeyLocked accumulates printable runes into buf and runs
// onEnter when Enter is pressed. Backspace deletes the last rune.
// Mirrors the add-cluster form's rune-buffer idiom. Caller holds v.mu.
func (v *View) handleTextKeyLocked(key *tcell.EventKey, buf *string, onEnter func()) bool {
	switch key.Key() {
	case tcell.KeyEnter:
		onEnter()
		v.requestRedrawLocked()
		return true
	case tcell.KeyBackspace, tcell.KeyBackspace2:
		if len(*buf) > 0 {
			// Trim one rune (not one byte) so multibyte input is safe.
			r := []rune(*buf)
			*buf = string(r[:len(r)-1])
		}
		v.requestRedrawLocked()
		return true
	case tcell.KeyRune:
		r := key.Rune()
		// Accept any printable, drop control / DEL.
		if r >= 0x20 && r != 0x7f && len([]rune(*buf)) < 200 {
			*buf += string(r)
		}
		v.requestRedrawLocked()
		return true
	}
	return false
}

// handleEnvPickKeyLocked drives an env (staging/prod) picker, advancing
// to next on Enter. Caller holds v.mu.
func (v *View) handleEnvPickKeyLocked(key *tcell.EventKey, next deployStage) bool {
	m := v.ctrl
	switch key.Key() {
	case tcell.KeyUp:
		if m.selected > 0 {
			m.selected--
		}
		v.requestRedrawLocked()
		return true
	case tcell.KeyDown:
		if m.selected < len(deployEnvs)-1 {
			m.selected++
		}
		v.requestRedrawLocked()
		return true
	case tcell.KeyEnter:
		m.env = deployEnvs[m.selected]
		m.selected = 0
		if next == deployStageRolloutPick {
			// Snapshot the available rollout names for this env so the
			// picker can offer them; empty -> the operator types one.
			m.rolloutOpts = v.rolloutNamesForEnvLocked(m.env)
		}
		m.stage = next
		v.requestRedrawLocked()
		return true
	}
	return false
}

// handleRolloutPickKeyLocked picks a rollout from the env's known names,
// or accepts a typed name when none are known. Caller holds v.mu.
func (v *View) handleRolloutPickKeyLocked(key *tcell.EventKey) bool {
	m := v.ctrl
	if len(m.rolloutOpts) == 0 {
		// No known rollouts -- fall back to typed input. Enter advances
		// to the action chooser once a name is entered.
		return v.handleTextKeyLocked(key, &m.rolloutName, func() {
			if strings.TrimSpace(m.rolloutName) != "" {
				m.stage = deployStageRolloutAction
				m.selected = 0
			}
		})
	}
	switch key.Key() {
	case tcell.KeyUp:
		if m.selected > 0 {
			m.selected--
		}
		v.requestRedrawLocked()
		return true
	case tcell.KeyDown:
		if m.selected < len(m.rolloutOpts)-1 {
			m.selected++
		}
		v.requestRedrawLocked()
		return true
	case tcell.KeyEnter:
		m.rolloutName = m.rolloutOpts[m.selected]
		m.selected = 0
		m.stage = deployStageRolloutAction
		v.requestRedrawLocked()
		return true
	}
	return false
}

// handleRolloutActionKeyLocked picks promote|abort. promote fires
// immediately; abort is destructive and advances to a type-to-confirm.
// Caller holds v.mu.
func (v *View) handleRolloutActionKeyLocked(key *tcell.EventKey) bool {
	m := v.ctrl
	switch key.Key() {
	case tcell.KeyUp:
		if m.selected > 0 {
			m.selected--
		}
		v.requestRedrawLocked()
		return true
	case tcell.KeyDown:
		if m.selected < len(rolloutActions)-1 {
			m.selected++
		}
		v.requestRedrawLocked()
		return true
	case tcell.KeyEnter:
		m.action = rolloutActions[m.selected]
		if m.action == "abort" {
			m.stage = deployStageRolloutConfirm
			m.confirm = ""
			v.requestRedrawLocked()
			return true
		}
		v.fireRolloutLocked()
		v.requestRedrawLocked()
		return true
	}
	return false
}

// promoteConfirmedLocked reports whether the type-to-confirm buffer
// matches an accepted confirmation phrase for a prod promote: either
// the version being promoted or the literal "promote prod".
func (v *View) promoteConfirmedLocked() bool {
	m := v.ctrl
	c := strings.TrimSpace(m.confirm)
	return c != "" && (c == strings.TrimSpace(m.version) || strings.EqualFold(c, "promote prod"))
}

// rollbackConfirmedLocked reports whether the confirm buffer matches the
// commit sha being rolled back or the literal "rollback <env>".
func (v *View) rollbackConfirmedLocked() bool {
	m := v.ctrl
	c := strings.TrimSpace(m.confirm)
	return c != "" && (c == strings.TrimSpace(m.sha) || strings.EqualFold(c, "rollback "+m.env))
}

// rolloutAbortConfirmedLocked reports whether the confirm buffer matches
// the rollout name being aborted or the literal "abort <rollout>".
func (v *View) rolloutAbortConfirmedLocked() bool {
	m := v.ctrl
	c := strings.TrimSpace(m.confirm)
	return c != "" && (c == strings.TrimSpace(m.rolloutName) || strings.EqualFold(c, "abort "+m.rolloutName))
}

// -----------------------------------------------------------------------------
// Action firing
// -----------------------------------------------------------------------------

// resolveActorLocked returns the action executor: the injected fake if
// set, else a fresh DeployControlClient from DeployClient(). nil when
// no cluster is connected. Caller holds v.mu.
func (v *View) resolveActorLocked() deployActions {
	if v.deployActor != nil {
		return v.deployActor
	}
	if v.DeployClient == nil {
		return nil
	}
	if dc := v.DeployClient(); dc != nil {
		return dc
	}
	return nil
}

// runAction is the common async-fire wrapper. It marks the modal
// running, launches fn in a goroutine with a bounded context, stores
// the ActionResult / error as a terminal result line, and asks for a
// repaint. A successful action also triggers OnActionComplete so the
// status strip refreshes. Caller holds v.mu.
func (v *View) runAction(fn func(ctx context.Context, a deployActions) (client.ActionResult, error)) {
	m := v.ctrl
	actor := v.resolveActorLocked()
	if actor == nil {
		m.stage = deployStageResult
		m.result = "ERROR: no connected cluster"
		m.resultOK = false
		v.requestRedrawLocked()
		return
	}
	m.running = true
	m.stage = deployStageResult
	m.result = "Working..."
	v.requestRedrawLocked()

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), deployActionTimeout)
		defer cancel()
		res, err := fn(ctx, actor)
		line, ok := formatActionOutcome(res, err)

		v.mu.Lock()
		// The modal could have been closed (Esc) mid-flight; only store
		// if it's still the same open modal in the result stage.
		if v.ctrl == m {
			m.running = false
			m.result = line
			m.resultOK = ok
		}
		onComplete := v.OnActionComplete
		v.mu.Unlock()

		if ok && onComplete != nil {
			onComplete()
		}
		v.requestRedraw()
	}()
}

func (v *View) fireStagingLocked() {
	ver := strings.TrimSpace(v.ctrl.version)
	if ver == "" {
		return // Enter on an empty version is a no-op; stay on the input.
	}
	v.runAction(func(ctx context.Context, a deployActions) (client.ActionResult, error) {
		return a.DeployStaging(ctx, ver)
	})
}

func (v *View) firePromoteLocked() {
	ver := strings.TrimSpace(v.ctrl.version)
	v.runAction(func(ctx context.Context, a deployActions) (client.ActionResult, error) {
		return a.Promote(ctx, ver)
	})
}

func (v *View) fireRollbackLocked() {
	env := v.ctrl.env
	sha := strings.TrimSpace(v.ctrl.sha)
	v.runAction(func(ctx context.Context, a deployActions) (client.ActionResult, error) {
		return a.Rollback(ctx, env, sha)
	})
}

func (v *View) fireRolloutLocked() {
	env := v.ctrl.env
	name := strings.TrimSpace(v.ctrl.rolloutName)
	action := v.ctrl.action
	v.runAction(func(ctx context.Context, a deployActions) (client.ActionResult, error) {
		return a.RolloutAction(ctx, env, name, action)
	})
}

// formatActionOutcome renders the terminal result line. On a transport
// error it surfaces the message, mapping a PermissionDenied to the
// owner/admin hint. On success it shows the ActionResult message plus
// the AuditEventId so the operator has the audit handle. Returns the
// line and whether it was a success.
func formatActionOutcome(res client.ActionResult, err error) (string, bool) {
	if err != nil {
		if isPermissionDenied(err) {
			return "ERROR: requires owner/admin", false
		}
		return "ERROR: " + err.Error(), false
	}
	if !res.OK {
		msg := res.Message
		if msg == "" {
			msg = "action rejected"
		}
		return "ERROR: " + msg, false
	}
	msg := res.Message
	if msg == "" {
		msg = "ok"
	}
	if res.AuditEventID != "" {
		return fmt.Sprintf("SUCCESS: %s (audit %s)", msg, res.AuditEventID), true
	}
	return "SUCCESS: " + msg, true
}

// isPermissionDenied reports whether err is (or wraps) a gRPC
// PermissionDenied. The SDK wraps the status error with %w so the gRPC
// status is still recoverable; we also fall back to a substring match
// for robustness against any non-status wrapper.
func isPermissionDenied(err error) bool {
	if err == nil {
		return false
	}
	if st, ok := grpcStatusFromError(err); ok && st == permissionDeniedCode {
		return true
	}
	return strings.Contains(strings.ToLower(err.Error()), "permissiondenied") ||
		strings.Contains(err.Error(), "PermissionDenied")
}

// requestRedrawLocked / requestRedraw ask the app to repaint. The
// *Locked variant is safe to call while holding v.mu (it only reads the
// callback pointer, which is set once at wire time); requestRedraw takes
// the read lock for use from the async goroutine.
func (v *View) requestRedrawLocked() {
	if v.OnRedraw != nil {
		v.OnRedraw()
	}
}

func (v *View) requestRedraw() {
	v.mu.RLock()
	cb := v.OnRedraw
	v.mu.RUnlock()
	if cb != nil {
		cb()
	}
}

// -----------------------------------------------------------------------------
// Rendering
// -----------------------------------------------------------------------------

// drawDeployModal renders the deploy-control modal centered over the
// pane. Caller holds v.mu.RLock (Draw frame).
func (v *View) drawDeployModal(screen *ui.Screen, bounds ui.Rect) {
	m := v.ctrl
	if m == nil {
		return
	}
	modalW := 60
	modalH := 14
	if modalW > bounds.Width-4 {
		modalW = bounds.Width - 4
	}
	if modalH > bounds.Height-4 {
		modalH = bounds.Height - 4
	}
	if modalW < 10 || modalH < 6 {
		return // pane too small to host the modal
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
	screen.DrawText(x+2, y, modalW-4, " DEPLOY CONTROLS ", accent.Bold(true))

	bodyX := x + 2
	bodyY := y + 2
	bodyW := modalW - 4
	v.drawDeployBody(screen, bodyX, bodyY, bodyW, modalH-4, base, subtle)

	// Hint row anchored to the modal's last interior row.
	hint := v.deployModalHint()
	screen.DrawText(bodyX, y+modalH-2, bodyW, hint, subtle)
}

// drawDeployBody paints the stage-specific body. Caller holds RLock.
func (v *View) drawDeployBody(screen *ui.Screen, x, y, w, h int, base, subtle tcell.Style) {
	m := v.ctrl
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
	drawInput := func(prompt, buf string) {
		screen.DrawText(x, y, w, prompt, subtle)
		screen.DrawText(x, y+2, w, "> "+buf+"_", base.Bold(true))
	}

	switch m.stage {
	case deployStageMenu:
		labels := make([]string, len(deployMenu))
		for i, it := range deployMenu {
			labels[i] = it.label
		}
		drawList("Select an action:", labels, m.selected)
	case deployStageStagingVersion:
		drawInput("Deploy to STAGING -- type version, Enter to deploy:", m.version)
	case deployStagePromoteVersion:
		drawInput("Promote STAGING -> PROD -- type version:", m.version)
	case deployStagePromoteConfirm:
		screen.DrawText(x, y, w, "Confirm PROMOTE to prod (destructive).", subtle)
		screen.DrawText(x, y+1, w, fmt.Sprintf("Type %q or \"promote prod\":", strings.TrimSpace(m.version)), subtle)
		screen.DrawText(x, y+3, w, "> "+m.confirm+"_", base.Bold(true))
	case deployStageRollbackEnv:
		drawList("Rollback -- pick env:", deployEnvsCopy(), m.selected)
	case deployStageRollbackSha:
		drawInput(fmt.Sprintf("Rollback %s -- type commit SHA:", strings.ToUpper(m.env)), m.sha)
	case deployStageRollbackConfirm:
		screen.DrawText(x, y, w, fmt.Sprintf("Confirm ROLLBACK %s (destructive).", strings.ToUpper(m.env)), subtle)
		screen.DrawText(x, y+1, w, fmt.Sprintf("Type the SHA or \"rollback %s\":", m.env), subtle)
		screen.DrawText(x, y+3, w, "> "+m.confirm+"_", base.Bold(true))
	case deployStageRolloutEnv:
		drawList("Rollout action -- pick env:", deployEnvsCopy(), m.selected)
	case deployStageRolloutPick:
		if len(m.rolloutOpts) == 0 {
			drawInput(fmt.Sprintf("Rollout (%s) -- type rollout name:", strings.ToUpper(m.env)), m.rolloutName)
		} else {
			drawList(fmt.Sprintf("Rollout (%s) -- pick rollout:", strings.ToUpper(m.env)), m.rolloutOpts, m.selected)
		}
	case deployStageRolloutAction:
		drawList(fmt.Sprintf("Rollout %s/%s -- pick action:", strings.ToUpper(m.env), m.rolloutName), rolloutActions, m.selected)
	case deployStageRolloutConfirm:
		screen.DrawText(x, y, w, "Confirm ABORT rollout (destructive).", subtle)
		screen.DrawText(x, y+1, w, fmt.Sprintf("Type %q or \"abort %s\":", strings.TrimSpace(m.rolloutName), m.rolloutName), subtle)
		screen.DrawText(x, y+3, w, "> "+m.confirm+"_", base.Bold(true))
	case deployStageResult:
		style := base.Bold(true)
		if !m.resultOK && !m.running {
			style = tcell.StyleDefault.Foreground(v.Theme.Error).Background(v.Theme.BG)
		} else if m.resultOK {
			style = tcell.StyleDefault.Foreground(v.Theme.Success).Background(v.Theme.BG)
		}
		// Wrap the result line over the available rows.
		v.drawWrapped(screen, x, y, w, h, m.result, style)
	}
}

// drawWrapped writes text wrapped to w over up to h rows.
func (v *View) drawWrapped(screen *ui.Screen, x, y, w, h int, text string, style tcell.Style) {
	r := []rune(text)
	row := 0
	for len(r) > 0 && row < h {
		n := w
		if n > len(r) {
			n = len(r)
		}
		screen.DrawText(x, y+row, w, string(r[:n]), style)
		r = r[n:]
		row++
	}
}

// deployEnvsCopy returns the env list as a []string for list rendering.
func deployEnvsCopy() []string {
	out := make([]string, len(deployEnvs))
	copy(out, deployEnvs)
	return out
}

// deployModalHint returns the context-aware bottom hint for the open
// modal. Caller holds RLock.
func (v *View) deployModalHint() string {
	m := v.ctrl
	switch m.stage {
	case deployStageMenu:
		return "Up/Down:Pick  Enter:Select  Esc:Close"
	case deployStageRollbackEnv, deployStageRolloutEnv, deployStageRolloutAction:
		return "Up/Down:Pick  Enter:Select  Esc:Back"
	case deployStageRolloutPick:
		if len(m.rolloutOpts) == 0 {
			return "Type name  Enter:Next  Esc:Back"
		}
		return "Up/Down:Pick  Enter:Select  Esc:Back"
	case deployStagePromoteConfirm, deployStageRollbackConfirm, deployStageRolloutConfirm:
		return "Type to confirm  Enter:Execute  Esc:Back"
	case deployStageResult:
		if m.running {
			return "Working..."
		}
		return "Enter:Close  Esc:Close"
	default:
		return "Type  Enter:Next  Esc:Back"
	}
}
