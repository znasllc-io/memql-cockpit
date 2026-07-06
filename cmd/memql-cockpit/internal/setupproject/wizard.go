package setupproject

import (
	"fmt"
	"strings"

	"github.com/gdamore/tcell/v2"
	"github.com/znasllc-io/memql-cockpit/cli/ui"
)

// wizard.go is the interactive front-end for `setup project`, reached when the
// required flags are absent and stdin is a TTY. It follows the genesis-wizard
// pattern (cli/wizard/genesis/genesis.go): a single centered tcell panel that
// walks intro -> inputs -> confirm. Unlike genesis it does NOT execute inside
// the wizard -- it only collects a Config and reports Confirmed/Canceled, so
// the caller can tear the screen down before the flow streams bootstrap output
// to a normal terminal.
//
// The state machine (wizModel) is deliberately separated from drawing so it can
// be unit-tested by feeding synthetic key events, with no screen involved.

// Outcome is what RunWizard returns.
type Outcome int

const (
	OutcomePending   Outcome = iota // still collecting (internal)
	OutcomeConfirmed                // user confirmed -- run the flow with the returned Config
	OutcomeCanceled                 // user backed out
)

type wizStep int

const (
	stepIntro wizStep = iota
	stepProduct
	stepOrg
	stepDomain
	stepEngine
	stepConfirm
	stepDone
)

// wizModel is the screen-free state of the wizard. field values seed from the
// caller's partial flags so a half-specified invocation pre-fills what it can.
type wizModel struct {
	step    wizStep
	product string
	org     string
	domain  string
	engine  string
	errMsg  string
	outcome Outcome
}

// newWizModel seeds the model from any flags already provided. Fields seed from
// the RAW config: an unset domain stays empty in the field (the default is
// shown as a hint and applied only in config()), so typing never appends to a
// pre-filled default.
func newWizModel(seed Config) *wizModel {
	return &wizModel{
		step:    stepIntro,
		product: seed.Product,
		org:     seed.ProductOrg,
		domain:  seed.Domain,
		engine:  seed.EngineVersion,
		outcome: OutcomePending,
	}
}

// config folds the collected fields back onto the seed config (preserving the
// non-interactive fields: Dir, CreateRepos, TemplateRef, TemplateRepo, Shallow,
// DryRun).
func (m *wizModel) config(seed Config) Config {
	seed.Product = strings.TrimSpace(m.product)
	seed.ProductOrg = strings.TrimSpace(m.org)
	seed.Domain = strings.TrimSpace(m.domain)
	seed.EngineVersion = strings.TrimSpace(m.engine)
	return seed.withDefaults()
}

// fieldPtr returns the editable field for a text-input step (nil otherwise).
func (m *wizModel) fieldPtr() *string {
	switch m.step {
	case stepProduct:
		return &m.product
	case stepOrg:
		return &m.org
	case stepDomain:
		return &m.domain
	case stepEngine:
		return &m.engine
	}
	return nil
}

// validateStep checks the current field before advancing. Domain empty is
// tolerated (default applied); engine is optional.
func (m *wizModel) validateStep() error {
	switch m.step {
	case stepProduct:
		return ValidateProduct(m.product)
	case stepOrg:
		return ValidateOrg(m.org)
	case stepDomain:
		return ValidateDomain(m.domain)
	}
	return nil
}

// handleKey advances the state machine for one key event. Returns false when
// the wizard is finished (outcome is set).
func (m *wizModel) handleKey(ev *tcell.EventKey) bool {
	// Esc / Ctrl-C / Ctrl-Q cancel from anywhere.
	if ev.Key() == tcell.KeyEscape || ev.Key() == tcell.KeyCtrlC || ev.Key() == tcell.KeyCtrlQ {
		m.outcome = OutcomeCanceled
		m.step = stepDone
		return false
	}

	switch m.step {
	case stepIntro:
		if ev.Key() == tcell.KeyEnter {
			m.step = stepProduct
		}
		return true

	case stepProduct, stepOrg, stepDomain, stepEngine:
		field := m.fieldPtr()
		switch ev.Key() {
		case tcell.KeyEnter:
			if err := m.validateStep(); err != nil {
				m.errMsg = err.Error()
				return true
			}
			m.errMsg = ""
			m.step++ // next step in the ordered sequence
		case tcell.KeyBackspace, tcell.KeyBackspace2:
			if field != nil && len(*field) > 0 {
				*field = trimLastRune(*field)
			}
		case tcell.KeyRune:
			r := ev.Rune()
			if field != nil && r >= 0x20 && r != 0x7f {
				*field += string(r)
			}
		}
		return true

	case stepConfirm:
		switch ev.Rune() {
		case 'y', 'Y':
			m.outcome = OutcomeConfirmed
			m.step = stepDone
			return false
		case 'e', 'E':
			// Edit -- jump back to the first field.
			m.errMsg = ""
			m.step = stepProduct
			return true
		}
		if ev.Key() == tcell.KeyEnter {
			m.outcome = OutcomeConfirmed
			m.step = stepDone
			return false
		}
		return true
	}
	return true
}

func trimLastRune(s string) string {
	r := []rune(s)
	if len(r) == 0 {
		return s
	}
	return string(r[:len(r)-1])
}

// RunWizard drives the wizard on the given screen until it terminates, then
// returns the collected Config and the outcome. seed pre-fills fields from any
// flags already supplied.
func RunWizard(screen *ui.Screen, theme ui.Theme, seed Config) (Config, Outcome) {
	m := newWizModel(seed)
	for {
		drawWizard(screen, theme, m)
		ev := screen.PollEvent()
		switch ev := ev.(type) {
		case *tcell.EventResize:
			screen.Sync()
		case *tcell.EventKey:
			if !m.handleKey(ev) {
				return m.config(seed), m.outcome
			}
		}
	}
}

// drawWizard renders the current step into a centered panel.
func drawWizard(screen *ui.Screen, theme ui.Theme, m *wizModel) {
	screen.Clear(theme.BaseStyle())
	sw, sh := screen.Size()

	panelW, panelH := 74, 22
	if panelW > sw-4 {
		panelW = sw - 4
	}
	if panelH > sh-4 {
		panelH = sh - 4
	}
	if panelW < 10 || panelH < 6 {
		screen.Show()
		return
	}
	px := (sw - panelW) / 2
	py := (sh - panelH) / 2
	screen.DrawBox(px, py, panelW, panelH, theme.SubtleStyle())

	title := " memQL Cockpit -- set up project "
	screen.DrawText(px+(panelW-len(title))/2, py+1, len(title), title, theme.AccentStyle().Bold(true))

	var hint string
	switch m.step {
	case stepIntro:
		drawIntro(screen, theme, px, py, panelW)
		hint = "Enter:Begin   Esc:Cancel"
	case stepProduct:
		drawField(screen, theme, px, py, panelW, m,
			"Product name (lowercase slug, e.g. acme):", m.product)
		hint = "Enter:Next   Esc:Cancel"
	case stepOrg:
		drawField(screen, theme, px, py, panelW, m,
			"Product org / GitHub owner (e.g. acme-io):", m.org)
		hint = "Enter:Next   Esc:Cancel"
	case stepDomain:
		drawField(screen, theme, px, py, panelW, m,
			"Local front-door domain (blank = "+defaultDomain+"):", m.domain)
		hint = "Enter:Next   Esc:Cancel"
	case stepEngine:
		drawField(screen, theme, px, py, panelW, m,
			"Engine version to pin (blank = latest release):", m.engine)
		hint = "Enter:Review   Esc:Cancel"
	case stepConfirm:
		drawConfirm(screen, theme, px, py, panelW, m)
		hint = "Y/Enter:Stamp   E:Edit   Esc:Cancel"
	}
	screen.DrawText(px+4, py+panelH-2, panelW-8, hint, theme.SubtleStyle())
	screen.Show()
}

func drawIntro(screen *ui.Screen, theme ui.Theme, px, py, panelW int) {
	lines := []string{
		"Stamp a new memQL product workspace from the memql-project template.",
		"",
		"This will clone the template, reset it to fresh git history, and run",
		"its bootstrap to scaffold your product's carrier + client repos and",
		"consolidate them under a committed go.work.",
		"",
		"You'll be asked for:",
		"  - a product name (lowercase slug)",
		"  - the GitHub org/user that will own the product repos",
		"  - a local front-door domain (optional)",
		"  - an engine version to pin (optional)",
		"",
		"Nothing is written until you confirm.",
	}
	drawBody(screen, theme, px, py, panelW, lines)
}

func drawField(screen *ui.Screen, theme ui.Theme, px, py, panelW int, m *wizModel, prompt, value string) {
	drawBody(screen, theme, px, py, panelW, []string{prompt, ""})

	fieldY := py + 5
	fieldX := px + 4
	fieldW := panelW - 8
	fieldStyle := tcell.StyleDefault.Background(tcell.NewRGBColor(50, 55, 65)).Foreground(theme.FG)
	screen.FillRect(fieldX, fieldY, fieldW, 1, fieldStyle)
	screen.DrawText(fieldX+1, fieldY, fieldW-2, value, fieldStyle)
	cursorX := fieldX + 1 + len([]rune(value))
	if cursorX < fieldX+fieldW-1 {
		screen.SetCell(cursorX, fieldY, ' ', tcell.StyleDefault.Background(theme.FG))
	}

	if m.errMsg != "" {
		screen.DrawText(px+4, fieldY+2, panelW-8, "! "+m.errMsg, theme.ErrorStyle())
	}
}

func drawConfirm(screen *ui.Screen, theme ui.Theme, px, py, panelW int, m *wizModel) {
	domain := strings.TrimSpace(m.domain)
	if domain == "" {
		domain = defaultDomain
	}
	engine := strings.TrimSpace(m.engine)
	if engine == "" {
		engine = "(latest release, resolved at stamp time)"
	}
	urls := FrontDoorURLs(domain)
	lines := []string{
		"Review the workspace to stamp:",
		"",
		fmt.Sprintf("  product:        %s", m.product),
		fmt.Sprintf("  org:            %s", m.org),
		fmt.Sprintf("  engine version: %s", engine),
		"",
		"  front door will be:",
		"    " + urls[0],
		"    " + urls[1],
		"    " + urls[2],
		"",
		fmt.Sprintf("  stamped repos:  %s-carrier, %s-client", m.product, m.product),
	}
	drawBody(screen, theme, px, py, panelW, lines)
}

func drawBody(screen *ui.Screen, theme ui.Theme, px, py, panelW int, lines []string) {
	for i, ln := range lines {
		screen.DrawText(px+4, py+3+i, panelW-8, ln, theme.BaseStyle())
	}
}
