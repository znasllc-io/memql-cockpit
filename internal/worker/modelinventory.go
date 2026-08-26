package worker

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/znasllc-io/memql-cockpit/internal/worker/models"
	"github.com/znasllc-io/memql-cockpit/internal/worker/tools"
)

// ModelInventory reports which local models this machine offers, for
// Register (memql-cockpit#361).
//
// An interface rather than a concrete discoverer so the runner's tests can
// drive the wire shape without Ollama on the box, and so a build that
// wants no model reporting at all can pass nil.
type ModelInventory interface {
	Models(ctx context.Context) models.Inventory
}

// DefaultModelInventoryTTL bounds how stale a discovery result may be.
//
// WHY CACHE. Discovery is one HTTP call to list what is installed plus one
// per model to ask what it can do, and it is taken on every registration
// and on every refresh check. Uncached, a machine with ten models pulled
// would issue eleven requests a minute forever to answer a question whose
// answer changes when somebody runs `ollama pull`.
const DefaultModelInventoryTTL = 30 * time.Second

// policyModelInventory pairs the discoverer with the policy that gates it.
// The policy is read on every call rather than captured, so a SIGHUP that
// adds a model to models.allow is picked up by the next refresh.
type policyModelInventory struct {
	discoverer *models.Discoverer
	policy     *tools.Policy
	ttl        time.Duration

	mu     sync.Mutex
	cached models.Inventory
	at     time.Time
	now    func() time.Time
}

// NewModelInventory builds the reporter the worker runs with.
func NewModelInventory(policy *tools.Policy) ModelInventory {
	return &policyModelInventory{
		discoverer: &models.Discoverer{},
		policy:     policy,
		ttl:        DefaultModelInventoryTTL,
	}
}

func (p *policyModelInventory) clock() time.Time {
	if p.now != nil {
		return p.now()
	}
	return time.Now()
}

func (p *policyModelInventory) Models(ctx context.Context) models.Inventory {
	if p == nil || p.discoverer == nil {
		return models.Inventory{}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.at.IsZero() && p.clock().Sub(p.at) < p.ttl {
		return p.cached
	}
	p.cached = p.discoverer.Discover(ctx, models.Request{
		Allow:    p.policy.ModelsAllow(),
		Runtimes: p.policy.ModelRuntimes(),
	})
	p.at = p.clock()
	return p.cached
}

// -----------------------------------------------------------------------------
// The registration shape
// -----------------------------------------------------------------------------

// modelRegistration is everything an inventory contributes to Register.
type modelRegistration struct {
	// Labels are the `model:<id>` and `runtime:<kind>` entries. Nil when
	// this machine offers nothing.
	Labels map[string]string
	// Capability is models.Capability when anything is offered, empty
	// otherwise. A machine that advertises MODEL with no model labels
	// would be selected by the capability-level plan and then ruled out
	// by every narrowing, which reads in the refusal report as a machine
	// that ALMOST worked.
	Capability string
	// Concurrency is the machine-wide MODEL ceiling: the sum of the
	// advertised per-model caps, which is the most this machine could be
	// running if every model ran at its own limit. The serving side
	// enforces this same number, so the advertisement and the enforcement
	// agree by construction rather than by two people remembering to
	// update both.
	Concurrency uint32
}

// modelRegistrationFor derives the registration contribution.
//
// It NEVER emits `sharedInference`. That label is the owner's grant, read
// by the engine from operatorLabels alone -- deliberately not from the
// merged map, because `labels` is overwritten from Register on every
// reconnect, so an opt-in stored there would be granted by the machine
// rather than by its owner and revoked roughly whenever the lid closed.
// A cockpit that derived one would be claiming a permission it has no
// standing to give itself.
func modelRegistrationFor(inv models.Inventory) modelRegistration {
	labels := inv.Labels()
	if len(labels) == 0 {
		return modelRegistration{}
	}
	var total int
	for _, m := range inv.Advertised() {
		total += m.MaxConcurrent
	}
	if total <= 0 {
		total = 1
	}
	return modelRegistration{
		Labels:      labels,
		Capability:  models.Capability,
		Concurrency: uint32(total),
	}
}

// mergeModelLabels folds the derived model labels onto the operator's own
// label map without mutating it.
//
// The operator's labels win on a collision. That direction matters
// exactly once -- if somebody hand-wrote a `model:` label in worker.yaml,
// discovery must not silently overrule it, because the machine they were
// describing is the one they are standing next to.
func mergeModelLabels(operator map[string]string, derived map[string]string) map[string]string {
	if len(derived) == 0 {
		return operator
	}
	out := make(map[string]string, len(operator)+len(derived))
	for k, v := range derived {
		out[k] = v
	}
	for k, v := range operator {
		out[k] = v
	}
	return out
}

// withModelCapability returns capabilities plus MODEL, without mutating
// the caller's slice and without a duplicate.
func withModelCapability(capabilities []string, capability string) []string {
	if capability == "" {
		return capabilities
	}
	for _, c := range capabilities {
		if c == capability {
			return capabilities
		}
	}
	out := make([]string, len(capabilities), len(capabilities)+1)
	copy(out, capabilities)
	return append(out, capability)
}

// withModelConcurrency returns concurrency plus the MODEL entry, without
// mutating the caller's map.
func withModelConcurrency(concurrency map[string]uint32, capability string, n uint32) map[string]uint32 {
	if capability == "" || n == 0 {
		return concurrency
	}
	out := make(map[string]uint32, len(concurrency)+1)
	for k, v := range concurrency {
		out[k] = v
	}
	out[capability] = n
	return out
}

// advertisedFingerprint renders the advertised label set as a stable
// string, so the runner can tell "the model set changed" from "discovery
// ran again" without comparing maps by hand.
func advertisedFingerprint(labels map[string]string) string {
	if len(labels) == 0 {
		return ""
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b []byte
	for _, k := range keys {
		b = append(b, k...)
		b = append(b, '=')
		b = append(b, labels[k]...)
		b = append(b, '\n')
	}
	return string(b)
}
