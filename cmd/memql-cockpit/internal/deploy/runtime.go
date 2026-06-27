package deploy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/component/automations/steps"
	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/component/memql"
)

// ErrAutomationNotFound is returned by Resolve / Run when the requested
// automation is not present in the embedded deployment bundle. The deploy
// command special-cases it: deployEngineCluster (I10 / memql#2224) does not
// exist yet, so `deploy --env` reports a clear "blocked until I10" message
// instead of a bare resolution error.
var ErrAutomationNotFound = errors.New("automation not found in embedded bundle")

// RunRequest parameterizes one embedded-runtime automation invocation.
type RunRequest struct {
	Automation string
	Owner      string
	Input      map[string]any
	DryRun     bool
}

// RunResult is the outcome of an embedded-runtime invocation.
type RunResult struct {
	Resolved    bool   // the automation was found + compiled from the bundle
	Executed    bool   // the embedded executor was invoked
	Status      string // automation execution status (when Executed)
	ExecutionID string
	// ExecError carries a non-fatal automation-level error string (e.g. a
	// step failure) so the caller can record it in the audit trail.
	ExecError string
}

// Runtime is the cockpit's view of the embedded engine automation runtime.
// It is an interface so the command layer (role gate + audit + version pin)
// is unit-testable against a fake, and so the real engine-backed
// implementation can be swapped for the fuller capability surface that
// lands with I13 (memql#2220) without touching the command code.
type Runtime interface {
	// Resolve loads the deployment bundle and resolves the named automation,
	// proving the embedding without side effects. Returns ErrAutomationNotFound
	// (wrapped) when the name is absent from the bundle.
	Resolve(name string) error
	// Run resolves then executes the named automation via the embedded engine
	// executor.
	Run(ctx context.Context, req RunRequest) (RunResult, error)
}

// embeddedRuntime is the real Runtime: it constructs an in-process memQL
// engine + automation loader + executor from the memql module the cockpit
// already depends on, exactly as app bootstrap wires the MCP run_automation
// runner (app/mcp_automation_runner.go). The engine carries no database
// (memql.New(nil)): bundle resolution and the action/logic/event path run
// in-process, while DB-backed mutation steps no-op.
//
// NOTE (I10 / I13, memql#2224 / #2220): the deployment automations reach the
// target cluster through capability actions that resolve to the cockpit/runner
// surface. That surface — and a fully-initialized engine for durable
// deployment-record mutations — is provided by I13. Until then this runtime
// proves the embedding (load + resolve + invoke the executor) but cannot run
// a DB-backed deployment automation to completion. See HandleDeploy.
type embeddedRuntime struct {
	logger *slog.Logger

	once     sync.Once
	initErr  error
	loader   *automations.Loader
	executor *automations.Executor
}

// NewEmbeddedRuntime returns the real engine-backed Runtime.
func NewEmbeddedRuntime(logger *slog.Logger) Runtime {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(stderrSink{}, &slog.HandlerOptions{Level: slog.LevelWarn}))
	}
	return &embeddedRuntime{logger: logger}
}

// stderrSink is a tiny io.Writer fallback so a nil logger doesn't panic; the
// real callers always pass a configured logger.
type stderrSink struct{}

func (stderrSink) Write(p []byte) (int, error) { return len(p), nil }

func (e *embeddedRuntime) init() error {
	e.once.Do(func() {
		engine, err := memql.New(nil)
		if err != nil {
			e.initErr = fmt.Errorf("construct embedded engine: %w", err)
			return
		}
		bus := events.NewBus()
		engine.SetEventBus(bus)

		e.loader = automations.NewLoader(automations.LoaderOptions{
			Logger:   e.logger,
			Registry: concept.DefaultRegistry(),
		})
		e.executor = automations.NewExecutor(automations.ExecutorOptions{
			Logger:       e.logger,
			Engine:       engine,
			EventBus:     bus,
			StepRegistry: steps.NewRegistry(),
		})
	})
	return e.initErr
}

func (e *embeddedRuntime) Resolve(name string) error {
	if err := e.init(); err != nil {
		return err
	}
	auto, err := e.loader.LoadByName(strings.TrimSpace(name))
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			return fmt.Errorf("%w: %q", ErrAutomationNotFound, name)
		}
		return err
	}
	if auto == nil {
		return fmt.Errorf("%w: %q", ErrAutomationNotFound, name)
	}
	return nil
}

func (e *embeddedRuntime) Run(ctx context.Context, req RunRequest) (RunResult, error) {
	if err := e.init(); err != nil {
		return RunResult{}, err
	}
	name := strings.TrimSpace(req.Automation)
	auto, err := e.loader.LoadByName(name)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			return RunResult{}, fmt.Errorf("%w: %q", ErrAutomationNotFound, name)
		}
		return RunResult{}, err
	}
	res := RunResult{Resolved: true}

	// TODO(I10/I13, memql#2224/#2220): dry-run should route through the
	// engine Gate-2 sandbox (memql.RunBundleDryRun) once the deployment
	// bundle + a fully-initialized engine are wired. For now a dry run
	// stops at resolution — it has proven the bundle loads and the
	// automation compiles, with zero side effects.
	if req.DryRun {
		return res, nil
	}

	ev := &events.Event{
		Topic:     "cockpit.deploy." + name,
		Kind:      events.KindNodeCreated,
		Payload:   req.Input,
		Timestamp: time.Now().UTC(),
	}
	owner := req.Owner
	if owner == "" {
		owner = "cockpit"
	}
	execn, execErr := e.executor.ExecuteWithEvent(ctx, auto, owner, ev)
	res.Executed = true
	if execErr != nil {
		// A step/engine error is reported up; the command records it in the
		// audit trail rather than treating it as a cockpit crash.
		res.ExecError = execErr.Error()
		return res, nil
	}
	if execn != nil {
		res.Status = string(execn.Status)
		res.ExecutionID = execn.ID
		res.ExecError = execn.Error
	}
	return res, nil
}
