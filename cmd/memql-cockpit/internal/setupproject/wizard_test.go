package setupproject

import (
	"testing"

	"github.com/gdamore/tcell/v2"
)

// typeRunes feeds each rune of s to the model as a KeyRune event.
func typeRunes(m *wizModel, s string) {
	for _, r := range s {
		m.handleKey(tcell.NewEventKey(tcell.KeyRune, r, tcell.ModNone))
	}
}

func enter(m *wizModel) bool {
	return m.handleKey(tcell.NewEventKey(tcell.KeyEnter, 0, tcell.ModNone))
}

func TestWizardHappyPath(t *testing.T) {
	m := newWizModel(Config{})
	if m.step != stepIntro {
		t.Fatalf("initial step = %v, want stepIntro", m.step)
	}
	enter(m) // intro -> product
	if m.step != stepProduct {
		t.Fatalf("after intro step = %v, want stepProduct", m.step)
	}
	typeRunes(m, "acme")
	enter(m) // product -> org
	typeRunes(m, "acme-io")
	enter(m) // org -> domain
	typeRunes(m, "acme.local")
	enter(m) // domain -> engine
	typeRunes(m, "v1.2.3")
	enter(m) // engine -> confirm
	if m.step != stepConfirm {
		t.Fatalf("after inputs step = %v, want stepConfirm", m.step)
	}

	cont := m.handleKey(tcell.NewEventKey(tcell.KeyRune, 'y', tcell.ModNone))
	if cont {
		t.Fatalf("confirm should end the wizard (return false)")
	}
	if m.outcome != OutcomeConfirmed {
		t.Fatalf("outcome = %v, want Confirmed", m.outcome)
	}

	cfg := m.config(Config{Dir: "/tmp/ws", DryRun: true})
	if cfg.Product != "acme" || cfg.ProductOrg != "acme-io" || cfg.Domain != "acme.local" || cfg.EngineVersion != "v1.2.3" {
		t.Errorf("collected config wrong: %+v", cfg)
	}
	// Non-interactive fields on the seed survive.
	if cfg.Dir != "/tmp/ws" || !cfg.DryRun {
		t.Errorf("seed fields not preserved: %+v", cfg)
	}
}

func TestWizardValidationBlocksAdvance(t *testing.T) {
	m := newWizModel(Config{})
	enter(m) // -> product
	typeRunes(m, "Bad_Name")
	enter(m) // invalid: should stay on product with an errMsg
	if m.step != stepProduct {
		t.Fatalf("invalid product advanced to %v, want stay on stepProduct", m.step)
	}
	if m.errMsg == "" {
		t.Fatalf("expected an errMsg for an invalid product")
	}
	// Fix it: clear the field and retype.
	for range "Bad_Name" {
		m.handleKey(tcell.NewEventKey(tcell.KeyBackspace2, 0, tcell.ModNone))
	}
	typeRunes(m, "good")
	enter(m)
	if m.step != stepOrg {
		t.Fatalf("after fixing product step = %v, want stepOrg", m.step)
	}
	if m.errMsg != "" {
		t.Errorf("errMsg not cleared after valid input: %q", m.errMsg)
	}
}

func TestWizardEmptyDomainKeepsDefault(t *testing.T) {
	m := newWizModel(Config{})
	enter(m) // -> product
	typeRunes(m, "acme")
	enter(m) // -> org
	typeRunes(m, "acme-io")
	enter(m) // -> domain (empty in the field; default applied only in config())
	if m.domain != "" {
		t.Fatalf("domain field seed = %q, want empty", m.domain)
	}
	enter(m) // domain -> engine (empty domain accepted; default kept)
	if m.step != stepEngine {
		t.Fatalf("step = %v, want stepEngine", m.step)
	}
	cfg := m.config(Config{})
	if cfg.Domain != defaultDomain {
		t.Errorf("domain = %q, want %q", cfg.Domain, defaultDomain)
	}
}

func TestWizardEscCancels(t *testing.T) {
	m := newWizModel(Config{})
	enter(m) // -> product
	typeRunes(m, "acme")
	cont := m.handleKey(tcell.NewEventKey(tcell.KeyEscape, 0, tcell.ModNone))
	if cont {
		t.Fatalf("Esc should end the wizard")
	}
	if m.outcome != OutcomeCanceled {
		t.Errorf("outcome = %v, want Canceled", m.outcome)
	}
}

func TestWizardEditFromConfirm(t *testing.T) {
	m := newWizModel(Config{Product: "acme", ProductOrg: "acme-io"})
	// Walk straight to confirm (fields seeded).
	enter(m) // intro -> product
	enter(m) // product -> org (seeded valid)
	enter(m) // org -> domain
	enter(m) // domain -> engine
	enter(m) // engine -> confirm
	if m.step != stepConfirm {
		t.Fatalf("step = %v, want stepConfirm", m.step)
	}
	m.handleKey(tcell.NewEventKey(tcell.KeyRune, 'e', tcell.ModNone))
	if m.step != stepProduct {
		t.Errorf("Edit should return to stepProduct, got %v", m.step)
	}
}
