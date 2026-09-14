package worker

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"

	"github.com/znasllc-io/memql-cockpit/internal/worker/appsession"
	"github.com/znasllc-io/memql-cockpit/internal/worker/backup"
	"github.com/znasllc-io/memql-cockpit/internal/worker/modelcall"
	"github.com/znasllc-io/memql-cockpit/internal/worker/models"
	"github.com/znasllc-io/memql-cockpit/internal/worker/tools"
)

// FleetOptions wires the shared machine-side singletons into one
// supervisor that fans out a Runner per enabled home.
type FleetOptions struct {
	Logger     *slog.Logger
	Workers    WorkersFile
	Policy     *tools.Policy
	PolicyPath string
	// ToolsFor builds the dispatcher one home's stream serves tool calls
	// through. PER HOME, because the consent gate inside it is per home
	// (memql-cockpit#433): a window opened for one cluster must admit
	// nothing another cluster dispatches.
	ToolsFor   func(homeID string) ToolDispatcher
	Apps       AppInventory
	Models     ModelInventory
	Discoverer *models.Discoverer
	Metrics    *Metrics
	// Limiter is the machine's model concurrency ceiling, shared by every
	// home (memql-cockpit#432). Nil builds one for this fleet.
	Limiter *modelcall.Limiter
}

// Fleet is one supervisor process, N cluster streams.
//
// Shared: metrics, policy, model inventory / discoverer, and the model
// concurrency ceiling -- the ceiling is a claim about this machine's
// hardware, which every cluster was told the same thing about. Per home:
// Runner, appsession.Manager, modelcall.Manager, backup sweeper, consent
// window -- so one home's disconnect must not StopAll another home's
// in-flight work, and one cluster's consent admits nothing for another.
type Fleet struct {
	logger     *slog.Logger
	workers    WorkersFile
	policy     *tools.Policy
	policyPath string
	toolsFor   func(homeID string) ToolDispatcher
	apps       AppInventory
	modelsInv  ModelInventory
	discoverer *models.Discoverer
	metrics    *Metrics
	limiter    *modelcall.Limiter

	mu      sync.Mutex
	runners []*Runner

	// newRunner is NewRunner, and a seam: a test swaps in one that fails
	// for one home, or one that hands the runner a scripted stream.
	newRunner func(Options) (*Runner, error)
}

// NewFleet validates the registry and returns a supervisor. Call Run.
func NewFleet(opts FleetOptions) (*Fleet, error) {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if err := opts.Workers.ValidateRun(); err != nil {
		return nil, err
	}
	limiter := opts.Limiter
	if limiter == nil {
		limiter = modelcall.NewLimiter()
	}
	return &Fleet{
		logger:     opts.Logger,
		workers:    opts.Workers,
		policy:     opts.Policy,
		policyPath: opts.PolicyPath,
		toolsFor:   opts.ToolsFor,
		apps:       opts.Apps,
		modelsInv:  opts.Models,
		discoverer: opts.Discoverer,
		metrics:    opts.Metrics,
		limiter:    limiter,
		newRunner:  NewRunner,
	}, nil
}

// RequestImmediateReadvertise fans out to every home stream so a
// SIGHUP / model-allow change becomes visible on all clusters.
func (f *Fleet) RequestImmediateReadvertise() {
	if f == nil {
		return
	}
	f.mu.Lock()
	runners := append([]*Runner(nil), f.runners...)
	f.mu.Unlock()
	for _, r := range runners {
		r.RequestImmediateReadvertise()
	}
}

// homeRun is one home, built and not yet started.
type homeRun struct {
	home     Home
	cfg      Config
	logger   *slog.Logger
	runner   *Runner
	sessions *appsession.Manager
	backups  *backup.Manager
}

// Run connects every enabled home in parallel, one stream per cluster
// (runnableHomes). One home failing (or reconnecting forever) does not
// cancel the others; only ctx cancel tears the supervisor down.
//
// EVERY HOME IS BUILT BEFORE ANY IS STARTED (memql-cockpit#433). A home
// that cannot be built is a configuration error, and finding it after
// its siblings were already connected used to return from Run with those
// siblings still running -- streams, backup sweepers and all -- under a
// supervisor that had reported itself finished. Built first, a bad home
// fails the fleet before anything exists to leak. Every goroutine Run
// starts runs under a context Run cancels on its way out, and is joined
// before Run returns; what those start in turn -- a generation, an app
// session's process -- is cancelled with them and ends on its own.
func (f *Fleet) Run(ctx context.Context) error {
	homes, duplicates := f.workers.runnableHomes()
	if len(homes) == 0 {
		return fmt.Errorf("fleet: no enabled homes")
	}
	for _, d := range duplicates {
		f.logger.Warn(duplicateHomeSentence(d), "home", d.Home.ID)
	}

	// One machine id for every home, resolved before any of them
	// connects: two homes minting at once would register this machine
	// under two ids, which is the duplicate the id exists to prevent
	// (memql-cockpit#430).
	machineID, err := ResolveMachineID(f.workers.machineRoot())
	if err != nil {
		f.logger.Warn("machine id unavailable; registering without one", "error", err)
	}

	runs := make([]homeRun, 0, len(homes))
	for _, home := range homes {
		run, err := f.buildHome(home, machineID)
		if err != nil {
			return fmt.Errorf("fleet: home %s: %w", home.ID, err)
		}
		runs = append(runs, run)
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	f.mu.Lock()
	for _, run := range runs {
		f.runners = append(f.runners, run.runner)
	}
	f.mu.Unlock()

	var wg sync.WaitGroup
	for _, run := range runs {
		run := run
		wg.Add(2)
		go func() {
			defer wg.Done()
			run.backups.Run(runCtx, run.runner.RegistrationId)
		}()
		go func() {
			defer wg.Done()
			run.logger.Info("home stream starting",
				"cluster_url", run.cfg.ClusterURL,
				"name", run.cfg.Name,
			)
			err := run.runner.Run(runCtx)
			run.sessions.StopAll("home stream ended")
			if err != nil && runCtx.Err() == nil {
				run.logger.Error("home stream exited", "error", err)
			}
		}()
	}

	// Every home's stream returning without a cancel is unusual (runners
	// reconnect forever); the backup sweepers are then stopped with the
	// rest, by the cancel on the way out.
	streams := make(chan struct{})
	go func() {
		defer close(streams)
		for _, run := range runs {
			<-run.runner.closed
		}
	}()
	select {
	case <-ctx.Done():
	case <-streams:
	}
	cancel()
	wg.Wait()
	return ctx.Err()
}

// buildHome makes one home's runner and the per-home machinery around
// it, starting nothing.
func (f *Fleet) buildHome(home Home, machineID string) (homeRun, error) {
	cfg := f.workers.ConfigForHome(home)
	cfg.MachineID = machineID
	homeLogger := f.logger.With("home", home.ID)

	sessions := appsession.NewManager(appsession.Options{
		Logger:     homeLogger,
		StateDir:   cfg.StateDir,
		ClusterURL: cfg.ClusterURL,
		Allowed: func(appID string) bool {
			if f.policy == nil {
				return false
			}
			for _, allowed := range f.policy.AppsAllow() {
				if strings.EqualFold(strings.TrimSpace(allowed), appID) {
					return true
				}
			}
			return false
		},
		// The owner's apps.levels (memql-cockpit#438). The method value is
		// safe on a nil policy: AppLevels answers "no entries", which is
		// the built-in table.
		Levels:         f.policy.AppLevels,
		CheckWorkspace: f.policyCheckPath(),
	})
	// Per-home Calls so a disconnect on home A does not StopAll
	// generations serving home B on the shared GPU -- over one Limiter,
	// so the two together cannot take more of that GPU than it has.
	calls := modelcall.NewManager(modelcall.Options{
		Logger:    homeLogger,
		Inventory: f.modelsInv,
		Limiter:   f.limiter,
	})
	var pull *ModelPullOptions
	if f.modelsInv != nil && f.policy != nil {
		ollamaBase := func() string { return "" }
		if f.discoverer != nil {
			ollamaBase = f.discoverer.ResolvedOllamaBaseURL
		}
		pull = &ModelPullOptions{
			PolicyPath:   f.policyPath,
			OllamaBase:   ollamaBase,
			PullAllowed:  f.policy.ModelsPullAllowed,
			ReloadPolicy: f.policy.Reload,
		}
	}
	var dispatcher ToolDispatcher
	if f.toolsFor != nil {
		dispatcher = f.toolsFor(home.ID)
	}
	newRunner := f.newRunner
	if newRunner == nil {
		newRunner = NewRunner
	}
	runner, err := newRunner(Options{
		Logger:         homeLogger,
		Config:         cfg,
		Tools:          dispatcher,
		Apps:           f.apps,
		Models:         f.modelsInv,
		Calls:          calls,
		Sessions:       sessions,
		Metrics:        f.metrics,
		InferenceServe: f.inferenceServe(),
		ModelPull:      pull,
	})
	if err != nil {
		return homeRun{}, err
	}
	backups := backup.New(backup.Options{
		Logger:     homeLogger,
		StateDir:   cfg.StateDir,
		BaseURL:    backupBaseURL(cfg.ClusterURL),
		Bearer:     backupBearer(cfg.ClusterURL, f.logger),
		CheckPath:  f.policyBackupCheck(),
		HTTPClient: &http.Client{Timeout: 0},
	})
	return homeRun{
		home:     home,
		cfg:      cfg,
		logger:   homeLogger,
		runner:   runner,
		sessions: sessions,
		backups:  backups,
	}, nil
}

func (f *Fleet) inferenceServe() func() string {
	if f.policy == nil {
		return nil
	}
	return f.policy.InferenceServe
}

func (f *Fleet) policyCheckPath() func(string) error {
	if f.policy == nil {
		return func(string) error { return nil }
	}
	return f.policy.CheckPath
}

func (f *Fleet) policyBackupCheck() func(string) error {
	if f.policy == nil {
		return func(string) error { return nil }
	}
	return f.policy.CheckBackupPath
}
