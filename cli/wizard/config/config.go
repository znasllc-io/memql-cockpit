// Package config hosts the cockpit's Configuration screen -- the 4th
// splash option (#2110, Epic 7 / workstream 7.7). It loads the memql
// env-var registry (the manifest in component/genesis), merges it with
// the values currently held in the operator's ~/.memory/genesis.znas
// envelope, and lets the operator VIEW + EDIT those values in a
// single-panel TUI. Saving re-seals the envelope through
// component/genesis.Seal (the same seal path the first-launch wizard
// uses), so the manifest floor is re-validated and the operator's
// shell rc files stay in sync.
//
// Required vars (manifest entries that carry a non-empty Required node
// list) are listed first; optional vars follow. Secrets are masked
// with a reveal toggle.
//
// The screen is launch-only, reached from the splash and returning to
// it -- it never starts a cluster connection. It mirrors the wizard
// chrome of cli/wizard/genesis and cli/wizard/runlocal.
//
// 7.8 EXTENSION POINT (#2111): the master-key display / regenerate
// panel mounts here. The 'K' key + the stepMasterKey step below are
// reserved for it; the current build surfaces a "coming soon" notice
// so the keybinding is discoverable but inert. Do NOT build the
// regenerate flow in this file -- that's #2111's job, layered on top
// of this screen.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gdamore/tcell/v2"
	"github.com/znasllc-io/memql-cockpit/cli/ui"
	corgenesis "github.com/znasllc-io/memql/component/genesis"
	"github.com/znasllc-io/memql/component/secret"
)

// Choice is what the screen's Run returns when it terminates. Esc / B
// go back to the splash; Q / Ctrl+Q quit the whole app.
type Choice int

const (
	ChoiceBack Choice = iota // Esc / B -- return to the splash menu
	ChoiceQuit               // Q / Ctrl+Q / Ctrl+C -- exit the cockpit
)

// step is the interaction mode the screen is in right now.
type step int

const (
	stepList       step = iota // browsing the registry rows
	stepEdit                   // editing the selected row's value
	stepConfirmKey             // Seal generated a fresh master key (shouldn't happen on update; defensive)
	stepMasterKey              // RESERVED for 7.8 (#2111) master-key panel
)

// row is one merged registry entry: the manifest definition plus the
// value currently in the envelope (or the manifest default when the
// envelope doesn't carry it).
type row struct {
	entry    corgenesis.ManifestEntry
	value    string // current effective value (envelope value, else default)
	fromEnv  bool   // true when the value came from the envelope (vs. manifest default)
	required bool   // entry has a non-empty Required node list
	secret   bool   // entry is a secret (masked by default)
}

// state is the screen's full mutable state.
type state struct {
	screen *ui.Screen
	theme  ui.Theme

	step step

	rows     []row
	selected int               // index into rows
	revealed map[int]bool      // per-row reveal toggle for secrets
	edits    map[string]string // name -> edited value (overrides envelope value on save)

	// Underlying data used to rebuild the full EnvEntry set on save.
	manifest   *corgenesis.Manifest
	envEntries []corgenesis.EnvEntry // the envelope as last loaded
	envPath    string

	// Edit buffer + the row being edited.
	editBuf  string
	editName string

	// Status line feedback (load error, save result, etc.).
	status      string
	statusError bool

	// scroll offset for the row list.
	scroll int
}

// Run loads the registry + envelope and drives the screen until the
// operator goes back (Esc / B) or quits (Q / Ctrl+Q). Blocks polling
// events on screen.
func Run(screen *ui.Screen, theme ui.Theme) Choice {
	s := &state{
		screen:   screen,
		theme:    theme,
		step:     stepList,
		revealed: map[int]bool{},
		edits:    map[string]string{},
		envPath:  defaultGenesisPath(),
	}
	s.load()

	for {
		s.draw()
		ev := screen.PollEvent()
		switch ev := ev.(type) {
		case *tcell.EventInterrupt:
			// redraw next iteration
		case *tcell.EventResize:
			screen.Sync()
		case *tcell.EventKey:
			if ev.Key() == tcell.KeyCtrlC || ev.Key() == tcell.KeyCtrlQ {
				return ChoiceQuit
			}
			if done, choice := s.handleKey(ev); done {
				return choice
			}
		}
	}
}

// defaultGenesisPath resolves the envelope location, honoring the
// MEMQL_GENESIS_PATH override the genesis wizard + runlocal probe use.
func defaultGenesisPath() string {
	if p := strings.TrimSpace(os.Getenv("MEMQL_GENESIS_PATH")); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".memql", "genesis.znas")
	}
	return filepath.Join(home, ".memql", "genesis.znas")
}

// load reads the manifest registry + the current envelope values and
// builds the merged row set. Any failure lands in the status line but
// the screen still renders (the registry alone is useful even without
// a readable envelope).
func (s *state) load() {
	manifest, err := corgenesis.LoadManifest("")
	if err != nil {
		s.status = "Could not load env-var registry: " + err.Error()
		s.statusError = true
		return
	}
	s.manifest = manifest

	// Current values from the envelope. Best-effort: a missing /
	// unreadable envelope just means every row falls back to its
	// manifest default, and Save is blocked with a helpful message.
	values := map[string]string{}
	if entries, oerr := corgenesis.OpenFile(s.envPath); oerr == nil {
		s.envEntries = entries
		for _, e := range entries {
			values[e.Name] = e.Value
		}
	} else {
		s.status = "Envelope not loaded (" + oerr.Error() + ") -- showing registry defaults; Save is disabled until the envelope opens."
		s.statusError = true
	}

	// Track which manifest names are secrets via the Secrets vs.
	// Variables split (AllEntries flattens that away, so walk the two
	// lists directly).
	secretNames := map[string]bool{}
	for _, e := range manifest.Secrets {
		secretNames[e.Name] = true
	}

	rows := make([]row, 0, len(manifest.AllEntries()))
	for _, e := range manifest.AllEntries() {
		val, fromEnv := values[e.Name]
		if !fromEnv {
			val = e.Default
		}
		rows = append(rows, row{
			entry:    e,
			value:    val,
			fromEnv:  fromEnv,
			required: len(e.Required) > 0,
			secret:   secretNames[e.Name] || isSecretKind(e.Kind),
		})
	}

	// Required first, then optional; stable alphabetical within each
	// group so the list is predictable to scan.
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].required != rows[j].required {
			return rows[i].required // required (true) sorts before optional
		}
		return rows[i].entry.Name < rows[j].entry.Name
	})
	s.rows = rows
}

// isSecretKind reports whether a manifest Kind marks the entry as a
// secret. The manifest's secret kinds are oauth_secret / vendor_api_key;
// the authoritative signal is membership in the manifest Secrets list,
// but Kind is a useful backstop for secret-shaped variables.
func isSecretKind(kind string) bool {
	switch kind {
	case "oauth_secret", "vendor_api_key":
		return true
	default:
		return false
	}
}

// effectiveValue returns the value for a row, applying any pending
// edit on top of the loaded value.
func (s *state) effectiveValue(idx int) string {
	r := s.rows[idx]
	if v, ok := s.edits[r.entry.Name]; ok {
		return v
	}
	return r.value
}

// envelopeReady reports whether the envelope opened cleanly (a
// precondition for Save -- we re-seal the existing envelope's full
// entry set with edits applied).
func (s *state) envelopeReady() bool {
	return s.envEntries != nil
}

// handleKey dispatches a keystroke for the current step. Returns
// done=true (plus the Choice) when the screen should exit.
func (s *state) handleKey(ev *tcell.EventKey) (bool, Choice) {
	switch s.step {
	case stepList:
		return s.handleListKey(ev)
	case stepEdit:
		s.handleEditKey(ev)
		return false, ChoiceBack
	case stepConfirmKey:
		s.handleConfirmKey(ev)
		return false, ChoiceBack
	case stepMasterKey:
		// 7.8 (#2111) lands here. For now any key returns to the list.
		s.step = stepList
		return false, ChoiceBack
	}
	return false, ChoiceBack
}

func (s *state) handleListKey(ev *tcell.EventKey) (bool, Choice) {
	switch ev.Key() {
	case tcell.KeyEscape:
		return true, ChoiceBack
	case tcell.KeyUp:
		s.move(-1)
		return false, ChoiceBack
	case tcell.KeyDown:
		s.move(1)
		return false, ChoiceBack
	case tcell.KeyEnter:
		s.beginEdit()
		return false, ChoiceBack
	case tcell.KeyCtrlS:
		s.save()
		return false, ChoiceBack
	}
	switch ev.Rune() {
	case 'q', 'Q':
		return true, ChoiceQuit
	case 'b', 'B':
		return true, ChoiceBack
	case 'r', 'R':
		// Reveal toggle for the selected secret row.
		if len(s.rows) > 0 && s.rows[s.selected].secret {
			s.revealed[s.selected] = !s.revealed[s.selected]
		}
		return false, ChoiceBack
	case 'k', 'K':
		// 7.8 (#2111) entry point -- master-key panel. Reserved; the
		// step renders a "coming soon" notice today.
		s.step = stepMasterKey
		return false, ChoiceBack
	}
	return false, ChoiceBack
}

func (s *state) move(delta int) {
	if len(s.rows) == 0 {
		return
	}
	s.selected += delta
	if s.selected < 0 {
		s.selected = 0
	}
	if s.selected >= len(s.rows) {
		s.selected = len(s.rows) - 1
	}
}

func (s *state) beginEdit() {
	if len(s.rows) == 0 {
		return
	}
	s.step = stepEdit
	s.editName = s.rows[s.selected].entry.Name
	s.editBuf = s.effectiveValue(s.selected)
	s.status = ""
	s.statusError = false
}

func (s *state) handleEditKey(ev *tcell.EventKey) {
	switch ev.Key() {
	case tcell.KeyEscape:
		s.step = stepList
		s.editBuf = ""
		s.editName = ""
	case tcell.KeyEnter:
		// Commit the edit into the pending-edits map. We don't seal
		// here -- the operator batches edits and saves with Ctrl+S.
		s.edits[s.editName] = s.editBuf
		s.step = stepList
		s.status = "Edited " + s.editName + " (unsaved -- Ctrl+S to re-seal)"
		s.statusError = false
		s.editBuf = ""
		s.editName = ""
	case tcell.KeyBackspace, tcell.KeyBackspace2:
		if len(s.editBuf) > 0 {
			s.editBuf = s.editBuf[:len(s.editBuf)-1]
		}
	case tcell.KeyRune:
		r := ev.Rune()
		if r >= 0x20 && r != 0x7f {
			s.editBuf += string(r)
		}
	}
}

// save rebuilds the full EnvEntry set (the envelope's existing entries
// with the operator's edits applied, plus any newly-set entries that
// only had a manifest default), then re-seals via component/genesis.Seal.
// Seal runs on a goroutine while the screen keeps polling so the TUI
// stays responsive, mirroring the genesis wizard's runSeal pattern.
func (s *state) save() {
	if len(s.edits) == 0 {
		s.status = "No unsaved edits."
		s.statusError = false
		return
	}
	if !s.envelopeReady() {
		s.status = "Cannot save: the envelope did not open (need MEMQL_MASTER_KEY in your environment)."
		s.statusError = true
		return
	}
	if strings.TrimSpace(os.Getenv(secret.EnvMasterKey)) == "" {
		s.status = "Cannot save: " + secret.EnvMasterKey + " is not set in this shell."
		s.statusError = true
		return
	}

	entries := s.buildEntriesForSeal()

	type sealOutcome struct {
		res *corgenesis.SealResult
		err error
	}
	outcomeCh := make(chan sealOutcome, 1)
	confirmCh := make(chan bool, 1)
	masterKey := strings.TrimSpace(os.Getenv(secret.EnvMasterKey))

	go func() {
		res, err := corgenesis.Seal(corgenesis.SealOptions{
			Entries:       entries,
			Manifest:      s.manifest,
			OutPath:       s.envPath,
			SourceEnvPath: "", // editing an existing envelope, not a source .env
			SyncShellRCs:  true,
			MasterKey:     masterKey,
			// Updating an existing envelope never generates a fresh key,
			// so ConfirmMasterKey shouldn't fire -- but wire it defensively
			// the same way the genesis wizard does.
			ConfirmMasterKey: func(masterKeyHex string) (bool, error) {
				s.step = stepConfirmKey
				s.screen.PostEvent(tcell.NewEventInterrupt(nil))
				return <-confirmCh, nil
			},
		})
		outcomeCh <- sealOutcome{res, err}
		s.screen.PostEvent(tcell.NewEventInterrupt(nil))
	}()

	// Drive the event loop while Seal runs so the panel can repaint
	// and the (defensive) confirm step can be answered.
	for {
		select {
		case out := <-outcomeCh:
			s.applySealResult(out.res, out.err, confirmCh)
			return
		default:
		}

		s.draw()
		ev := s.screen.PollEvent()
		switch ev := ev.(type) {
		case *tcell.EventInterrupt:
		case *tcell.EventResize:
			s.screen.Sync()
		case *tcell.EventKey:
			if ev.Key() == tcell.KeyCtrlC || ev.Key() == tcell.KeyCtrlQ {
				select {
				case confirmCh <- false:
				default:
				}
				s.status = "Save canceled."
				s.statusError = true
				s.step = stepList
				return
			}
			if s.step == stepConfirmKey {
				switch ev.Rune() {
				case 'y', 'Y':
					select {
					case confirmCh <- true:
					default:
					}
				case 'n', 'N':
					select {
					case confirmCh <- false:
					default:
					}
				}
				if ev.Key() == tcell.KeyEscape {
					select {
					case confirmCh <- false:
					default:
					}
				}
			}
		}
	}
}

func (s *state) applySealResult(res *corgenesis.SealResult, err error, confirmCh chan bool) {
	// Drain the confirm channel so the goroutine never blocks on exit.
	select {
	case confirmCh <- false:
	default:
	}
	s.step = stepList
	if err != nil {
		s.status = "Save failed: " + err.Error()
		s.statusError = true
		return
	}
	// Re-load from the freshly-sealed envelope so the row values + the
	// fromEnv markers reflect what's now on disk, and clear pending edits.
	n := 0
	if res != nil {
		n = res.EntriesWritten
	}
	s.edits = map[string]string{}
	s.load()
	s.status = fmt.Sprintf("Sealed envelope at %s (%d entries).", s.envPath, n)
	s.statusError = false
}

// buildEntriesForSeal returns the full EnvEntry set to seal: every
// entry currently in the envelope (with edits applied), followed by
// any edited entry that wasn't in the envelope yet (e.g. an operator
// set a value for a row that only carried a manifest default).
func (s *state) buildEntriesForSeal() []corgenesis.EnvEntry {
	out := make([]corgenesis.EnvEntry, 0, len(s.envEntries)+len(s.edits))
	seen := map[string]bool{}
	for _, e := range s.envEntries {
		if v, ok := s.edits[e.Name]; ok {
			e.Value = v
		}
		seen[e.Name] = true
		out = append(out, e)
	}
	// Edits to names not yet in the envelope -> append as new entries.
	for name, v := range s.edits {
		if seen[name] {
			continue
		}
		out = append(out, corgenesis.EnvEntry{Name: name, Value: v})
	}
	return out
}

func (s *state) handleConfirmKey(ev *tcell.EventKey) {
	// Confirm-key answering is handled inside save()'s inner loop; this
	// path only runs if a stray key arrives at stepConfirmKey outside
	// that loop. Treat any key as a return to the list.
	s.step = stepList
}
