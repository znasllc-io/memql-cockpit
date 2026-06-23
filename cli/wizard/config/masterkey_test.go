package config

// Tests for the 7.8 (#2111) master-key panel: read-only display with a
// provenance note, a reveal toggle, a regenerate action, and -- the
// load-bearing invariant -- NO edit path for the key. The tests drive
// the production draw + key-handling paths against a
// tcell.NewSimulationScreen so the assertions are about what the
// operator actually sees and can press.

import (
	"os"
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"

	"github.com/znasllc-io/memql-cockpit/cli/ui"
	"github.com/znasllc-io/memql/component/genesis"
)

const (
	panelTestW = 120
	panelTestH = 40
)

// freshKey returns a freshly generated 64-hex master key. Generated at
// runtime (never a hardcoded literal) so secret scanners have nothing to flag.
func freshKey(t *testing.T) string {
	t.Helper()
	k, err := genesis.GenerateMasterKey()
	if err != nil {
		t.Fatalf("GenerateMasterKey: %v", err)
	}
	return k
}

func newMasterKeyState(t *testing.T) (*state, tcell.SimulationScreen) {
	t.Helper()
	sim := tcell.NewSimulationScreen("UTF-8")
	if err := sim.Init(); err != nil {
		t.Fatalf("simulation screen init: %v", err)
	}
	t.Cleanup(sim.Fini)
	sim.SetSize(panelTestW, panelTestH)
	sim.Clear()
	screen := ui.NewScreenFromTcell(sim)

	s := &state{
		screen:   screen,
		theme:    ui.DefaultTheme(),
		step:     stepMasterKey,
		revealed: map[int]bool{},
		edits:    map[string]string{},
		envPath:  "/tmp/does-not-exist/genesis.znas",
	}
	return s, sim
}

func snapshot(s *state, sim tcell.SimulationScreen) string {
	s.draw()
	sim.Show()
	cells, w, h := sim.GetContents()
	var b strings.Builder
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			c := cells[y*w+x]
			if len(c.Runes) == 0 {
				b.WriteByte(' ')
				continue
			}
			b.WriteRune(c.Runes[0])
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// TestMasterKey_MaskedByDefaultRevealToggle asserts the key is masked
// by default and the R key reveals it (reusing the screen's R reveal
// convention).
func TestMasterKey_MaskedByDefaultRevealToggle(t *testing.T) {
	key := freshKey(t)
	t.Setenv("MEMQL_MASTER_KEY", key)

	s, sim := newMasterKeyState(t)

	out := snapshot(s, sim)
	if strings.Contains(out, key) {
		t.Fatalf("master key shown in cleartext while masked:\n%s", out)
	}
	if !strings.Contains(out, maskGlyph) {
		t.Fatalf("masked key not rendered with mask glyph:\n%s", out)
	}

	// Press R -> reveal.
	s.handleMasterKeyKey(tcell.NewEventKey(tcell.KeyRune, 'R', tcell.ModNone))
	out = snapshot(s, sim)
	if !strings.Contains(out, key) {
		t.Fatalf("R did not reveal the key:\n%s", out)
	}

	// Press R again -> mask.
	s.handleMasterKeyKey(tcell.NewEventKey(tcell.KeyRune, 'R', tcell.ModNone))
	out = snapshot(s, sim)
	if strings.Contains(out, key) {
		t.Fatalf("second R did not re-mask the key:\n%s", out)
	}
}

// TestMasterKey_ProvenanceNote asserts the provenance note reflects
// where the key came from.
func TestMasterKey_ProvenanceNote(t *testing.T) {
	key := freshKey(t)
	t.Setenv("MEMQL_MASTER_KEY", key)

	s, sim := newMasterKeyState(t)

	s.keyProvenance = provenanceShellRC
	if out := snapshot(s, sim); !strings.Contains(out, "loaded from shell rc") {
		t.Fatalf("shell-rc provenance note missing:\n%s", out)
	}

	s.keyProvenance = provenanceGenerated
	if out := snapshot(s, sim); !strings.Contains(out, "generated this session") {
		t.Fatalf("generated provenance note missing:\n%s", out)
	}
}

// TestMasterKey_NoEditPath asserts there is no editable field for the
// key: typing printable runes never transitions into an edit step and
// never mutates a key buffer (there is none). Only R / G / B / Esc are
// meaningful.
func TestMasterKey_NoEditPath(t *testing.T) {
	key := freshKey(t)
	t.Setenv("MEMQL_MASTER_KEY", key)

	s, _ := newMasterKeyState(t)

	for _, r := range []rune{'a', '0', 'x', 'Z', '/', ' '} {
		s.handleMasterKeyKey(tcell.NewEventKey(tcell.KeyRune, r, tcell.ModNone))
		if s.step != stepMasterKey {
			t.Fatalf("rune %q moved off the display panel to step %d (no edit path allowed)", r, s.step)
		}
	}
	// Enter must not open an edit field either.
	s.handleMasterKeyKey(tcell.NewEventKey(tcell.KeyEnter, 0, tcell.ModNone))
	if s.step == stepEdit {
		t.Fatal("Enter opened an edit step on the master-key panel (no edit path allowed)")
	}
}

// TestMasterKey_RegenerateConfirmGate asserts G opens a yes/no confirm
// before any rotation (it rewrites the operator's shell rc), and N
// backs out without rotating. We need an open envelope for G to be
// live, so seal a throwaway one first.
func TestMasterKey_RegenerateConfirmGate(t *testing.T) {
	key := freshKey(t)
	t.Setenv("MEMQL_MASTER_KEY", key)
	t.Setenv("HOME", t.TempDir()) // isolate shell-rc sync away from the real home

	envPath := t.TempDir() + "/genesis.znas"
	if _, err := genesis.Seal(genesis.SealOptions{
		Entries:      []genesis.EnvEntry{{Name: "MEMQL_MASTER_KEY", Value: key}},
		OutPath:      envPath,
		SyncShellRCs: false,
		MasterKey:    key,
	}); err != nil {
		t.Fatalf("seed envelope: %v", err)
	}

	s, _ := newMasterKeyState(t)
	s.envPath = envPath
	// Mark the envelope as opened so envelopeReady() is true.
	s.envEntries = []genesis.EnvEntry{{Name: "MEMQL_MASTER_KEY", Value: key}}

	// G -> confirm gate.
	s.handleMasterKeyKey(tcell.NewEventKey(tcell.KeyRune, 'G', tcell.ModNone))
	if s.step != stepConfirmRegenerate {
		t.Fatalf("G did not open the regenerate confirm gate; step=%d", s.step)
	}

	// N -> back to the display panel, no rotation.
	s.handleConfirmRegenerateKey(tcell.NewEventKey(tcell.KeyRune, 'N', tcell.ModNone))
	if s.step != stepMasterKey {
		t.Fatalf("N did not return to the display panel; step=%d", s.step)
	}
}

// TestMasterKey_RotateReEncryptsEnvelope asserts the rotate actually
// re-encrypts the WHOLE envelope under a NEW key: after rotateMasterKey
// the in-process key changes, the new key opens the envelope, the OLD
// key no longer does, and every original entry survives. This is the
// "don't orphan the envelope" invariant.
func TestMasterKey_RotateReEncryptsEnvelope(t *testing.T) {
	oldKey := freshKey(t)
	t.Setenv("MEMQL_MASTER_KEY", oldKey)
	t.Setenv("HOME", t.TempDir()) // shell-rc sync writes here (no rc files -> no-op)

	envPath := t.TempDir() + "/genesis.znas"
	entries := []genesis.EnvEntry{
		{Name: "MEMQL_MASTER_KEY", Value: oldKey},
		{Name: "MEMQL_SOME_SETTING", Value: "keep-me"},
	}
	if _, err := genesis.Seal(genesis.SealOptions{
		Entries:      append([]genesis.EnvEntry(nil), entries...),
		OutPath:      envPath,
		SyncShellRCs: false,
		MasterKey:    oldKey,
	}); err != nil {
		t.Fatalf("seed envelope: %v", err)
	}

	s, _ := newMasterKeyState(t)
	s.envPath = envPath
	s.keyProvenance = provenanceShellRC
	s.envEntries = append([]genesis.EnvEntry(nil), entries...)

	newKey, err := s.rotateMasterKey(append([]genesis.EnvEntry(nil), entries...), oldKey)
	if err != nil {
		t.Fatalf("rotateMasterKey: %v", err)
	}
	if newKey == oldKey || newKey == "" {
		t.Fatalf("rotate did not produce a fresh key: %q", newKey)
	}

	// In-process key advanced to the new key.
	if got := s.currentMasterKey(); got != newKey {
		t.Fatalf("in-process key not advanced: got %q want %q", got, newKey)
	}

	// New key opens the rotated envelope and carries every entry.
	opened, err := genesis.OpenFile(envPath)
	if err != nil {
		t.Fatalf("new key cannot open rotated envelope: %v", err)
	}
	if v, ok := genesis.LookupEnv(opened, "MEMQL_SOME_SETTING"); !ok || v != "keep-me" {
		t.Fatalf("rotated envelope lost a non-key entry: %v / %q", ok, v)
	}
	if v, ok := genesis.LookupEnv(opened, "MEMQL_MASTER_KEY"); !ok || v != newKey {
		t.Fatalf("rotated envelope's in-envelope key not reconciled: %v / %q", ok, v)
	}

	// Old key must NO LONGER open the rotated envelope.
	t.Setenv("MEMQL_MASTER_KEY", oldKey)
	if _, err := genesis.OpenFile(envPath); err == nil {
		t.Fatal("old key still opens the rotated envelope -- rotation did not take")
	}

	// No temp file left behind.
	if _, err := os.Stat(envPath + ".rotate.tmp"); !os.IsNotExist(err) {
		t.Fatalf("rotate temp file was left on disk (stat err=%v)", err)
	}
}
