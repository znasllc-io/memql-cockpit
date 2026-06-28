// Package deploy implements the cockpit's `deploy` and `run` subcommands —
// the control-plane surface for epic memql#2212 (DevOps DSL deployment
// bundle). The cockpit embeds the engine automation runtime in-process and
// runs a named automation from OUTSIDE the target cluster, enforcing the
// dsl/deployment/specs.memql role gates and emitting an audit trail, with
// the procedure pinned by `cockpit version + bundle version`.
//
//	memql-cockpit deploy --env=<env> [--ref=<ver>]   run deployEngineCluster for an environment
//	memql-cockpit run <automation> [--input=<json>]  run a named automation via the embedded runtime
//
// Execution model (handoff "Execution model — the control plane"):
//
//	make {up,deploy} → memql-cockpit <cmd> → embedded engine runtime
//	  → pinned automation → logic/mutations/actions → capability scripts → cluster
//
// NOTE: deployEngineCluster (I10 / memql#2224) and the cockpit/runner
// capability surface (I13 / memql#2220) are not merged yet. This package is
// the command INFRASTRUCTURE: the role gate, audit trail, version pinning,
// embedded-runtime wiring, and `run` smoke path are live and proven against
// an existing engine automation; `deploy --env` resolves to the future
// deployEngineCluster automation by name and reports a clear blocked message
// until I10 lands. See the TODO in runtime.go and HandleDeploy.
package deploy

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
)

// deployAutomation is the name of the per-environment deployment automation
// the deploy command drives. It is resolved by name through the embedded
// runtime; the automation itself lands with I10 (memql#2224).
const deployAutomation = "deployEngineCluster"

// invocation is the fully-resolved parameters of one deploy / run call.
type invocation struct {
	command    string // "deploy" | "run"
	automation string
	env        string
	ref        string
	role       Role
	actor      string
	dryRun     bool
	input      map[string]any
	gate       Gate
}

// HandleDeploy implements `memql-cockpit deploy --env=<env> [--ref=<ver>]`.
func HandleDeploy(args []string, cockpitVersion string) int {
	inv := invocation{command: "deploy", automation: deployAutomation, input: map[string]any{}}
	var jsonInput string

	for i := 0; i < len(args); i++ {
		a := args[i]
		var val string
		var hasVal bool
		if eq := strings.IndexByte(a, '='); strings.HasPrefix(a, "--") && eq >= 0 {
			val, hasVal = a[eq+1:], true
			a = a[:eq]
		}
		next := func() (string, bool) {
			if hasVal {
				return val, true
			}
			if i+1 < len(args) {
				i++
				return args[i], true
			}
			return "", false
		}
		switch a {
		case "--env":
			inv.env, _ = next()
		case "--ref":
			inv.ref, _ = next()
		case "--role":
			v, _ := next()
			inv.role = ParseRole(v)
		case "--actor":
			inv.actor, _ = next()
		case "--input":
			jsonInput, _ = next()
		case "--dry-run":
			inv.dryRun = true
		case "--help", "-h":
			printDeployUsage()
			return 0
		default:
			fmt.Fprintf(os.Stderr, "ERROR: unknown flag %q\n\n", a)
			printDeployUsage()
			return 2
		}
	}

	if inv.env == "" {
		fmt.Fprintln(os.Stderr, "ERROR: --env is required (development | staging | production)")
		printDeployUsage()
		return 2
	}
	if err := applyInput(&inv, jsonInput); err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		return 2
	}
	inv.input["env"] = inv.env
	if inv.ref != "" {
		inv.input["ref"] = inv.ref
	}
	inv.gate = GateForward // forward deploy: requiresDeveloperOrAbove
	resolveActorRole(&inv)

	// Report the runner-surface credential readiness (I17 / memql#2228).
	// Redacted, never logs secrets. Non-fatal: a dry-run / no-op deploy runs
	// without creds; a live deploy only WARNS when the surface is incomplete
	// because the deployEngineCluster capability actions that consume these
	// creds land fully with I13 (memql#2220).
	surface := ResolveSurface(os.Getenv)
	fmt.Println(surface.Summary())
	if !inv.dryRun {
		if missing := surface.MissingRequired(); len(missing) > 0 {
			fmt.Fprintf(os.Stderr,
				"WARNING: runner surface incomplete; a live deploy will not reach the cluster until these are set:\n  - %s\n"+
					"Source them from CI secrets / Key Vault (see docs/deploy-runner.md). TODO(owner): becomes fatal once I13 (memql#2220) wires the runner capability surface.\n",
				strings.Join(missing, "\n  - "))
		}
	}

	return runInvocation(inv, cockpitVersion, NewEmbeddedRuntime(newLogger()), DefaultAuditor())
}

// HandleRun implements `memql-cockpit run <automation> [--input=<json>]`.
func HandleRun(args []string, cockpitVersion string) int {
	inv := invocation{command: "run", input: map[string]any{}}
	var jsonInput string

	for i := 0; i < len(args); i++ {
		a := args[i]
		var val string
		var hasVal bool
		if eq := strings.IndexByte(a, '='); strings.HasPrefix(a, "--") && eq >= 0 {
			val, hasVal = a[eq+1:], true
			a = a[:eq]
		}
		next := func() (string, bool) {
			if hasVal {
				return val, true
			}
			if i+1 < len(args) {
				i++
				return args[i], true
			}
			return "", false
		}
		switch {
		case a == "--role":
			v, _ := next()
			inv.role = ParseRole(v)
		case a == "--actor":
			inv.actor, _ = next()
		case a == "--input":
			jsonInput, _ = next()
		case a == "--dry-run":
			inv.dryRun = true
		case a == "--help" || a == "-h":
			printRunUsage()
			return 0
		case strings.HasPrefix(a, "-"):
			fmt.Fprintf(os.Stderr, "ERROR: unknown flag %q\n\n", a)
			printRunUsage()
			return 2
		default:
			if inv.automation != "" {
				fmt.Fprintf(os.Stderr, "ERROR: too many positional arguments (got %q after %q)\n", a, inv.automation)
				return 2
			}
			inv.automation = a
		}
	}

	if inv.automation == "" {
		fmt.Fprintln(os.Stderr, "ERROR: an automation name is required")
		printRunUsage()
		return 2
	}
	if err := applyInput(&inv, jsonInput); err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		return 2
	}
	inv.gate = GateForAutomation(inv.automation)
	resolveActorRole(&inv)

	return runInvocation(inv, cockpitVersion, NewEmbeddedRuntime(newLogger()), DefaultAuditor())
}

// runInvocation is the shared core: version pin → role gate (+audit) →
// embedded-runtime execute (+audit) → print. It is the single seam the unit
// tests drive against a fake Runtime + in-memory Auditor.
func runInvocation(inv invocation, cockpitVersion string, rt Runtime, auditor *Auditor) int {
	bundleVersion, err := BundleVersion()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: compute bundle version: %v\n", err)
		return 1
	}
	fmt.Println(VersionLine(cockpitVersion, bundleVersion))

	rec := AuditRecord{
		Command:        inv.command,
		Automation:     inv.automation,
		Env:            inv.env,
		Ref:            inv.ref,
		Actor:          inv.actor,
		Role:           inv.role,
		Gate:           inv.gate,
		CockpitVersion: cockpitVersion,
		BundleVersion:  bundleVersion,
		DryRun:         inv.dryRun,
	}

	// 1. Role gate — enforced BEFORE the embedded runtime is touched.
	if gateErr := Authorize(inv.gate, inv.role); gateErr != nil {
		rec.Decision = DecisionDenied
		rec.Reason = gateErr.Error()
		_ = auditor.Emit(rec)
		fmt.Fprintf(os.Stderr, "ERROR: permission denied: %v\n", gateErr)
		return 1
	}
	rec.Decision = DecisionAllowed

	// 2. Execute via the embedded engine automation runtime.
	res, runErr := rt.Run(context.Background(), RunRequest{
		Automation: inv.automation,
		Owner:      inv.actor,
		Input:      inv.input,
		DryRun:     inv.dryRun,
	})
	if runErr != nil {
		rec.Reason = runErr.Error()
		_ = auditor.Emit(rec)
		// deployEngineCluster (I10) is not merged: surface a clear,
		// actionable blocked message rather than a bare resolution error.
		if inv.command == "deploy" && errorsIsNotFound(runErr) {
			fmt.Fprintf(os.Stderr,
				"BLOCKED: the %q automation is not in the deployment bundle yet.\n"+
					"deploy --env is wired but is a no-op until I10 (memql#2224) ships the\n"+
					"per-environment deployment automations. The role gate, audit trail, and\n"+
					"version pin above all ran.\n", inv.automation)
			return 3
		}
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", runErr)
		return 1
	}

	rec.Status = res.Status
	rec.ExecutionID = res.ExecutionID
	if res.ExecError != "" {
		rec.Reason = res.ExecError
	}
	_ = auditor.Emit(rec)

	switch {
	case inv.dryRun && res.Resolved:
		fmt.Printf("OK (dry-run): automation %q resolved from the bundle; no side effects.\n", inv.automation)
	case res.Executed && res.ExecError == "":
		fmt.Printf("OK: automation %q executed (status=%s id=%s)\n", inv.automation, res.Status, res.ExecutionID)
	case res.Executed && res.ExecError != "":
		fmt.Printf("DONE (with errors): automation %q executed via the embedded runtime; step error: %s\n", inv.automation, res.ExecError)
	default:
		fmt.Printf("OK: automation %q resolved.\n", inv.automation)
	}
	return 0
}

// applyInput parses an optional --input JSON object into inv.input.
func applyInput(inv *invocation, jsonInput string) error {
	jsonInput = strings.TrimSpace(jsonInput)
	if jsonInput == "" {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(jsonInput), &m); err != nil {
		return fmt.Errorf("--input must be a JSON object: %w", err)
	}
	for k, v := range m {
		inv.input[k] = v
	}
	return nil
}

// resolveActorRole fills in the caller's role + identity from flags then
// environment. Role precedence: --role flag → MEMQL_COCKPIT_ROLE env →
// reader (deny-by-default). Actor precedence: --actor → MEMQL_COCKPIT_ACTOR
// → $USER.
//
// TODO(I13, memql#2220): derive the role from the active cluster identity /
// token claims via the cockpit auth layer instead of an explicit flag/env,
// once the runner control surface exposes it.
func resolveActorRole(inv *invocation) {
	if inv.role == "" {
		inv.role = ParseRole(os.Getenv("MEMQL_COCKPIT_ROLE"))
	}
	if inv.actor == "" {
		if v := strings.TrimSpace(os.Getenv("MEMQL_COCKPIT_ACTOR")); v != "" {
			inv.actor = v
		} else {
			inv.actor = strings.TrimSpace(os.Getenv("USER"))
		}
	}
}

func newLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
}

func errorsIsNotFound(err error) bool {
	return err != nil && strings.Contains(err.Error(), ErrAutomationNotFound.Error())
}

func printDeployUsage() {
	fmt.Println("Usage: memql-cockpit deploy --env=<env> [--ref=<ver>] [flags]")
	fmt.Println()
	fmt.Println("Deploy the engine cluster for an environment by running the pinned")
	fmt.Println("deployEngineCluster automation via the embedded engine runtime, from")
	fmt.Println("OUTSIDE the target cluster. Enforces spec(requiresDeveloperOrAbove).")
	fmt.Println()
	fmt.Println("Flags:")
	fmt.Println("  --env <env>     Target environment: development | staging | production (required)")
	fmt.Println("  --ref <ver>     Engine version / git ref to deploy")
	fmt.Println("  --role <role>   Caller role (or MEMQL_COCKPIT_ROLE); default reader (denied)")
	fmt.Println("  --actor <id>    Caller identity for the audit trail (or MEMQL_COCKPIT_ACTOR / $USER)")
	fmt.Println("  --input <json>  Extra JSON-object input merged into the automation input")
	fmt.Println("  --dry-run       Resolve + preview without executing")
	fmt.Println("  --help, -h      Show this help")
	fmt.Println()
	fmt.Println("Exit codes: 0 success · 1 error/denied · 2 usage · 3 blocked (deployEngineCluster pending I10)")
}

func printRunUsage() {
	fmt.Println("Usage: memql-cockpit run <automation> [flags]")
	fmt.Println()
	fmt.Println("Run a named automation via the embedded engine automation runtime,")
	fmt.Println("in-process. Enforces the deployment role gates (forward =")
	fmt.Println("requiresDeveloperOrAbove; rollback-shaped = requiresOwner) and emits an")
	fmt.Println("audit trail. Pins cockpit + bundle version for reproducibility.")
	fmt.Println()
	fmt.Println("Flags:")
	fmt.Println("  --role <role>   Caller role (or MEMQL_COCKPIT_ROLE); default reader (denied)")
	fmt.Println("  --actor <id>    Caller identity for the audit trail (or MEMQL_COCKPIT_ACTOR / $USER)")
	fmt.Println("  --input <json>  JSON-object input bound as the automation's trigger payload")
	fmt.Println("  --dry-run       Resolve + preview without executing")
	fmt.Println("  --help, -h      Show this help")
}
