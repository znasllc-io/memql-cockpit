package worker

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// machineIDFile is the persistent install id for this physical machine.
// The engine uses label machineId on Register to reclaim an existing
// registration when the operator re-pairs with a new worker token, instead of
// minting a duplicate row for the same Mac.
const machineIDFile = "machine-id"

// LabelMachineId matches component/worker.LabelMachineId on the engine.
const LabelMachineId = "machineId"

// EnsureMachineID returns a stable id for this install, creating
// <stateDir>/machine-id on first use. Empty stateDir falls back to the default.
func EnsureMachineID(stateDir string) (string, error) {
	if strings.TrimSpace(stateDir) == "" {
		stateDir = defaultStateDir()
	}
	path := filepath.Join(stateDir, machineIDFile)
	if data, err := os.ReadFile(path); err == nil {
		id := strings.TrimSpace(string(data))
		if id != "" {
			return id, nil
		}
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("machine-id: read %s: %w", path, err)
	}
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("machine-id: rand: %w", err)
	}
	id := hex.EncodeToString(raw)
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return "", fmt.Errorf("machine-id: mkdir: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(id+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("machine-id: write: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return "", fmt.Errorf("machine-id: rename: %w", err)
	}
	return id, nil
}
