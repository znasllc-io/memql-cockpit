package worker

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/znasllc-io/memql-cockpit/internal/worker/hardware"
	"github.com/znasllc-io/memql-cockpit/internal/worker/models"
	"github.com/znasllc-io/memql-cockpit/internal/worker/tools"
)

func TestEnsureMachineIDPersists(t *testing.T) {
	dir := t.TempDir()
	first, err := EnsureMachineID(dir)
	if err != nil || first == "" {
		t.Fatalf("first: %q %v", first, err)
	}
	second, err := EnsureMachineID(dir)
	if err != nil || second != first {
		t.Fatalf("second = %q, want %q (%v)", second, first, err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, machineIDFile))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(raw); got != first+"\n" {
		t.Fatalf("file = %q", got)
	}
}

func TestBuildRegisterCarriesMachineId(t *testing.T) {
	dir := t.TempDir()
	register := buildRegister(Config{
		Name:         "mbp",
		Capabilities: []string{"HEADLESS"},
		StateDir:     dir,
		Labels:       map[string]string{"os": "darwin"},
		Concurrency:  map[string]uint32{"HEADLESS": 1},
	}, nil, models.Inventory{}, hardware.Inventory{}, tools.ServeOwner)
	if register.Labels[LabelMachineId] == "" {
		t.Fatal("Register must carry machineId label")
	}
	again := buildRegister(Config{
		Name:         "mbp",
		Capabilities: []string{"HEADLESS"},
		StateDir:     dir,
		Concurrency:  map[string]uint32{"HEADLESS": 1},
	}, nil, models.Inventory{}, hardware.Inventory{}, tools.ServeOwner)
	if again.Labels[LabelMachineId] != register.Labels[LabelMachineId] {
		t.Fatalf("machineId must be stable across Registers: %q vs %q", again.Labels[LabelMachineId], register.Labels[LabelMachineId])
	}
}
