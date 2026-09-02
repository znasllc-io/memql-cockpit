package backup

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"math/rand"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// The watched-folder backup: one folder on this machine, kept arriving in the
// Library (memql#4841, the cockpit half of epic memql#4783).
//
// ===========================================================================
// THE INVARIANT, RESTATED HERE BECAUSE THIS IS THE CODE THAT COULD BREAK IT
// ===========================================================================
// ONE-WAY, FOREVER. Nothing in this package reads the Library and writes this
// machine, and nothing anywhere deletes, moves or hides a copy because of
// something that happened at the origin. A file deleted here is FLAGGED there
// (`origin_gone`) and stays whole and downloadable -- because a backup whose
// copy follows the original into the bin is not a backup.
//
// Two-way sync and conflict resolution are refused deliberately. That is the
// complexity cliff this feature exists on the safe side of.
//
// ===========================================================================
// WHO DECIDES WHAT
// ===========================================================================
// The GRAPH says which folder, where it files, what to skip -- so a person can
// set a backup up from a browser on a different machine while this one is
// asleep. THIS MACHINE decides whether it will do it at all: every path is
// checked against policy.yaml's backup.roots before anything is read, and a
// refusal is REPORTED rather than silent, because a machine that quietly
// ignored a watch would be indistinguishable from one that was offline.

const (
	// DefaultSweepInterval is how often a watch is walked when nothing is
	// wrong. Five minutes is chosen against what a person notices: a file
	// saved and then looked for in the Library is worth waiting a few minutes
	// for, and a folder of video is not worth walking every thirty seconds.
	DefaultSweepInterval = 5 * time.Minute

	// verifyRefreshInterval is how often an UNCHANGED file's `synced` is
	// re-stamped so `linkCheckedAt` does not silently mean "as of three weeks
	// ago".
	//
	// A DAY, not a sweep. A state CHANGE is reported immediately; this paces
	// only the repeats. Without it, a watched folder of ten thousand files
	// would be ten thousand graph writes every five minutes, forever, to say
	// nothing at all -- and the engine's own field doc warns that touching
	// linkCheckedAt is what strobes the Files list.
	verifyRefreshInterval = 24 * time.Hour

	// initialDelay lets the worker finish registering and settle before the
	// first walk, so a laptop waking up does not start hashing immediately.
	initialDelay = 20 * time.Second
)

// Options wires the manager. Shaped after appsession.Options for the reason
// that package's own header gives: one struct the caller fills from policy and
// config, with the seams the tests replace.
type Options struct {
	Logger *slog.Logger
	// StateDir is where the per-watch ledgers live.
	StateDir string
	// BaseURL is the cluster's HTTP origin. Derived from the worker's own
	// cluster_url by the caller -- there is no second host to configure.
	BaseURL string
	// WorkerID is this machine's v1:worker:registration id, taken from the
	// RegisterAck. Empty means the manager does nothing: without it no push
	// can name its machine, and an unnamed push is a file with no origin.
	WorkerID string
	// Bearer resolves the signed-in user's access token. Called per request
	// so a token that rolls forward mid-sweep is picked up.
	Bearer func(context.Context) (string, error)
	// CheckPath is this machine's veto. A nil check REFUSES EVERYTHING, which
	// is the safe direction: a wiring mistake must not become "back up
	// anything the cluster names".
	CheckPath func(path string) error
	// SweepInterval overrides DefaultSweepInterval. Tests set it.
	SweepInterval time.Duration
	// HTTPClient is shared by both clients. Tests point it at an httptest
	// server.
	HTTPClient *http.Client
}

// Manager runs the sweep loop.
type Manager struct {
	opts    Options
	graph   *Graph
	library *Library
	logger  *slog.Logger

	mu      sync.Mutex
	ledgers map[string]*Ledger
}

// New builds a manager, or nil when it has nothing to work with.
//
// A NIL MANAGER IS A WORKING ONE that does nothing, so the caller wires it
// unconditionally and every method is nil-safe. A machine with no signed-in
// user is the ordinary state of a freshly paired worker, and it must not be a
// startup failure.
func New(opts Options) *Manager {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.SweepInterval <= 0 {
		opts.SweepInterval = DefaultSweepInterval
	}
	if strings.TrimSpace(opts.BaseURL) == "" || opts.Bearer == nil {
		return nil
	}
	return &Manager{
		opts:    opts,
		graph:   NewGraph(opts.BaseURL, opts.HTTPClient, opts.Bearer),
		library: NewLibrary(opts.BaseURL, opts.HTTPClient, opts.Bearer),
		logger:  opts.Logger,
		ledgers: map[string]*Ledger{},
	}
}

// Run sweeps until the context is cancelled.
//
// A TICKER, in the shape heartbeatLoop established, and every failure inside
// one pass is logged and dropped rather than returned: a cluster that is
// briefly unreachable, or a folder on an unmounted drive, must not end the
// loop -- the next tick is the retry, and it is already scheduled.
// `registrationID` is a GETTER, not a value, because the id is not known when
// this starts: it arrives on the RegisterAck, inside a reconnect loop that may
// still be backing off. A sweep with no id is SKIPPED and the next tick asks
// again -- which is also the right behaviour for a machine whose registration
// was revoked while it was running.
func (m *Manager) Run(ctx context.Context, registrationID func() string) {
	if m == nil || registrationID == nil {
		return
	}
	if !sleepFor(ctx, initialDelay) {
		return
	}
	ticker := time.NewTicker(m.opts.SweepInterval)
	defer ticker.Stop()
	announced := false
	for {
		if id := strings.TrimSpace(registrationID()); id != "" {
			m.opts.WorkerID = id
			m.SweepOnce(ctx)
		} else if !announced {
			// Said ONCE. A machine that has not registered yet is the ordinary
			// state for the first few seconds, and logging it every five
			// minutes forever would be the only line in a quiet log.
			m.logger.Info("backup: waiting for this machine to register before sweeping")
			announced = true
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// SweepOnce runs one pass over every watch this machine holds. Exported so
// `memql worker backup --once` runs EXACTLY what the loop runs -- the
// discipline models_cmd.go states: a second implementation could disagree
// with the first and the operator would have no way to know which one lied.
func (m *Manager) SweepOnce(ctx context.Context) {
	if m == nil {
		return
	}
	watches, err := m.graph.Watches(ctx, m.opts.WorkerID)
	if err != nil {
		m.logger.Warn("backup: could not read this machine's watched folders", "error", err)
		return
	}
	for _, watch := range watches {
		if err := ctx.Err(); err != nil {
			return
		}
		if !watch.Active() {
			// Paused. Nothing is scanned, nothing is reported, and the ledger
			// is deliberately LEFT ALONE -- resuming must not re-push a folder
			// somebody paused for an afternoon.
			continue
		}
		m.sweepWatch(ctx, watch)
	}
}

func (m *Manager) sweepWatch(ctx context.Context, watch Watch) {
	log := m.logger.With("watch", watch.ID, "path", watch.LocalPath)

	// THE VETO RUNS FIRST, before the path is even stat'd. A folder this
	// machine has not been told to back up is not a folder to go looking at.
	check := m.opts.CheckPath
	if check == nil {
		m.report(ctx, watch.ID, "refused_by_policy", 0, 0,
			"this machine has no backup policy configured, so it backs up nothing")
		return
	}
	if err := check(watch.LocalPath); err != nil {
		m.report(ctx, watch.ID, "refused_by_policy", 0, 0, err.Error())
		return
	}

	scan, err := Scan(watch.LocalPath, watch.ExcludeGlobs, watch.IncludeHidden)
	if err != nil {
		state := "unreadable"
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, os.ErrNotExist) {
			state = "missing"
		}
		m.report(ctx, watch.ID, state, 0, 0, err.Error())
		// A folder that is gone takes its files with it, and every one of them
		// is now origin_gone. Reported from the ledger, because the walk found
		// nothing to report from.
		m.flagAllGone(ctx, watch)
		return
	}

	ledger := m.ledgerFor(watch.ID)
	var pushErr string
	pushed := 0
	for _, entry := range scan.Entries {
		if err := ctx.Err(); err != nil {
			return
		}
		changed, err := m.pushIfChanged(ctx, watch, ledger, entry)
		if err != nil {
			// The FIRST failure is what is reported; the sweep carries on, so
			// one file the cluster refuses does not stop the other thousand.
			if pushErr == "" {
				pushErr = err.Error()
			}
			log.Warn("backup: a file did not arrive", "file", entry.Rel, "error", err)
			continue
		}
		if changed {
			pushed++
		}
	}

	m.verify(ctx, watch, ledger, scan)
	if err := ledger.Save(); err != nil {
		log.Warn("backup: could not save this machine's record of what it sent", "error", err)
	}

	state := "ok"
	message := pushErr
	if scan.Skipped > 0 && message == "" {
		// A partly-unreadable folder reports ok with a count rather than
		// `unreadable`: most of it worked, and calling the whole watch
		// unreadable would hide the files that did arrive.
		message = "some entries could not be read on this machine"
	}
	m.report(ctx, watch.ID, state, len(scan.Entries), scan.Bytes, message)
	if pushed > 0 {
		log.Info("backup: files arrived", "count", pushed, "seen", len(scan.Entries))
	}
}

// pushIfChanged sends a file only when its bytes actually differ.
//
// THREE GATES, CHEAPEST FIRST, and the order is the whole performance story:
// a stat that matches costs nothing; a stat that moved but a digest that did
// not costs one read and no upload (a touched file, a copy restored, a
// timestamp changed by a backup tool); only a digest that moved sends bytes.
func (m *Manager) pushIfChanged(ctx context.Context, watch Watch, ledger *Ledger, entry Entry) (bool, error) {
	prior, known := ledger.Get(entry.Path)
	if known && prior.Stamp.Unchanged(entry.Stamp) {
		return false, nil
	}
	digest, err := Digest(entry.Path)
	if err != nil {
		return false, err
	}
	if known && prior.Stamp.SHA256 == digest && prior.FileID != "" {
		// Same bytes, new timestamp. Record the new stamp so the next sweep
		// takes the cheap path again, and send nothing.
		prior.Stamp = entry.Stamp
		prior.Stamp.SHA256 = digest
		ledger.Put(entry.Path, prior)
		return false, nil
	}

	result, err := m.library.Push(ctx, m.opts.WorkerID, entry.Path, watch.FolderID, entry.Stamp.Size)
	if err != nil {
		return false, err
	}
	stamp := entry.Stamp
	stamp.SHA256 = digest
	now := time.Now().UTC().Unix()
	ledger.Put(entry.Path, Record{
		Stamp:          stamp,
		FileID:         result.FileID,
		PushedAtUnix:   now,
		VerifiedAtUnix: now,
		// The ENGINE stamps `synced` on any push naming a (machine, path), so
		// recording it here is recording what the server just did -- not a
		// claim this side made. Writing it again over the wire would be a
		// second write saying the same thing.
		LinkState: "synced",
	})
	return true, nil
}

// verify is the origin-liveness lane: the two states only something looking at
// the machine can give, plus the paced re-stamp of the one the engine gave.
func (m *Manager) verify(ctx context.Context, watch Watch, ledger *Ledger, scan ScanResult) {
	present := make(map[string]struct{}, len(scan.Entries))
	for _, entry := range scan.Entries {
		present[entry.Path] = struct{}{}
	}
	now := time.Now().UTC().Unix()

	for _, path := range ledger.Paths() {
		if err := ctx.Err(); err != nil {
			return
		}
		rec, ok := ledger.Get(path)
		if !ok {
			continue
		}
		want := "synced"
		if _, here := present[path]; !here {
			// It was pushed from this folder and is not there any more. THE
			// COPY IS NOT TOUCHED -- this is a label, and the row stays in the
			// ledger so a later sweep can tell "gone" from "never seen".
			want = "origin_gone"
		}
		changed := rec.LinkState != want
		stale := now-rec.VerifiedAtUnix >= int64(verifyRefreshInterval/time.Second)
		if !changed && !stale {
			continue
		}
		fileID := rec.FileID
		if fileID == "" {
			resolved, err := m.graph.FileIDAt(ctx, m.opts.WorkerID, path)
			if err != nil || resolved == "" {
				continue
			}
			fileID = resolved
		}
		if err := m.graph.SetLinkState(ctx, fileID, want); err != nil {
			m.logger.Warn("backup: could not report a file's state", "file", path, "error", err)
			continue
		}
		rec.FileID = fileID
		rec.LinkState = want
		rec.VerifiedAtUnix = now
		ledger.Put(path, rec)
	}
	_ = watch
}

// flagAllGone reports every file of a watch whose folder itself has vanished.
func (m *Manager) flagAllGone(ctx context.Context, watch Watch) {
	ledger := m.ledgerFor(watch.ID)
	m.verify(ctx, watch, ledger, ScanResult{})
	if err := ledger.Save(); err != nil {
		m.logger.Warn("backup: could not save this machine's record", "watch", watch.ID, "error", err)
	}
}

func (m *Manager) report(ctx context.Context, watchID, state string, files int, bytes int64, message string) {
	if err := m.graph.ReportSweep(ctx, SweepReport{
		WatchID:     watchID,
		OriginState: state,
		FilesSeen:   files,
		BytesSeen:   bytes,
		Error:       message,
	}); err != nil {
		m.logger.Warn("backup: could not report a sweep", "watch", watchID, "error", err)
	}
}

func (m *Manager) ledgerFor(watchID string) *Ledger {
	m.mu.Lock()
	defer m.mu.Unlock()
	if l, ok := m.ledgers[watchID]; ok {
		return l
	}
	l := LoadLedger(m.opts.StateDir, watchID)
	m.ledgers[watchID] = l
	return l
}

// sleepFor waits, with a little jitter, and reports whether the wait finished
// rather than the context ending. Jittered for the reason the reconnect
// backoff is: a fleet of machines that all woke at nine o'clock should not all
// start walking at the same instant.
func sleepFor(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return true
	}
	jitter := time.Duration(rand.Int63n(int64(d/4) + 1))
	timer := time.NewTimer(d + jitter)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
