// Package runlocal is the "Set up local cluster" wizard reached
// from the launch splash. Two modes:
//
//   1. running -- when the local memQL cluster is already up,
//      shows the service inventory + status so the operator can
//      see what's online without running irrelevant port checks.
//   2. deps-check -- when nothing is running, probes the machine
//      for prerequisites (docker, mkcert, free ports, etc.) and
//      surfaces remediation hints for each failure.
//
// The wizard decides which mode to enter by asking
// dockerprobe.ClusterRunning at entry + on every R re-probe.
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
	"github.com/visionarys-io/memql-cockpit/cli/dockerprobe"
	"github.com/visionarys-io/memql-cockpit/cli/ui"
	corgenesis "github.com/visionarys-io/memql/component/genesis"
	"github.com/visionarys-io/memql/component/secret"
)

// Run renders the wizard and blocks until the user dismisses it.
func Run(screen *ui.Screen, theme ui.Theme) {
	w := &state{screen: screen, theme: theme}
	w.refresh()
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
				w.refresh()
			case 'q', 'Q':
				return
			}
		}
	}
}

// mode is the panel the wizard renders right now. Settled by
// refresh() based on whether docker reports any running cluster
// containers.
type mode int

const (
	modeRunning   mode = iota // cluster up -- show status list
	modeDepsCheck             // cluster down -- run prerequisite probe
)

type state struct {
	screen *ui.Screen
	theme  ui.Theme

	mode       mode
	containers []dockerprobe.Container
	checks     []*check
}

// refresh polls docker. If any container is up, we render the
// running-status panel. Otherwise we run the dep-check probe.
func (s *state) refresh() {
	running, containers, _ := dockerprobe.ClusterRunning()
	s.containers = containers
	if running {
		s.mode = modeRunning
		return
	}
	s.mode = modeDepsCheck
	s.checks = newChecks()
	s.draw() // paint pending rows before the first probe so the panel doesn't blink
	for _, c := range s.checks {
		c.status = statusRunning
		s.draw()
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

// draw paints whichever panel mode says is current.
func (s *state) draw() {
	screen := s.screen
	theme := s.theme
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

	switch s.mode {
	case modeRunning:
		s.drawRunning(px, py, panelW, panelH)
	case modeDepsCheck:
		s.drawDepsCheck(px, py, panelW, panelH)
	}
	screen.Show()
}

func (s *state) drawRunning(px, py, panelW, panelH int) {
	theme := s.theme
	title := " Local cluster -- running "
	s.screen.DrawText(px+(panelW-len(title))/2, py+1, len(title), title, theme.AccentStyle().Bold(true))

	subtitle := fmt.Sprintf("%d service(s) reported by docker compose project %q.", len(s.containers), dockerprobe.ProjectName)
	s.screen.DrawText(px+4, py+3, panelW-8, subtitle, theme.SubtleStyle())

	row := py + 5
	running, degraded, stopped := 0, 0, 0
	for _, c := range s.containers {
		icon, color, classBucket := iconForContainer(c, theme)
		switch classBucket {
		case "running":
			running++
		case "degraded":
			degraded++
		default:
			stopped++
		}
		s.screen.SetCell(px+4, row, icon, theme.BaseStyle().Foreground(color))

		nameLabel := fmt.Sprintf("%-20s %-20s", c.Service, c.Name)
		s.screen.DrawText(px+6, row, panelW-10, nameLabel, theme.BaseStyle())
		status := c.Status
		if c.HealthHint != "" && !strings.Contains(status, "("+c.HealthHint+")") {
			status += " (" + c.HealthHint + ")"
		}
		statusX := px + 6 + 42
		s.screen.DrawText(statusX, row, panelW-(statusX-px)-4, status, theme.SubtleStyle())
		row++
	}

	if len(s.containers) == 0 {
		s.screen.DrawText(px+4, row, panelW-8, "No containers found for project "+dockerprobe.ProjectName+".", theme.BaseStyle().Foreground(theme.Warning))
		row++
	}

	row++
	summary := fmt.Sprintf("%d running   %d degraded   %d stopped", running, degraded, stopped)
	s.screen.DrawText(px+4, row, panelW-8, summary, theme.BaseStyle())

	hint := "R:Re-probe   Esc:Back"
	if stopped > 0 || degraded > 0 {
		hint = "R:Re-probe   Esc:Back   (stopped / degraded containers: investigate with `docker logs <name>`)"
	}
	s.screen.DrawText(px+4, py+panelH-2, panelW-8, hint, theme.SubtleStyle())
}

func (s *state) drawDepsCheck(px, py, panelW, panelH int) {
	theme := s.theme
	title := " Set up local cluster -- dependency check "
	s.screen.DrawText(px+(panelW-len(title))/2, py+1, len(title), title, theme.AccentStyle().Bold(true))

	subtitle := "This machine needs the following to host a local memQL cluster:"
	s.screen.DrawText(px+4, py+3, panelW-8, subtitle, theme.SubtleStyle())

	row := py + 5
	pass, fail := 0, 0
	for _, c := range s.checks {
		icon, color := iconFor(c.status, theme)
		s.screen.SetCell(px+4, row, icon, theme.BaseStyle().Foreground(color))
		labelStyle := theme.BaseStyle()
		if c.status == statusFail {
			labelStyle = theme.BaseStyle().Foreground(theme.Error)
		}
		s.screen.DrawText(px+6, row, panelW-10, c.label, labelStyle)
		if c.detail != "" {
			detailX := px + 6 + len(c.label) + 2
			s.screen.DrawText(detailX, row, panelW-(detailX-px)-4, c.detail, theme.SubtleStyle())
		}
		switch c.status {
		case statusPass:
			pass++
		case statusFail:
			fail++
		}
		row++
	}

	if hint := firstFailHint(s.checks); hint != "" {
		row++
		for _, ln := range ui.WrapText("Next step: "+hint, panelW-8) {
			s.screen.DrawText(px+4, row, panelW-8, ln, theme.BaseStyle().Foreground(theme.Warning))
			row++
		}
	}

	summary := fmt.Sprintf("%d / %d checks passing", pass, len(s.checks))
	if fail == 0 && pass == len(s.checks) {
		summary = "All checks passed. Bring the stack up with:  docker compose -f docker/docker-compose.full.yml up -d"
	}
	s.screen.DrawText(px+4, py+panelH-3, panelW-8, summary, theme.BaseStyle())
	hint := "R:Re-probe   Esc:Back"
	s.screen.DrawText(px+4, py+panelH-2, panelW-8, hint, theme.SubtleStyle())
}

// iconForContainer maps a probed container to (glyph, colour,
// coarse-bucket). The bucket drives the summary counters.
func iconForContainer(c dockerprobe.Container, theme ui.Theme) (rune, tcell.Color, string) {
	switch c.State {
	case "running":
		if c.HealthHint == "unhealthy" {
			return '●', theme.Error, "degraded"
		}
		if c.HealthHint == "starting" {
			return '◌', theme.Warning, "degraded"
		}
		return '●', theme.Success, "running"
	case "paused", "restarting", "created":
		return '◌', theme.Warning, "degraded"
	default:
		// exited / dead / removing / unknown
		return '○', theme.Error, "stopped"
	}
}

// ----- dependency probe (unchanged from previous round) ----------------------

type checkStatus int

const (
	statusPending checkStatus = iota
	statusRunning
	statusPass
	statusFail
)

type check struct {
	label  string
	status checkStatus
	detail string
	hint   string
	runFn  func() (ok bool, detail string, hint string)
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
	return false, "", "Export MEMQL_MASTER_KEY in your shell (cockpit added an `export` line to your ~/.bashrc / ~/.zshrc during first-launch setup -- start a new shell so it's picked up)."
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
		return false, "decrypt failed", "The key in your environment doesn't open the envelope on disk. Either you're using the wrong key, or the envelope was sealed under a different key -- re-create the envelope, or update MEMQL_MASTER_KEY."
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
