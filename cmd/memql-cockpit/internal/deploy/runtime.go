package deploy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/component/automations/steps"
	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/component/memql"
)

// ErrAutomationNotFound is returned by Resolve / Run when the requested
// automation is not present in the embedded deployment bundle. Since I10
// (memql#2224) landed, deployEngineCluster IS in the bundle, so for the deploy
// command this is a genuine error (a typo'd / removed automation), not the old
// "blocked until I10" no-op — it is reported as a hard error like any other
// `run` of a missing automation.
var ErrAutomationNotFound = errors.New("automation not found in embedded bundle")

// noEngineDBMarker is the substring the embedded engine surfaces when a step
// reaches a DB-backed read/write but the cockpit's in-process engine carries
// no database (memql.New(nil)). The deploy command detects it to report an
// honest "owner-gated: needs a live engine DB" outcome instead of a crash.
const noEngineDBMarker = "memory engine not initialized"

// isNoEngineDB reports whether an automation step error is the embedded
// engine's no-database condition.
func isNoEngineDB(s string) bool { return strings.Contains(s, noEngineDBMarker) }

// RunRequest parameterizes one embedded-runtime automation invocation.
type RunRequest struct {
	Automation string
	Owner      string
	Input      map[string]any
	DryRun     bool
	// Topic overrides the triggering-event topic the executor sees. The deploy
	// command sets it to the automation's declared trigger ("deploy.requested")
	// so deployEngineCluster runs exactly as an event-driven deploy would;
	// empty falls back to a synthetic "cockpit.deploy.<name>" topic.
	Topic string
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
// implementation can be swapped for the fuller capability surface tracked
// in znasllc-io/memql#2377 (D1) without touching the command code.
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
// D1 (znasllc-io/memql#2377): when MEMQL_DATABASE_DSN is set the engine boots
// DB-BACKED (LoadUnifiedConcepts -> MemoryNodesDatabase migrate-on-start ->
// New(BunDB) -> Init) and the deployment lifecycle mutations
// (createDeployment / updateDeploymentStatus) persist for real, with the
// owner identity stamped on the writes. Without the DSN the engine carries no
// database and a DB-backed step surfaces the honest "memory engine not
// initialized" error (isNoEngineDB), which the deploy command reports as
// owner-gated -- see HandleDeploy. Capability actions remain dry-run by
// default either way (apply-mode plumbing is D2, memql#2378).
type embeddedRuntime struct {
	logger *slog.Logger

	once     sync.Once
	initErr  error
	loader   *automations.Loader
	executor *automations.Executor
	// dbBacked is true when MEMQL_DATABASE_DSN was set at init and the
	// engine carries a real database (D1, memql#2377): the deployment
	// lifecycle mutations (createDeployment / updateDeploymentStatus)
	// execute for real instead of surfacing "memory engine not
	// initialized".
	dbBacked bool
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
		var engine *memql.MemQLEngine
		var err error
		// D1 (memql#2377): when MEMQL_DATABASE_DSN is set (the same env the
		// engine reads everywhere; after `make up` the k3d postgres is the
		// development target), boot the engine DB-BACKED via the blessed
		// recipe (LoadUnifiedConcepts -> MemoryNodesDatabase migrate-on-start
		// -> New(BunDB) -> Init) so the deployment lifecycle mutations
		// execute for real. Without the DSN the prior no-DB behavior is
		// unchanged: a DB-backed step surfaces the honest "memory engine not
		// initialized" error and the deploy command reports it owner-gated.
		// Env-agnostic by construction -- only the DSN varies per env.
		if dsn := strings.TrimSpace(os.Getenv("MEMQL_DATABASE_DSN")); dsn != "" {
			if _, cerr := memql.LoadUnifiedConcepts(nil); cerr != nil {
				e.initErr = fmt.Errorf("deploy runtime: load unified concepts: %w", cerr)
				return
			}
			mnd, merr := concept.NewMemoryNodesDatabase()
			if merr != nil {
				e.initErr = fmt.Errorf("deploy runtime: construct database: %w", merr)
				return
			}
			mnd.Start(context.Background())
			select {
			case <-mnd.Ready():
			case <-time.After(60 * time.Second):
				e.initErr = fmt.Errorf("deploy runtime: database not ready within 60s (MEMQL_DATABASE_DSN set but unreachable, or migrations stuck)")
				return
			}
			db := mnd.BunDB()
			if db == nil {
				e.initErr = fmt.Errorf("deploy runtime: database ready but BunDB is nil")
				return
			}
			engine, err = memql.New(db)
			if err != nil {
				e.initErr = fmt.Errorf("construct embedded engine (db-backed): %w", err)
				return
			}
			if ierr := engine.Init(concept.DefaultRegistry()); ierr != nil {
				e.initErr = fmt.Errorf("deploy runtime: engine init: %w", ierr)
				return
			}
			e.dbBacked = true
			e.logger.Info("deploy runtime: engine is DB-backed; lifecycle mutations will persist")
		} else {
			engine, err = memql.New(nil)
			if err != nil {
				e.initErr = fmt.Errorf("construct embedded engine: %w", err)
				return
			}
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

	topic := strings.TrimSpace(req.Topic)
	if topic == "" {
		topic = "cockpit.deploy." + name
	}
	ev := &events.Event{
		Topic:     topic,
		Kind:      events.KindNodeCreated,
		Payload:   req.Input,
		Timestamp: time.Now().UTC(),
	}
	owner := req.Owner
	if owner == "" {
		owner = "cockpit"
	}
	if e.dbBacked {
		// The lifecycle mutations resolve their actor from the context.
		// The deploy command is owner-gated upstream (HandleDeploy
		// authorize), so stamping the owner identity here is consistent
		// -- it is what lands on createdBy/triggered records.
		ctx = auth.ContextWithAccess(ctx, &auth.AccessContext{UserId: owner, Role: auth.RoleOwner})
		ctx = auth.ContextWithToken(ctx, &auth.TokenInfo{Subject: owner})
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
