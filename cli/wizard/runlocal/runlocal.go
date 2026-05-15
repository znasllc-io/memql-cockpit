// Package runlocal is the "Set up local cluster" wizard reached
// from the launch splash. It probes the machine for the
// dependencies the local memQL cluster needs (genesis envelope,
// docker, docker compose, mkcert, free ports), reports pass/fail
// per check, and renders remediation hints for the failures so
// the operator can fix them and re-probe.
//
// The wizard is read-only today: it diagnoses, it doesn't auto-
// install. Auto-fixers (e.g. running `mkcert -install`) and the
// actual `docker compose up` invocation are scoped to a follow-up.
package runlocal

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/visionarys-io/memql-cockpit/cli/ui"
	corgenesis "github.com/visionarys-io/memql/component/genesis"
	"github.com/visionarys-io/memql/component/secret"
)

// checkStatus is the per-row outcome on the probe screen.
type checkStatus int

const (
	statusPending checkStatus = iota // not yet evaluated this round
	statusRunning                    // currently probing
	statusPass
	statusFail
)

// check is one row on the probe panel.
type check struct {
	label     string
	status    checkStatus
	detail    string // pass: short success info; fail: short failure reason
	hint      string // remediation guidance shown when status==statusFail
	runFn     func() (ok bool, detail string, hint string)
}

// Run renders the wizard and blocks until the user dismisses it.
func Run(screen *ui.Screen, theme ui.Theme) {
	w := &state{
		screen: screen,
		theme:  theme,
		checks: newChecks(),
	}
	w.runAll()
	for {
		w.draw()
		ev := screen.PollEvent()
		switch ev := ev.(type) {
		case *tcell.EventInterrupt:
		case *tcell.EventResize:
			screen.Sync()
		case *tcell.EventKey:
			if ev.Key() == tcell.KeyCtrlC || ev.Key() == tcell.KeyCtrlQ {
				return
			}
			if ev.Key() == tcell.KeyEscape {
				return
			}
			switch ev.Rune() {
			case 'r', 'R':
				w.runAll()
			case 'q', 'Q':
				return
			}
		}
	}
}

type state struct {
	screen *ui.Screen
	theme  ui.Theme
	checks []*check
}

func newChecks() []*check {
	return []*check{
		{
			label: "genesis.znas envelope present",
			runFn: checkGenesisExists,
		},
		{
			label: "MEMQL_MASTER_KEY in environment",
			runFn: checkMasterKeyInEnv,
		},
		{
			label: "Master key decrypts envelope",
			runFn: checkMasterKeyOpensGenesis,
		},
		{
			label: "Docker available",
			runFn: checkDockerAvailable,
		},
		{
			label: "Docker Compose available",
			runFn: checkDockerComposeAvailable,
		},
		{
			label: "mkcert installed",
			runFn: checkMkcertInstalled,
		},
		{
			label: "mkcert root CA in trust store",
			runFn: checkMkcertCAInstalled,
		},
		{
			label: "Port 80 free",
			runFn: checkPortFree(80),
		},
		{
			label: "Port 443 free",
			runFn: checkPortFree(443),
		},
		{
			label: "memql repo locatable",
			runFn: checkMemqlRepoLocatable,
		},
	}
}

// runAll runs every check in sequence, redrawing between each so
// the panel feels alive. Each runFn is expected to complete in
// well under a second (no blocking network).
func (w *state) runAll() {
	for _, c := range w.checks {
		c.status = statusPending
		c.detail = ""
		c.hint = ""
	}
	w.draw()
	for _, c := range w.checks {
		c.status = statusRunning
		w.draw()
		ok, detail, hint := c.runFn()
		if ok {
			c.status = statusPass
		} else {
			c.status = statusFail
		}
		c.detail = detail
		c.hint = hint
	}
}

func (w *state) draw() {
	screen := w.screen
	theme := w.theme
	screen.Clear(theme.BaseStyle())
	sw, sh := screen.Size()

	panelW := 78
	panelH := 26
	if panelW > sw-4 {
		panelW = sw - 4
	}
	if panelH > sh-4 {
		panelH = sh - 4
	}
	px := (sw - panelW) / 2
	py := (sh - panelH) / 2
	screen.DrawBox(px, py, panelW, panelH, theme.SubtleStyle())

	title := " Set up local cluster -- dependency check "
	screen.DrawText(px+(panelW-len(title))/2, py+1, len(title), title, theme.AccentStyle().Bold(true))

	subtitle := "This machine needs the following to host a local memQL cluster:"
	screen.DrawText(px+4, py+3, panelW-8, subtitle, theme.SubtleStyle())

	row := py + 5
	pass, fail := 0, 0
	for _, c := range w.checks {
		icon, color := iconFor(c.status, theme)
		screen.SetCell(px+4, row, icon, theme.BaseStyle().Foreground(color))
		labelStyle := theme.BaseStyle()
		if c.status == statusFail {
			labelStyle = theme.BaseStyle().Foreground(theme.Error)
		}
		screen.DrawText(px+6, row, panelW-10, c.label, labelStyle)
		if c.detail != "" {
			detailX := px + 6 + len(c.label) + 2
			screen.DrawText(detailX, row, panelW-(detailX-px)-4, c.detail, theme.SubtleStyle())
		}
		switch c.status {
		case statusPass:
			pass++
		case statusFail:
			fail++
		}
		row++
	}

	// First failing check's hint, rendered just above the action bar.
	if hint := firstFailHint(w.checks); hint != "" {
		row++
		for _, ln := range ui.WrapText("Next step: "+hint, panelW-8) {
			screen.DrawText(px+4, row, panelW-8, ln, theme.BaseStyle().Foreground(theme.Warning))
			row++
		}
	}

	// Summary + hint strip.
	summary := fmt.Sprintf("%d / %d checks passing", pass, len(w.checks))
	if fail == 0 && pass == len(w.checks) {
		summary = "All checks passed. Bring the stack up with:  docker compose -f docker/docker-compose.full.yml up -d"
	}
	screen.DrawText(px+4, py+panelH-3, panelW-8, summary, theme.BaseStyle())
	hint := "R:Re-probe   Esc:Back"
	screen.DrawText(px+4, py+panelH-2, panelW-8, hint, theme.SubtleStyle())

	screen.Show()
}

func iconFor(s checkStatus, theme ui.Theme) (rune, tcell.Color) {
	switch s {
	case statusPass:
		return '✓', theme.Success
	case statusFail:
		return '✗', theme.Error
	case statusRunning:
		return '◌', theme.Warning
	default:
		return '·', theme.Subtle
	}
}

func firstFailHint(checks []*check) string {
	for _, c := range checks {
		if c.status == statusFail && c.hint != "" {
			return c.hint
		}
	}
	return ""
}

// ----- individual probes -----------------------------------------------------

func checkGenesisExists() (bool, string, string) {
	path := genesisPath()
	if path == "" {
		return false, "", "Couldn't resolve $HOME; can't locate genesis.znas."
	}
	if _, err := os.Stat(path); err != nil {
		return false, "missing", fmt.Sprintf("Launch the cockpit on a fresh machine to run the setup wizard, or restore %s from backup.", path)
	}
	return true, path, ""
}

func checkMasterKeyInEnv() (bool, string, string) {
	if v := strings.TrimSpace(os.Getenv(secret.EnvMasterKey)); v != "" {
		return true, "set", ""
	}
	return false, "", "Export MEMQL_MASTER_KEY in your shell (cockpit added an `export` line to your ~/.bashrc / ~/.zshrc during first-launch setup — start a new shell so it's picked up)."
}

func checkMasterKeyOpensGenesis() (bool, string, string) {
	path := genesisPath()
	if path == "" {
		return false, "", "Couldn't resolve $HOME; can't locate genesis.znas."
	}
	if _, err := os.Stat(path); err != nil {
		return false, "no envelope", "Create the envelope via the first-launch wizard."
	}
	if strings.TrimSpace(os.Getenv(secret.EnvMasterKey)) == "" {
		return false, "no key", "Export MEMQL_MASTER_KEY in your shell."
	}
	if _, err := corgenesis.OpenFile(path); err != nil {
		return false, "decrypt failed", "The key in your environment doesn't open the envelope on disk. Either you're using the wrong key, or the envelope was sealed under a different key — re-create the envelope, or update MEMQL_MASTER_KEY."
	}
	return true, "ok", ""
}

func checkDockerAvailable() (bool, string, string) {
	if _, err := exec.LookPath("docker"); err != nil {
		return false, "not installed", "Install Docker Desktop (macOS / Windows) or Docker Engine (Linux): https://docs.docker.com/get-docker/"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := exec.CommandContext(ctx, "docker", "info").Run(); err != nil {
		return false, "daemon down", "Docker is installed but the daemon isn't responding. Start Docker Desktop, or `sudo systemctl start docker` on Linux."
	}
	return true, "running", ""
}

func checkDockerComposeAvailable() (bool, string, string) {
	if _, err := exec.LookPath("docker"); err != nil {
		return false, "no docker", "Install Docker first; Compose ships with modern Docker installs."
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := exec.CommandContext(ctx, "docker", "compose", "version").Run(); err != nil {
		return false, "missing", "Update Docker to a recent release (Compose V2 is bundled with Docker 20.10+). Verify with: docker compose version"
	}
	return true, "v2", ""
}

func checkMkcertInstalled() (bool, string, string) {
	if _, err := exec.LookPath("mkcert"); err != nil {
		return false, "not installed", "Install mkcert. macOS: `brew install mkcert nss`. Ubuntu/Debian: `sudo apt install libnss3-tools` then grab the binary from https://github.com/FiloSottile/mkcert/releases. Arch: `sudo pacman -S mkcert`."
	}
	return true, "ok", ""
}

func checkMkcertCAInstalled() (bool, string, string) {
	if _, err := exec.LookPath("mkcert"); err != nil {
		return false, "mkcert missing", "Install mkcert first (see check above)."
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "mkcert", "-CAROOT").Output()
	if err != nil {
		return false, "CAROOT lookup failed", "Run `mkcert -CAROOT` manually to see why; install the CA with `mkcert -install`."
	}
	caRoot := strings.TrimSpace(string(out))
	if caRoot == "" {
		return false, "no CAROOT", "Run `mkcert -install` to provision the local CA."
	}
	if _, err := os.Stat(filepath.Join(caRoot, "rootCA.pem")); err != nil {
		return false, "CA not generated", "Run `mkcert -install` to provision and install the local root CA into the system trust store."
	}
	return true, caRoot, ""
}

func checkPortFree(port int) func() (bool, string, string) {
	return func() (bool, string, string) {
		l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err != nil {
			return false, "in use", fmt.Sprintf("Something else is listening on :%d. Identify with `sudo lsof -i :%d` (macOS/Linux) and stop it before bringing the cluster up.", port, port)
		}
		_ = l.Close()
		return true, "ok", ""
	}
}

func checkMemqlRepoLocatable() (bool, string, string) {
	candidates := []string{}
	if env := strings.TrimSpace(os.Getenv("MEMQL_REPO")); env != "" {
		candidates = append(candidates, env)
	}
	// Common sibling locations relative to where cockpit is typically checked
	// out (e.g. ~/projects/memql/memql). Best-effort; failure is non-blocking.
	if cwd, err := os.Getwd(); err == nil {
		candidates = append(candidates,
			filepath.Join(cwd, "..", "memql"),
			filepath.Join(filepath.Dir(cwd), "memql"),
		)
	}
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates, filepath.Join(home, "projects", "memql", "memql"))
	}
	for _, p := range candidates {
		marker := filepath.Join(p, "docker", "docker-compose.full.yml")
		if _, err := os.Stat(marker); err == nil {
			abs, _ := filepath.Abs(p)
			return true, abs, ""
		}
	}
	return false, "not found", "Clone the memql repo (https://github.com/znasllc-io/memql) and either run cockpit from its parent dir or export MEMQL_REPO=/path/to/memql. The wizard needs docker/docker-compose.full.yml to bring the stack up."
}

func genesisPath() string {
	if p := strings.TrimSpace(os.Getenv("MEMQL_GENESIS_PATH")); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".memql", "genesis.znas")
}
