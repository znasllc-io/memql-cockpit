package cluster

// Deployment-v2 status surface for the Topology pane (#144). Read-only:
// it renders the per-env GetDeploymentStatus result (deployed version +
// engineVersion + gate, Argo CD sync/health + drift, per-rollout
// blue-green / canary state + latest analysis, and the deploy-gate
// badge with leg summary). The polling + role-gating that feed it live
// in cli/app.go (refreshDeployStatus); the SDK client + result structs
// live in sdk/go/client/deploycontrol.go.
//
// Thread-safety: every function here is called from Draw with v.mu held
// (RLock). They only READ v.deployStatus / v.clusterRole; mutation goes
// through SetDeploymentStatus / SetClusterRole (Lock).

import (
	"fmt"
	"strings"

	"github.com/gdamore/tcell/v2"

	"github.com/znasllc-io/memql-cockpit/cli/ui"
	"github.com/znasllc-io/memql/sdk/go/client"
)

// deployStripHeight is the fixed number of rows reserved for the
// deployment-v2 strip, anchored just above the topology status bar.
// One header row + one row per env section (each env renders a compact
// multi-line block, but the band height is bounded so the topology grid
// above never gets squeezed unpredictably). When the role can't view,
// only a single muted line is drawn and the rest of the band stays blank.
const deployStripRows = 8

// drawDeployStrip renders the deployment-v2 status band. The band sits
// directly above the one-row status bar at the bottom of the pane. The
// caller (Draw) holds v.mu.RLock.
func (v *View) drawDeployStrip(screen *ui.Screen, bounds ui.Rect) {
	// Reserve a band of deployStripRows immediately above the status
	// bar (which lives on the last row). Clip to the pane so a tiny
	// terminal degrades gracefully rather than drawing over the title.
	bandTop := bounds.Y + bounds.Height - 1 - deployStripRows
	if bandTop <= bounds.Y+1 {
		return // pane too short to host the strip; topology already used the space
	}
	x := bounds.X + 1
	maxW := bounds.Width - 2
	if maxW <= 0 {
		return
	}

	headerStyle := tcell.StyleDefault.Foreground(v.Theme.Info).Background(v.Theme.BG).Bold(true)
	mutedStyle := v.Theme.SubtleStyle()

	// Separator above the band so it reads as its own section.
	sepStyle := mutedStyle
	for i := 0; i < maxW; i++ {
		screen.SetCell(x+i, bandTop, '─', sepStyle)
	}

	y := bandTop + 1
	screen.DrawText(x, y, maxW, "DEPLOYMENTS", headerStyle)
	y++

	// Role gate: GetDeploymentStatus is owner/admin-only server-side.
	// Non-admins get a single muted line, never the panel.
	if !v.canViewDeploymentsLocked() {
		screen.DrawText(x, y, maxW, "Deployments: owner/admin only", mutedStyle)
		return
	}

	bandBottom := bounds.Y + bounds.Height - 2 // last usable row before status bar
	for _, env := range deployEnvs {
		if y > bandBottom {
			break
		}
		st, ok := v.deployStatus[env]
		y = v.drawDeployEnv(screen, x, y, maxW, bandBottom, env, st, ok)
	}
}

// drawDeployEnv renders one env's section and returns the next free y.
func (v *View) drawDeployEnv(screen *ui.Screen, x, y, maxW, bandBottom int, env string, st client.DeploymentStatus, ok bool) int {
	labelStyle := tcell.StyleDefault.Foreground(v.Theme.FG).Background(v.Theme.BG).Bold(true)
	mutedStyle := v.Theme.SubtleStyle()

	label := strings.ToUpper(env)
	if !ok {
		screen.DrawText(x, y, maxW, fmt.Sprintf("%-8s no data yet", label), mutedStyle)
		return y + 1
	}

	// Line 1: env + version + engineVersion + gate badge.
	version := st.Version
	if version == "" {
		version = "-"
	}
	engine := st.EngineVersion
	if engine == "" {
		engine = "-"
	}
	gate := deployGateBadge(st.Gate, st.GateResult.Result)
	line := fmt.Sprintf("%-8s ver=%s  engine=%s  gate=%s", label, version, engine, gate)
	screen.DrawText(x, y, maxW, line, labelStyle)
	y++
	if y > bandBottom {
		return y
	}

	// Line 2: Argo CD sync / health (color-coded) + drift indicator.
	v.drawArgoLine(screen, x, y, maxW, st.Argocd)
	y++
	if y > bandBottom {
		return y
	}

	// Line 3: per-rollout summary (color + canary + latest analysis).
	rolloutText := formatRollouts(st.Rollouts)
	rolloutStyle := mutedStyle
	if hasFailedAnalysis(st.Rollouts) {
		rolloutStyle = v.errorStyle()
	}
	screen.DrawText(x, y, maxW, "  rollouts: "+rolloutText, rolloutStyle)
	y++
	if y > bandBottom {
		return y
	}

	// Line 4: gate legs summary (only when legs present).
	if legs := formatGateLegs(st.GateResult.Legs); legs != "" {
		screen.DrawText(x, y, maxW, "  gate: "+legs, mutedStyle)
		y++
	}
	return y
}

// drawArgoLine paints the Argo CD sync/health line, color-coding the
// sync + health tokens the same way node health is colored: green for
// Synced/Healthy, amber for Progressing/OutOfSync, red for Degraded.
func (v *View) drawArgoLine(screen *ui.Screen, x, y, maxW int, a client.ArgoStatus) {
	mutedStyle := v.Theme.SubtleStyle()
	prefix := "  argo: "
	screen.DrawText(x, y, maxW, prefix, mutedStyle)
	cx := x + len(prefix)
	limit := x + maxW

	sync := a.SyncStatus
	if sync == "" {
		sync = "unknown"
	}
	cx = drawColoredToken(screen, cx, y, limit, sync, v.argoColor(sync))

	cx = drawColoredToken(screen, cx, y, limit, " / ", mutedStyle)

	health := a.HealthStatus
	if health == "" {
		health = "unknown"
	}
	cx = drawColoredToken(screen, cx, y, limit, health, v.argoColor(health))

	if a.OutOfSync {
		drawColoredToken(screen, cx, y, limit, "  [drift]", v.warningStyle())
	}
}

// successStyle / warningStyle / errorStyle are the deployment strip's
// semantic color helpers. ui.Theme only ships ErrorStyle, so these wrap
// the raw Success / Warning / Error palette colors the same way the
// node-health renderer does -- centralized here so the badge / Argo /
// analysis coloring stays consistent.
func (v *View) successStyle() tcell.Style {
	return tcell.StyleDefault.Foreground(v.Theme.Success).Background(v.Theme.BG)
}

func (v *View) warningStyle() tcell.Style {
	return tcell.StyleDefault.Foreground(v.Theme.Warning).Background(v.Theme.BG)
}

func (v *View) errorStyle() tcell.Style {
	return tcell.StyleDefault.Foreground(v.Theme.Error).Background(v.Theme.BG)
}

// argoColor maps an Argo CD sync/health string to a theme style.
// green=Synced/Healthy, amber=Progressing/OutOfSync, red=Degraded/Missing.
func (v *View) argoColor(s string) tcell.Style {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "synced", "healthy":
		return v.successStyle()
	case "progressing", "outofsync", "out-of-sync", "suspended":
		return v.warningStyle()
	case "degraded", "missing", "unknown":
		return v.errorStyle()
	default:
		return v.Theme.SubtleStyle()
	}
}

// drawColoredToken writes text at (cx, y), clipping to limit, and
// returns the x just past what it wrote.
func drawColoredToken(screen *ui.Screen, cx, y, limit int, text string, style tcell.Style) int {
	for _, ch := range text {
		if cx >= limit {
			break
		}
		screen.SetCell(cx, y, ch, style)
		cx++
	}
	return cx
}

// deployGateBadge picks the displayed gate badge. Prefers the structured
// GateResult.Result ("pass"/"fail"/"unknown") when set; falls back to the
// flat Gate string. Rendered text-only per the no-emoji convention.
func deployGateBadge(gate, gateResult string) string {
	r := strings.ToLower(strings.TrimSpace(gateResult))
	switch r {
	case "pass":
		return "SUCCESS"
	case "fail":
		return "ERROR"
	}
	if g := strings.TrimSpace(gate); g != "" {
		return g
	}
	return "unknown"
}

// formatRollouts renders the per-rollout color / canary / analysis
// summary into one compact line. Blue-green rollouts show active/preview
// color; canary rollouts show weight + step. Each carries its latest
// analysis result as [x] pass / [ ] fail style tokens.
func formatRollouts(rollouts []client.RolloutStatus) string {
	if len(rollouts) == 0 {
		return "none"
	}
	parts := make([]string, 0, len(rollouts))
	for _, r := range rollouts {
		name := r.Name
		if name == "" {
			name = "(rollout)"
		}
		var detail string
		switch strings.ToLower(r.Kind) {
		case "bluegreen", "blue-green":
			active := r.ActiveColor
			if active == "" {
				active = "-"
			}
			preview := r.PreviewColor
			if preview == "" {
				preview = "-"
			}
			detail = fmt.Sprintf("bg active=%s preview=%s", active, preview)
		case "canary":
			detail = fmt.Sprintf("canary w=%d%% step=%d", r.CanaryWeight, r.CurrentStep)
		default:
			if r.Phase != "" {
				detail = r.Phase
			} else {
				detail = r.Kind
			}
		}
		parts = append(parts, fmt.Sprintf("%s[%s %s]", name, detail, analysisBadge(r.LatestAnalysisResult)))
	}
	return strings.Join(parts, "  ")
}

// analysisBadge renders a rollout's latest analysis result as a
// text-only pass/fail/pending token.
func analysisBadge(result string) string {
	switch strings.ToLower(strings.TrimSpace(result)) {
	case "successful", "success", "pass", "passed":
		return "[x]pass"
	case "failed", "fail", "error", "inconclusive":
		return "[ ]fail"
	case "":
		return "-"
	default:
		return result
	}
}

// hasFailedAnalysis reports whether any rollout's latest analysis failed,
// so the rollout line can be drawn in the error color.
func hasFailedAnalysis(rollouts []client.RolloutStatus) bool {
	for _, r := range rollouts {
		switch strings.ToLower(strings.TrimSpace(r.LatestAnalysisResult)) {
		case "failed", "fail", "error", "inconclusive":
			return true
		}
	}
	return false
}

// formatGateLegs renders the deploy-gate legs as a compact pass/fail
// summary. Empty string when there are no legs (the caller skips the row).
func formatGateLegs(legs []client.GateLeg) string {
	if len(legs) == 0 {
		return ""
	}
	parts := make([]string, 0, len(legs))
	for _, leg := range legs {
		mark := "[ ]"
		if leg.Passed {
			mark = "[x]"
		}
		name := leg.Name
		if name == "" {
			name = "leg"
		}
		parts = append(parts, mark+name)
	}
	return strings.Join(parts, " ")
}
