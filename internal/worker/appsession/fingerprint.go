package appsession

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/znasllc-io/memql-cockpit/internal/worker/apps"
	"github.com/znasllc-io/memql-cockpit/internal/worker/harness"
)

// fingerprint.go builds the session's FIRST event (memql-cockpit#443,
// decision D16 of the engine's recording record): what the world looked
// like when the session started, so a later replay can tell whether it is
// standing in the same one before it acts.

const (
	// maxListingEntries bounds the workspace listing. A workspace is a
	// project, not a home directory, but nothing forces that, and the
	// fingerprint must not become a walk of somebody's whole disk; a
	// listing cut short says so (cwdTruncated).
	maxListingEntries = 100_000

	// toolProbeTimeout bounds one tool's version probe, and toolProbeBudget
	// all of them together: they run concurrently, in front of the app's
	// start, so the pathological machine costs the budget once.
	toolProbeTimeout = 3 * time.Second
	toolProbeBudget  = 4 * time.Second

	// toolVersionTTL is how long a probed version is reused. The binary's
	// size and mtime are in the cache key too, so an upgrade shows at once.
	toolVersionTTL = 5 * time.Minute
)

// sendFingerprint sends the fingerprint as the session's first chunk.
//
// It never fails the session. A part that cannot be established is absent
// from the fingerprint, and a fingerprint that cannot be sent leaves a
// recording that starts at its first action -- worse than a whole one,
// and far better than refusing to run.
func (s *session) sendFingerprint(ctx context.Context, spec apps.Spec, workspace string) {
	body, err := json.Marshal(s.fingerprint(ctx, spec, workspace))
	if err != nil {
		s.logger.Warn("the session fingerprint could not be encoded", "error", err)
		return
	}
	if err := s.emitRecord(append(body, '\n')); err != nil {
		s.logger.Warn("the session fingerprint could not be sent", "error", err)
	}
}

// fingerprint describes the world the session starts in.
func (s *session) fingerprint(ctx context.Context, spec apps.Spec, workspace string) harness.Fingerprint {
	harnessWord := spec.Harness
	if s.start.GetKind() == KindOpen {
		// A person drives an open session; no protocol does.
		harnessWord = ""
	}
	fp := harness.Fingerprint{
		Type:    harness.FingerprintEventType,
		V:       harness.RecordVersion,
		Seq:     0,
		TakenAt: time.Now().UTC().Format(time.RFC3339Nano),
		App: harness.FingerprintApp{
			ID:      spec.ID,
			Version: s.manager.opts.Detector.Version(ctx, spec.ID),
			Harness: harnessWord,
		},
		Platform: harness.Platform{OS: runtime.GOOS, Arch: runtime.GOARCH},
		Tools:    s.manager.toolVersions(ctx),
		Cwd:      workspace,
		// The environment the APP gets: the worker's, with the session's
		// own additions last, the way startChildStdin builds it.
		Variables: fingerprintVariables(harness.FingerprintVariables(spec.Harness),
			append(os.Environ(), s.mcpEnv()...)),
		Inputs: s.inputDigests(),
	}
	config, backup := s.mcpPaths()
	if l, err := listWorkspace(workspace, config, backup, maxListingEntries); err == nil {
		entries := l.entries
		fp.CwdDigest, fp.CwdEntries, fp.CwdTruncated = l.digest, &entries, l.truncated
	} else {
		s.logger.Warn("the workspace could not be listed for the session fingerprint", "error", err)
	}
	return fp
}

// mcpPaths is where the session's MCP configuration sits and, when a
// person's own was moved aside for it, where theirs went.
func (s *session) mcpPaths() (config, backup string) {
	s.mu.Lock()
	mcp := s.mcp
	s.mu.Unlock()
	return mcp.paths()
}

// fingerprintVariables records each named variable as set-or-not plus a
// digest of its value, as env would give it to a process: the LAST entry
// for a name wins, which is what os/exec does with a duplicate.
func fingerprintVariables(names, env []string) []harness.Variable {
	values := make(map[string]string, len(env))
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && k != "" {
			values[k] = v
		}
	}
	out := make([]harness.Variable, 0, len(names))
	for _, name := range names {
		v, set := values[name]
		entry := harness.Variable{Name: name, Set: set}
		if set {
			entry.Digest = harness.Digest([]byte(v))
		}
		out = append(out, entry)
	}
	return out
}

// pulledInput is one Library input and where it landed.
type pulledInput struct {
	artifact string
	path     string
}

// inputDigests describes the Library inputs as they landed, before the
// app could touch them.
func (s *session) inputDigests() []harness.Input {
	s.mu.Lock()
	pulled := append([]pulledInput(nil), s.pulled...)
	s.mu.Unlock()
	out := make([]harness.Input, 0, len(pulled))
	for _, p := range pulled {
		got := readForRecord(p.path, 0)
		out = append(out, harness.Input{
			Artifact: p.artifact, Path: p.path,
			Digest: got.digest, Bytes: got.size, Omitted: got.omitted,
		})
	}
	return out
}

// workspaceListing is the digest of a workspace's listing.
type workspaceListing struct {
	digest    string
	entries   int
	truncated bool
}

// listWorkspace digests the workspace's LISTING: the name and kind of
// everything in it, never a byte of any file.
//
// Names and kinds, because the listing answers "is this the same
// project" and contents answer "did this file change" -- which the action
// that READ a file already answers, precisely, for the files that
// mattered. Folding sizes or times in here would make every edit to any
// file, and every fresh checkout, a different world.
//
// Dependency and cache directories are LISTED BUT NOT ENTERED: whether
// node_modules or .git is there is a fact a command depends on, and what
// is inside them changes on every install and every commit. The session's
// own scaffolding is not listed at all -- it is this cockpit's, not the
// project's -- and a configuration the session moved aside is listed under
// the name it had, so the listing describes the workspace as the person
// left it.
func listWorkspace(root, configPath, backupPath string, limit int) (workspaceListing, error) {
	configRel := relUnder(root, configPath)
	backupRel := relUnder(root, backupPath)
	var lines []string
	truncated := false
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if path == root {
				return err
			}
			// A corner that cannot be read is left out, not fatal: this is
			// a fingerprint, not an inventory.
			return nil
		}
		if path == root {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() && d.Name() == sessionScaffoldDir {
			return filepath.SkipDir
		}
		if len(lines) >= limit {
			truncated = true
			return filepath.SkipAll
		}
		if d.IsDir() {
			lines = append(lines, "d "+rel+"/")
			if pushExcludedDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		switch {
		case configRel != "" && rel == configRel:
			// The session's own configuration, holding its bearer.
			return nil
		case backupRel != "" && rel == backupRel:
			rel = configRel
		}
		kind := "o"
		switch {
		case d.Type()&fs.ModeSymlink != 0:
			kind = "l"
		case d.Type().IsRegular():
			kind = "f"
		}
		lines = append(lines, kind+" "+rel)
		return nil
	})
	if err != nil {
		return workspaceListing{}, err
	}
	sort.Strings(lines)
	hash := sha256.New()
	for _, line := range lines {
		_, _ = io.WriteString(hash, line+"\n")
	}
	return workspaceListing{
		digest:    harness.DigestPrefix + hex.EncodeToString(hash.Sum(nil)),
		entries:   len(lines),
		truncated: truncated,
	}, nil
}

// relUnder is path relative to root, in slash form, or "" when path is
// empty or not beneath root.
func relUnder(root, path string) string {
	if strings.TrimSpace(path) == "" {
		return ""
	}
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return ""
	}
	return filepath.ToSlash(rel)
}

// --- the tools -------------------------------------------------------

// toolchain reports the versions of the developer tools a recorded
// command most likely ran through.
//
// A FIXED LIST, ASKED CHEAPLY. Which tools a session will use is not
// known when it starts, so the list is the toolchains coding agents reach
// for, each asked for its version the way it prints it. The answers are
// cached against each binary's size and mtime, the way the app detector
// caches the apps', so sessions back to back fork these once, not once a
// session. The probes run in the root directory, so what they report is
// the machine's default for each tool: a version manager or a project's
// own `toolchain` line can pick another for the command itself, and that
// command's own output then says so.
//
// NEVER A SHIM THAT OPENS A WINDOW. On a Mac without the command-line
// developer tools, /usr/bin/git, /usr/bin/python3 and /usr/bin/make are
// stubs that answer `--version` by raising an install dialog -- from a
// LaunchAgent, on a machine whose owner may not be at it, which is the
// hazard the worker already avoids for the Keychain. So on darwin a tool
// that resolves into /usr/bin is asked only when those tools are
// installed, and is otherwise absent from the list: the safe direction.
type toolchain struct {
	lookPath func(string) (string, error)
	run      func(ctx context.Context, bin string, args []string) (string, error)
	goos     string
	devTools func() bool
	now      func() time.Time

	mu    sync.Mutex
	cache map[string]toolEntry
}

type toolEntry struct {
	stamp   string
	version string
	ok      bool
	at      time.Time
}

// toolProbe is one tool and how it asks for its own version -- `go
// version` is a subcommand, not a flag.
type toolProbe struct {
	name string
	args []string
}

// toolProbes is the list, in name order, which is the order reported.
var toolProbes = []toolProbe{
	{"cargo", []string{"--version"}},
	{"docker", []string{"--version"}},
	{"git", []string{"--version"}},
	{"go", []string{"version"}},
	{"make", []string{"--version"}},
	{"node", []string{"--version"}},
	{"npm", []string{"--version"}},
	{"python3", []string{"--version"}},
	{"rustc", []string{"--version"}},
}

func newToolchain() *toolchain {
	return &toolchain{
		lookPath: exec.LookPath,
		run:      runToolVersion,
		goos:     runtime.GOOS,
		devTools: macDeveloperToolsInstalled,
		now:      time.Now,
		cache:    map[string]toolEntry{},
	}
}

// versions asks every tool concurrently, under one budget, and returns
// the ones that answered, in name order.
func (t *toolchain) versions(ctx context.Context) []harness.ToolVersion {
	ctx, cancel := context.WithTimeout(ctx, toolProbeBudget)
	defer cancel()
	found := make([]*harness.ToolVersion, len(toolProbes))
	var wg sync.WaitGroup
	for i, p := range toolProbes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if v, ok := t.version(ctx, p); ok {
				found[i] = &harness.ToolVersion{Name: p.name, Version: v}
			}
		}()
	}
	wg.Wait()
	out := []harness.ToolVersion{}
	for _, v := range found {
		if v != nil {
			out = append(out, *v)
		}
	}
	return out
}

// version is one tool's own report of its version, cached against its
// binary.
func (t *toolchain) version(ctx context.Context, p toolProbe) (string, bool) {
	path, err := t.lookPath(p.name)
	if err != nil || strings.TrimSpace(path) == "" {
		return "", false
	}
	if t.goos == "darwin" && strings.HasPrefix(path, "/usr/bin/") && !t.devTools() {
		return "", false
	}
	stamp := toolStamp(path)
	t.mu.Lock()
	if e, ok := t.cache[p.name]; ok && e.stamp == stamp && t.now().Sub(e.at) < toolVersionTTL {
		t.mu.Unlock()
		return e.version, e.ok
	}
	t.mu.Unlock()

	probeCtx, cancel := context.WithTimeout(ctx, toolProbeTimeout)
	defer cancel()
	raw, err := t.run(probeCtx, path, p.args)
	version := apps.Truncate(firstNonEmptyLine(raw))
	ok := err == nil && version != ""
	if ctx.Err() != nil {
		// The budget ran out, which says nothing about this tool; asking
		// again next session is the honest answer.
		return version, ok
	}
	t.mu.Lock()
	t.cache[p.name] = toolEntry{stamp: stamp, version: version, ok: ok, at: t.now()}
	t.mu.Unlock()
	return version, ok
}

// runToolVersion runs one version probe in the root directory, with
// nothing on stdin and stderr thrown away.
func runToolVersion(ctx context.Context, bin string, args []string) (string, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = string(filepath.Separator)
	out, err := cmd.Output()
	return string(out), err
}

// macDeveloperToolsInstalled reports whether the command-line developer
// tools or Xcode are installed, which is what turns /usr/bin/git and its
// neighbours from install-dialog stubs into the tools themselves.
func macDeveloperToolsInstalled() bool {
	for _, dir := range []string{
		"/Library/Developer/CommandLineTools/usr/bin",
		"/Applications/Xcode.app/Contents/Developer/usr/bin",
	} {
		if info, err := os.Stat(dir); err == nil && info.IsDir() {
			return true
		}
	}
	return false
}

// toolStamp identifies a binary by path, size and mtime, so replacing it
// in place invalidates its cached version at once.
func toolStamp(path string) string {
	info, err := os.Stat(path)
	if err != nil {
		return path
	}
	return path + "|" + info.ModTime().UTC().Format(time.RFC3339Nano) + "|" + strconv.FormatInt(info.Size(), 10)
}

func firstNonEmptyLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			return trimmed
		}
	}
	return ""
}
