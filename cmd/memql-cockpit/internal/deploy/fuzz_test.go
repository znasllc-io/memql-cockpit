package deploy

// Native Go fuzzing over the deploy command's untrusted inputs (Scorecard
// FuzzingID): the --input JSON object merge and the dry-run ladder. Run
// locally with: go test -fuzz=FuzzApplyInput ./cmd/memql-cockpit/internal/deploy/

import "testing"

func FuzzApplyInput(f *testing.F) {
	f.Add(`{"workdir":"/tmp/x","version":"local"}`)
	f.Add(`{"dryRun":false}`)
	f.Add(`{`)
	f.Add(``)
	f.Add(`[1,2]`)
	f.Add(`{"a":{"b":[{"c":null}]}}`)
	f.Fuzz(func(t *testing.T, jsonInput string) {
		inv := invocation{input: map[string]any{}}
		// Must never panic; errors are the contract for malformed input.
		_ = applyInput(&inv, jsonInput)
		_ = stampDryRunLadder(&inv)
	})
}
