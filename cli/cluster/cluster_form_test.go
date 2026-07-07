package cluster

import (
	"testing"

	"github.com/gdamore/tcell/v2"

	"github.com/znasllc-io/memql-cockpit/cli/config"
	"github.com/znasllc-io/memql-cockpit/cli/ui"
)

func TestComposeFromDomain(t *testing.T) {
	c := composeFromDomain("staging.copresent.ai")
	want := config.ClusterConfig{
		Name:        "staging-copresent-ai",
		DisplayName: "staging.copresent.ai",
		Domain:      "staging.copresent.ai",
		Endpoint:    "https://cockpit.staging.copresent.ai",
		Issuer:      "https://identity.staging.copresent.ai",
		ClientId:    "cockpit",
	}
	if c != want {
		t.Errorf("composeFromDomain mismatch:\n got %+v\nwant %+v", c, want)
	}
}

func TestDomainFromConfig(t *testing.T) {
	// Stored Domain wins.
	if got := domainFromConfig(config.ClusterConfig{Domain: "a.b.c", Endpoint: "https://cockpit.x.y"}); got != "a.b.c" {
		t.Errorf("stored domain not preferred: got %q", got)
	}
	// Derive from a conventional endpoint when Domain is absent.
	if got := domainFromConfig(config.ClusterConfig{Endpoint: "https://cockpit.staging.copresent.ai"}); got != "staging.copresent.ai" {
		t.Errorf("derive-from-endpoint: got %q, want staging.copresent.ai", got)
	}
	// Strip a trailing port too.
	if got := domainFromConfig(config.ClusterConfig{Endpoint: "https://cockpit.local.znas.io:443"}); got != "local.znas.io" {
		t.Errorf("derive-with-port: got %q, want local.znas.io", got)
	}
}

// TestAddFormComposesOnEnter drives the form the way the TUI does:
// type a domain, press Enter, and assert OnAdd receives the composed
// config -- no Host/Port/Issuer/ClientId entry required.
func TestAddFormComposesOnEnter(t *testing.T) {
	v := NewClustersView(ui.DefaultTheme())
	var got config.ClusterConfig
	var called bool
	v.OnAdd = func(c config.ClusterConfig) { got = c; called = true }

	// Open a fresh Add form.
	v.showAddForm = true
	v.addForm = addFormState{}

	for _, r := range "Staging.CoPresent.AI" { // mixed case -> lower-cased on input
		v.HandleEvent(tcell.NewEventKey(tcell.KeyRune, r, tcell.ModNone))
	}
	v.HandleEvent(tcell.NewEventKey(tcell.KeyEnter, 0, tcell.ModNone))

	if !called {
		t.Fatal("OnAdd was not invoked on Enter")
	}
	if v.showAddForm {
		t.Error("form stayed open after a successful save")
	}
	if got.Domain != "staging.copresent.ai" {
		t.Errorf("composed domain = %q, want staging.copresent.ai", got.Domain)
	}
	if got.Endpoint != "https://cockpit.staging.copresent.ai" || got.Issuer != "https://identity.staging.copresent.ai" || got.ClientId != "cockpit" {
		t.Errorf("composed URLs wrong: %+v", got)
	}
}

// TestAddFormRejectsBareLabel: a non-FQDN keeps the form open with an
// error instead of saving.
func TestAddFormRejectsBareLabel(t *testing.T) {
	v := NewClustersView(ui.DefaultTheme())
	v.OnAdd = func(config.ClusterConfig) { t.Fatal("OnAdd should not fire for an invalid domain") }
	v.showAddForm = true
	v.addForm = addFormState{}

	for _, r := range "localhost" {
		v.HandleEvent(tcell.NewEventKey(tcell.KeyRune, r, tcell.ModNone))
	}
	v.HandleEvent(tcell.NewEventKey(tcell.KeyEnter, 0, tcell.ModNone))

	if !v.showAddForm {
		t.Error("form closed despite an invalid domain")
	}
	if v.addForm.formError == "" {
		t.Error("expected an inline validation error")
	}
}
