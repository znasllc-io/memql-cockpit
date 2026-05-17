// Package genesis hosts the first-launch wizard that creates the
// operator's ~/.memql/genesis.znas envelope from a .env file. Wraps
// memql/component/genesis.Seal in a tcell single-panel TUI.
//
// The wizard is launch-only: cockpit calls Run when no envelope
// exists, and once Run returns the main TUI continues. Cancel
// returns the user to the shell.
package genesis

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/gdamore/tcell/v2"
	"github.com/znasllc-io/memql-cockpit/cli/ui"
	corgenesis "github.com/znasllc-io/memql/component/genesis"
	"github.com/znasllc-io/memql/component/secret"
)

// Result reports what the wizard did.
type Result int

const (
	ResultCanceled Result = iota // user pressed Esc / Ctrl+Q without finishing
	ResultSealed                 // envelope written
	ResultError                  // wizard ended on an error (message in the panel)
)

// Run drives the wizard until it terminates. Blocks polling events
// on screen. Returns the outcome so the caller can decide whether
// to continue into the main TUI (ResultSealed) or exit.
func Run(screen *ui.Screen, theme ui.Theme) Result {
	w := &wizard{
		screen:  screen,
		theme:   theme,
		step:    stepIntro,
		envPath: defaultEnvPath(),
		outPath: defaultOutPath(),
	}
	for {
		w.draw()
		ev := screen.PollEvent()
		switch ev := ev.(type) {
		case *tcell.EventInterrupt:
			// fall through -> redraw next iter
		case *tcell.EventResize:
			screen.Sync()
		case *tcell.EventKey:
			if ev.Key() == tcell.KeyCtrlC || ev.Key() == tcell.KeyCtrlQ {
				return ResultCanceled
			}
			if !w.handleKey(ev) {
				return w.result
			}
		}
	}
}

type step int

const (
	stepIntro step = iota
	stepEnvPath
	stepConfirmKey
	stepDone
)

type wizard struct {
	screen *ui.Screen
	theme  ui.Theme

	step step

	// Input state.
	envPath string
	outPath string

	// Seal output / progress.
	sealResult *corgenesis.SealResult
	sealError  error

	// Set when we run Seal and the master key was freshly generated.
	// Drives the stepConfirmKey gate. nil at other steps.
	pendingKey string
	pendingFn  func(bool) // resumes Seal after the user confirms / declines

	result Result
}

func defaultEnvPath() string {
	for _, candidate := range []string{".env.local", ".env"} {
		if _, err := os.Stat(candidate); err == nil {
			abs, err := filepath.Abs(candidate)
			if err == nil {
				return abs
			}
			return candidate
		}
	}
	return ""
}

func defaultOutPath() string {
	if p := os.Getenv("MEMQL_GENESIS_PATH"); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".memql/genesis.znas"
	}
	return filepath.Join(home, ".memql", "genesis.znas")
}

// handleKey advances the wizard state. Returns false when the
// wizard is finished (caller should exit the loop).
func (w *wizard) handleKey(ev *tcell.EventKey) bool {
	switch w.step {
	case stepIntro:
		if ev.Key() == tcell.KeyEnter {
			w.step = stepEnvPath
		}
		if ev.Key() == tcell.KeyEscape {
			w.result = ResultCanceled
			return false
		}
		return true

	case stepEnvPath:
		switch ev.Key() {
		case tcell.KeyEscape:
			w.result = ResultCanceled
			return false
		case tcell.KeyEnter:
			w.runSeal()
			// runSeal sets either pendingKey (-> stepConfirmKey),
			// or sealResult/sealError (-> stepDone).
			return true
		case tcell.KeyBackspace, tcell.KeyBackspace2:
			if len(w.envPath) > 0 {
				w.envPath = w.envPath[:len(w.envPath)-1]
			}
			return true
		case tcell.KeyRune:
			r := ev.Rune()
			if r >= 0x20 && r != 0x7f {
				w.envPath += string(r)
			}
			return true
		}
		return true

	case stepConfirmKey:
		switch ev.Rune() {
		case 'y', 'Y':
			cb := w.pendingFn
			w.pendingFn = nil
			w.pendingKey = ""
			if cb != nil {
				cb(true)
			}
			// runSeal's goroutine continues; after Seal returns we
			// land on stepDone via the result path.
			return true
		case 'n', 'N':
			cb := w.pendingFn
			w.pendingFn = nil
			w.pendingKey = ""
			if cb != nil {
				cb(false)
			}
			return true
		}
		if ev.Key() == tcell.KeyEscape {
			cb := w.pendingFn
			w.pendingFn = nil
			w.pendingKey = ""
			if cb != nil {
				cb(false)
			}
			return true
		}
		return true

	case stepDone:
		// Any key returns. ResultSealed if Seal succeeded, ResultError
		// otherwise.
		if w.sealError != nil {
			w.result = ResultError
		} else {
			w.result = ResultSealed
		}
		return false
	}
	return true
}

// runSeal walks the entries through component/genesis.Seal. The
// confirmation callback is invoked SYNCHRONOUSLY from Seal -- we
// pause the wizard there and let the next handleKey resume by
// calling the resume function we stashed.
//
// The callback's blocking nature works because Seal runs on the
// goroutine handling the keystroke. We capture the resume closure
// in pendingFn and switch the step to stepConfirmKey; the next
// keystroke fires the callback, which unblocks Seal back inside
// runSeal, which then sets the final state.
//
// We use a one-shot channel so the synchronous blocking happens
// inside ConfirmMasterKey but the wizard's event loop owns the
// "is the user pressing Y or N" question.
func (w *wizard) runSeal() {
	envPath := strings.TrimSpace(w.envPath)
	if envPath == "" {
		w.sealError = fmt.Errorf(".env path is required")
		w.step = stepDone
		return
	}

	entries, err := corgenesis.ParseEnvFile(envPath)
	if err != nil {
		w.sealError = err
		w.step = stepDone
		return
	}
	manifest, err := corgenesis.LoadManifest("")
	if err != nil {
		w.sealError = err
		w.step = stepDone
		return
	}

	// ConfirmMasterKey is called inline by Seal. Run Seal on a
	// goroutine so we can paint the confirm-key step while it
	// blocks. The result channel feeds the final state.
	type sealOutcome struct {
		res *corgenesis.SealResult
		err error
	}
	outcomeCh := make(chan sealOutcome, 1)
	confirmCh := make(chan bool, 1)

	go func() {
		res, err := corgenesis.Seal(corgenesis.SealOptions{
			Entries:       entries,
			Manifest:      manifest,
			OutPath:       w.outPath,
			SourceEnvPath: envPath,
			SyncShellRCs:  true,
			ConfirmMasterKey: func(masterKeyHex string) (bool, error) {
				w.pendingKey = masterKeyHex
				w.step = stepConfirmKey
				w.pendingFn = func(ok bool) { confirmCh <- ok }
				w.screen.PostEvent(tcell.NewEventInterrupt(nil))
				return <-confirmCh, nil
			},
		})
		outcomeCh <- sealOutcome{res, err}
		w.screen.PostEvent(tcell.NewEventInterrupt(nil))
	}()

	// Drive the event loop while Seal runs.
	for {
		select {
		case out := <-outcomeCh:
			w.sealResult = out.res
			w.sealError = out.err
			w.step = stepDone
			return
		default:
		}

		w.draw()
		ev := w.screen.PollEvent()
		switch ev := ev.(type) {
		case *tcell.EventInterrupt:
			// loop back -- maybe outcome arrived, or pendingKey
			// transitioned us to stepConfirmKey.
		case *tcell.EventResize:
			w.screen.Sync()
		case *tcell.EventKey:
			if ev.Key() == tcell.KeyCtrlC || ev.Key() == tcell.KeyCtrlQ {
				// Hard cancel mid-seal: drain the confirm channel
				// with a "no" so the goroutine exits, then bail.
				select {
				case confirmCh <- false:
				default:
				}
				w.sealError = fmt.Errorf("canceled by user")
				w.step = stepDone
				return
			}
			if !w.handleKey(ev) {
				// handleKey wants to exit; capture outcome if it landed.
				select {
				case out := <-outcomeCh:
					w.sealResult = out.res
					w.sealError = out.err
				default:
				}
				w.step = stepDone
				return
			}
		}
	}
}

// draw renders the wizard panel for the current step.
func (w *wizard) draw() {
	screen := w.screen
	theme := w.theme
	screen.Clear(theme.BaseStyle())
	sw, sh := screen.Size()

	panelW := 72
	panelH := 20
	if panelW > sw-4 {
		panelW = sw - 4
	}
	if panelH > sh-4 {
		panelH = sh - 4
	}
	px := (sw - panelW) / 2
	py := (sh - panelH) / 2
	screen.DrawBox(px, py, panelW, panelH, theme.SubtleStyle())

	title := " memQL Cockpit — first-launch setup "
	screen.DrawText(px+(panelW-len(title))/2, py+1, len(title), title, theme.AccentStyle().Bold(true))

	switch w.step {
	case stepIntro:
		w.drawIntro(px, py, panelW)
	case stepEnvPath:
		w.drawEnvPath(px, py, panelW)
	case stepConfirmKey:
		w.drawConfirmKey(px, py, panelW)
	case stepDone:
		w.drawDone(px, py, panelW)
	}

	screen.Show()
}

func (w *wizard) drawIntro(px, py, panelW int) {
	lines := []string{
		"No ~/.memql/genesis.znas was found on this machine.",
		"",
		"This wizard will create one by sealing your .env file under a master key.",
		"The envelope holds your operator-side secrets (API keys, cluster domain,",
		"identity URLs) and is what every memql process reads at startup.",
		"",
		"You will need:",
		"  • a .env file that covers the manifest floor",
		"  • a place to save the generated master key (password manager)",
		"",
		"Nothing is written until you confirm.",
	}
	w.drawBody(px, py, panelW, lines)
	w.drawHint(px, py, panelW, "Enter:Continue   Esc:Cancel")
}

func (w *wizard) drawEnvPath(px, py, panelW int) {
	lines := []string{
		"Path to your .env file (./.env.local and ./.env auto-detected):",
		"",
	}
	w.drawBody(px, py, panelW, lines)

	fieldY := py + 5
	fieldX := px + 4
	fieldW := panelW - 8
	w.screen.FillRect(fieldX, fieldY, fieldW, 1, tcell.StyleDefault.Background(tcell.NewRGBColor(50, 55, 65)).Foreground(w.theme.FG))
	w.screen.DrawText(fieldX+1, fieldY, fieldW-2, w.envPath, tcell.StyleDefault.Background(tcell.NewRGBColor(50, 55, 65)).Foreground(w.theme.FG))

	cursorX := fieldX + 1 + len(w.envPath)
	if cursorX < fieldX+fieldW-1 {
		w.screen.SetCell(cursorX, fieldY, ' ', tcell.StyleDefault.Background(w.theme.FG))
	}

	w.screen.DrawText(px+4, fieldY+2, panelW-8, fmt.Sprintf("Output: %s", w.outPath), w.theme.SubtleStyle())

	w.drawHint(px, py, panelW, "Enter:Seal envelope   Esc:Cancel")
}

func (w *wizard) drawConfirmKey(px, py, panelW int) {
	lines := []string{
		"MEMQL_MASTER_KEY -- SAVE THIS NOW.",
		"",
		"It will not be shown again. Losing it makes genesis.znas unrecoverable.",
		"",
		"  " + w.pendingKey,
		"",
		"  1. Paste it into your password manager.",
		"  2. Cockpit will also add it to your ~/.bashrc / ~/.zshrc so future",
		"     shells decrypt the envelope without re-typing.",
	}
	w.drawBody(px, py, panelW, lines)
	w.drawHint(px, py, panelW, "Y:Saved -- write envelope   N/Esc:Cancel")
}

func (w *wizard) drawDone(px, py, panelW int) {
	var lines []string
	if w.sealError != nil {
		lines = []string{
			"Setup failed:",
			"",
			"  " + w.sealError.Error(),
		}
		w.drawBody(px, py, panelW, lines)
		w.drawHint(px, py, panelW, "Any key to exit")
		return
	}
	res := w.sealResult
	envelope := "envelope"
	if res != nil && res.FirstTime {
		envelope = "envelope (new)"
	} else if res != nil {
		envelope = "envelope (updated)"
	}
	keyNote := ""
	if res != nil && res.ReusedKeyFromEnv {
		keyNote = "Reused " + secret.EnvMasterKey + " already present in your environment."
	}
	lines = []string{
		"Setup complete.",
		"",
		"  Sealed " + envelope + " at " + w.outPath,
	}
	if keyNote != "" {
		lines = append(lines, "  "+keyNote)
	}
	if res != nil && res.ManifestSource != "" {
		lines = append(lines, "  Manifest source: "+res.ManifestSource)
	}
	w.drawBody(px, py, panelW, lines)
	w.drawHint(px, py, panelW, "Any key to continue")
}

func (w *wizard) drawBody(px, py, panelW int, lines []string) {
	y := py + 3
	for _, ln := range lines {
		w.screen.DrawText(px+4, y, panelW-8, ln, w.theme.BaseStyle())
		y++
	}
}

func (w *wizard) drawHint(px, py, panelW int, hint string) {
	w.screen.DrawText(px+4, py+/*panelHeight is 20, hint at row -2*/ 18, panelW-8, hint, w.theme.SubtleStyle())
}
