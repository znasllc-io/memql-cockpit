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
	Tools      ToolDispatcher
	Apps       AppInventory
	Models     ModelInventory
	Discoverer *models.Discoverer
	Metrics    *Metrics
}

// Fleet is one supervisor process, N cluster streams.
//
// Shared: consent (caller-owned), metrics, policy/tools, model
// inventory / discoverer. Per home: Runner, appsession.Manager,
// modelcall.Manager, backup sweeper -- so one home's disconnect
// must not StopAll another home's in-flight work.
type Fleet struct {
	logger     *slog.Logger
	workers    WorkersFile
	policy     *tools.Policy
	policyPath string
	tools      ToolDispatcher
	apps       AppInventory
	modelsInv  ModelInventory
	discoverer *models.Discoverer
	metrics    *Metrics

	mu      sync.Mutex
	runners []*Runner
}

// NewFleet validates the registry and returns a supervisor. Call Run.
func NewFleet(opts FleetOptions) (*Fleet, error) {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if err := opts.Workers.ValidateRun(); err != nil {
		return nil, err
	}
	return &Fleet{
		logger:     opts.Logger,
		workers:    opts.Workers,
		policy:     opts.Policy,
		policyPath: opts.PolicyPath,
		tools:      opts.Tools,
		apps:       opts.Apps,
		modelsInv:  opts.Models,
		discoverer: opts.Discoverer,
		metrics:    opts.Metrics,
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

// Run connects every enabled home in parallel. One home failing (or
// reconnecting forever) does not cancel the others; only ctx cancel
// tears the supervisor down.
func (f *Fleet) Run(ctx context.Context) error {
	homes := f.workers.EnabledHomes()
	if len(homes) == 0 {
		return fmt.Errorf("fleet: no enabled homes")
	}

	var wg sync.WaitGroup
	for _, home := range homes {
		home := home
		cfg := f.workers.ConfigForHome(home)
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
			CheckWorkspace: f.policyCheckPath(),
		})
		// Per-home Calls so a disconnect on home A does not StopAll
		// generations serving home B on the shared GPU.
		calls := modelcall.NewManager(modelcall.Options{
			Logger:    homeLogger,
			Inventory: f.modelsInv,
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
		runner, err := NewRunner(Options{
			Logger:         homeLogger,
			Config:         cfg,
			Tools:          f.tools,
			Apps:           f.apps,
			Models:         f.modelsInv,
			Calls:          calls,
			Sessions:       sessions,
			Metrics:        f.metrics,
			InferenceServe: f.inferenceServe(),
			ModelPull:      pull,
		})
		if err != nil {
			return fmt.Errorf("fleet: home %s: %w", home.ID, err)
		}
		f.mu.Lock()
		f.runners = append(f.runners, runner)
		f.mu.Unlock()

		backups := backup.New(backup.Options{
			Logger:     homeLogger,
			StateDir:   cfg.StateDir,
			BaseURL:    backupBaseURL(cfg.ClusterURL),
			Bearer:     backupBearer(cfg.ClusterURL, f.logger),
			CheckPath:  f.policyBackupCheck(),
			HTTPClient: &http.Client{Timeout: 0},
		})
		wg.Add(1)
		go func() {
			defer wg.Done()
			homeLogger.Info("home stream starting",
				"cluster_url", cfg.ClusterURL,
				"name", cfg.Name,
			)
			go backups.Run(ctx, runner.RegistrationId)
			err := runner.Run(ctx)
			sessions.StopAll("home stream ended")
			if err != nil && ctx.Err() == nil {
				homeLogger.Error("home stream exited", "error", err)
			}
		}()
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-ctx.Done():
		<-done
		return ctx.Err()
	case <-done:
		// Every home returned without a cancel -- unusual (runners
		// reconnect forever). Treat as clean stop.
		return nil
	}
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
