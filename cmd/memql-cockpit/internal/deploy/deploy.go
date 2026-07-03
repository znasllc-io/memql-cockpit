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
// NOTE: deployEngineCluster (I10 / memql#2224) has landed, so `deploy --env`
// now RESOLVES + INVOKES it through the embedded runtime — the same path
// `run <automation>` uses — building the canonical `deploy.requested` event
// payload, enforcing the role gate, emitting the audit trail, and pinning the
// version. The cockpit/runner capability surface + a fully-initialized engine
// DB for the DB-backed deployment-record mutations land with I13 (memql#2220):
// until then the in-process engine carries no database, so a real deploy runs
// as far as the bare engine allows and reports an honest, owner-gated
// "needs a live engine DB" outcome rather than a no-op stub. See HandleDeploy.
package deploy

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"
)

// deployAutomation is the name of the per-environment deployment automation
// the deploy command drives. It is resolved by name through the embedded
// runtime; the automation landed with I10 (memql#2224).
const deployAutomation = "deployEngineCluster"

// deployRequestedTopic is the automation's declared trigger topic
// (@trigger(event="deploy.requested") in dsl/deployment/automations.memql).
// The deploy command emits the run under this topic so deployEngineCluster
// executes exactly as an event-driven deploy would.
const deployRequestedTopic = "deploy.requested"

// invocation is the fully-resolved parameters of one deploy / run call.
type invocation struct {
	command    string // "deploy" | "run"
	automation string
	env        string
	ref        string
	role       Role
	actor      string
	dryRun     bool
	apply      bool
	input      map[string]any
	topic      string // triggering-event topic for the embedded runtime (deploy path)
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
		case "--apply":
			inv.apply = true
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
	if err := stampDryRunLadder(&inv); err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		return 2
	}
	inv.gate = GateForward // forward deploy: requiresDeveloperOrAbove
	inv.topic = deployRequestedTopic
	resolveActorRole(&inv)
	applyDeployDefaults(&inv) // build the canonical deploy.requested payload

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

// stampDryRunLadder applies the D2 (memql#2378) three-rung ladder to a deploy
// invocation: --dry-run stops at RESOLUTION (nothing executes); the default
// EXECUTES the automation with every capability script in its own dry-run mode
// (the payload carries dryRun=true explicitly); --apply flips the payload
// field so the scripts perform their work. An explicit dryRun in --input wins.
// One documented switch, dry-run default everywhere.
func stampDryRunLadder(inv *invocation) error {
	if inv.apply && inv.dryRun {
		return fmt.Errorf("--apply and --dry-run are mutually exclusive")
	}
	if _, ok := inv.input["dryRun"]; !ok {
		inv.input["dryRun"] = !inv.apply
	}
	return nil
}

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

	// 2. Execute via the embedded engine automation runtime — the same path
	// `run <automation>` uses. deployEngineCluster (I10) is now in the bundle,
	// so the deploy command resolves + invokes it here.
	res, runErr := rt.Run(context.Background(), RunRequest{
		Automation: inv.automation,
		Owner:      inv.actor,
		Input:      inv.input,
		DryRun:     inv.dryRun,
		Topic:      inv.topic,
	})
	if runErr != nil {
		rec.Reason = runErr.Error()
		_ = auditor.Emit(rec)
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
	case res.Executed && isNoEngineDB(res.ExecError):
		// The automation resolved + ran, but the cockpit's in-process engine
		// carries no database, so the DB-backed deployment steps cannot
		// complete here. This is the expected, owner-gated condition until the
		// I13 (memql#2220) runner surface + a live engine DB are wired — report
		// it HONESTLY (not a no-op stub, not a crash). Role gate, audit, and
		// version pin above all ran.
		fmt.Printf("BLOCKED (owner-gated): automation %q resolved + invoked via the embedded runtime, "+
			"but the cockpit's in-process engine has no database — DB-backed deployment steps cannot "+
			"complete here. Run against a live engine DB / the I13 runner surface to deploy for real.\n",
			inv.automation)
	case res.Executed && res.ExecError != "":
		fmt.Printf("DONE (with errors): automation %q executed via the embedded runtime; step error: %s\n", inv.automation, res.ExecError)
	default:
		fmt.Printf("OK: automation %q resolved.\n", inv.automation)
	}
	return 0
}

// applyDeployDefaults fills the canonical deploy.requested event payload the
// deployEngineCluster automation reads (event.payload.*), without clobbering
// anything an operator passed via --input. The automation's contract is in
// dsl/deployment/automations.memql ("Deploy event taxonomy").
func applyDeployDefaults(inv *invocation) {
	setDefault := func(k string, v any) {
		if _, ok := inv.input[k]; !ok {
			inv.input[k] = v
		}
	}
	setDefault("environment", inv.env)
	if inv.ref != "" {
		setDefault("ref", inv.ref)
		setDefault("version", inv.ref)
	}
	if inv.actor != "" {
		setDefault("triggeredBy", inv.actor)
	}
	// deploymentId is the v1:cluster:deployment timeline id; one per invocation
	// unless the operator pins it (e.g. to append to an existing timeline).
	setDefault("deploymentId", fmt.Sprintf("dep-%s-%d", inv.env, time.Now().UTC().Unix()))
	setDefault("engineNodeTypes", defaultEngineNodeTypes())
}

// defaultEngineNodeTypes mirrors the canonical engine node-type list the deploy
// bundle builds + places (logic engineNodeTypes, dsl/deployment/logic.memql):
// engine mesh ONLY — no bff/copresent. An operator can override or subset it
// via --input engineNodeTypes for a single-node redeploy.
func defaultEngineNodeTypes() []string {
	return []string{"identity", "cognition", "voice", "agent", "planner", "workbench", "mcp", "voice-agent"}
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
	fmt.Println("  --apply         Perform the work: sets the payload dryRun=false so every capability script applies instead of reporting (default: scripts run in dry-run mode)")
	fmt.Println("  --help, -h      Show this help")
	fmt.Println()
	fmt.Println("Exit codes: 0 success/invoked · 1 error/denied · 2 usage")
	fmt.Println("A development deploy runs through the embedded runtime; DB-backed steps need a")
	fmt.Println("live engine DB / the I13 runner surface (owner-gated) and report honestly until then.")
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
