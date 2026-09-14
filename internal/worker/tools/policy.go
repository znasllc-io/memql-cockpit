package tools

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"

	"github.com/znasllc-io/memql-cockpit/internal/worker/apps"
	"github.com/znasllc-io/memql-cockpit/internal/worker/harness"
	"github.com/znasllc-io/memql-cockpit/internal/worker/models"
)

// Policy controls allow/deny decisions for the cockpit's headless
// tools. Loaded from ~/.memql/policy.yaml at startup; reload via
// SIGHUP.
//
// Defaults (DefaultPolicy) ship a curated allow list for common
// development commands and a sticky deny list for destructive ops.
// User policy can extend `Allow` and `Deny`; `Deny` always beats
// `Allow`.
type Policy struct {
	mu         sync.RWMutex
	shell      ShellPolicy
	fs         FSPolicy
	http       HTTPPolicy
	apps       AppsPolicy
	models     ModelsPolicy
	inference  InferencePolicy
	backup     BackupPolicy
	configPath string
}

// ShellPolicy controls workerHost.exec.
//
// RunAsUser is the optional system username to setuid to before
// running the child process. Empty (the default) inherits the
// cockpit-worker process's uid. Setting this to a dedicated user
// like "memql-worker-exec" gives shell exec a clean blast radius
// even if the worker itself runs as the user. NOTE: setuid
// requires the cockpit-worker process to be running as root (or
// have the appropriate capability); silently ignored otherwise.
//
// The MaxCPUSeconds / MaxMemoryMB / MaxOpenFiles fields apply
// rlimits to the child process via syscall.Setrlimit on Unix
// (Linux + macOS). Zero or negative values leave the corresponding
// rlimit at the inherited default.
type ShellPolicy struct {
	Allow         []string `yaml:"allow"`
	Deny          []string `yaml:"deny"`
	RunAsUser     string   `yaml:"run_as_user"`
	MaxCPUSeconds int      `yaml:"max_cpu_seconds"`
	MaxMemoryMB   int      `yaml:"max_memory_mb"`
	MaxOpenFiles  int      `yaml:"max_open_files"`
}

// FSPolicy controls workerHost.fs_*.
type FSPolicy struct {
	WorkspaceRoot string   `yaml:"workspace_root"`
	Allow         []string `yaml:"allow"`
	Deny          []string `yaml:"deny"`
}

// AppsPolicy controls which local apps this machine will let the engine
// drive (memql-cockpit#346).
//
//	apps:
//	  allow:
//	    - claude-code
//	    - codex
//
// DEFAULT-DENY, and deliberately so. An app session does exactly what
// workerHost.exec does -- edits files and runs commands on somebody's own
// computer -- so it gets the same posture the rest of this file has:
// nothing runs until the machine's owner says which app may. An empty
// allow list is the state of every machine that has not been configured,
// including every machine upgrading into this feature, and it must not
// mean "all".
//
// An app that is present but not listed is still REPORTED, with
// allowed=false. That is what makes the portal able to say
// "present, blocked" instead of rendering it identically to "not
// installed" -- one of those an operator can fix, the other sends them
// looking for the wrong problem.
//
// LEVELS ARE THE OPPOSITE POSTURE, and on purpose (memql-cockpit#438,
// design D8). allow decides WHETHER an app may run here, which only the
// owner can grant; levels decide HOW it runs once it may -- which model and
// effort a LEVEL the engine names becomes -- and the cockpit ships an
// answer for that (internal/worker/harness BuiltinLevels). So an absent
// block is the built-in table, not "nothing":
//
//	apps:
//	  levels:
//	    claude-code:
//	      reasoning:
//	        model: fable
//	        effort: max
//	    codex:
//	      strong:
//	        model: gpt-5.5
//	        effort: xhigh
//
// An entry replaces its level's built-in entry WHOLE, model and effort
// together; a level with no entry keeps the built-in one; an entry with
// neither knob runs that level at the app's own defaults. An entry the app
// would misread REFUSES its level rather than falling back -- see
// AppLevels.
type AppsPolicy struct {
	Allow []string `yaml:"allow"`
	// Levels is kept as the YAML node it was written as, and read by
	// readAppLevels, rather than decoded into Go types here. A typed decode
	// has two failure modes and both are wrong for this block. A key it
	// does not know (`efort: max`) is DROPPED without a word -- and since an
	// entry replaces its row whole, the owner's effort silently becomes the
	// app's default, the very failure CheckKnobs exists to prevent. A shape
	// it does not expect (`reasoning: opus`) fails the WHOLE file, and a
	// worker that cannot parse policy.yaml runs on the defaults, which allow
	// no app at all. Walking the node turns both into a sentence about the
	// one entry that is wrong.
	Levels yaml.Node `yaml:"levels"`
}

// ModelsPolicy controls which local models this machine will serve, and
// which OpenAI-compatible runtimes it knows about (memql-cockpit#359).
//
//	models:
//	  allow:
//	    - qwen3.5:9b
//	    - qwen3-embedding:0.6b
//	  runtimes:
//	    - name: lmstudio
//	      base_url: http://127.0.0.1:1234/v1
//	      models:
//	        - id: qwen2.5-7b-instruct
//	          context_window: 32768
//	          structured_output: true
//	    - name: local-speech
//	      base_url: http://127.0.0.1:8880/v1
//	      models:
//	        - id: kokoro-82m
//	          audio_out: true
//
// DEFAULT-DENY, for the reason apps.allow is. Serving a model call spends
// this machine's own GPU on somebody else's prompt, so nothing is offered
// until the machine's owner says which model may be. An empty allow list
// is the state of every machine upgrading into this feature, and it must
// not mean "all".
//
// A model that is present but unlisted is still REPORTED, blocked --
// which is what lets the portal say "present, blocked" rather than
// rendering it identically to "not installed".
//
// The runtime declaration types live in the models package rather than
// here: they are the shape discovery consumes, and the yaml tags belong
// with the struct that carries the fields. models imports nothing from
// this repo, so the direction stays acyclic.
//
// models.pull is the OPPOSITE posture -- DEFAULT TRUE -- and the
// inconsistency is the point rather than an oversight. Allowing a model
// spends this machine's GPU on somebody ELSE's prompt, which is a grant
// only the machine's owner can make; PULLING one is that owner acting on
// their own machine, and the engine gates the act on being that owner
// before it ever reaches this process. A default-deny pull switch would
// mean every machine in the fleet answered "Pull" with a refusal until
// somebody edited a file on it by hand, on the machine -- which is the
// exact errand this feature exists to remove. An operator who wants the
// machine to refuse says so:
//
//	models:
//	  pull: false
type ModelsPolicy struct {
	Allow    []string                 `yaml:"allow"`
	Runtimes []models.DeclaredRuntime `yaml:"runtimes"`
	// Pull is a POINTER, and that is the whole of how the default-true
	// posture survives contact with a yaml file. An absent key
	// unmarshals into the zero value, so a plain bool would read every
	// policy.yaml written before this key existed -- which is all of
	// them -- as `pull: false`, and every machine already in the fleet
	// would start refusing pulls with nothing on it saying why. nil
	// means "the file has no opinion", and the accessor answers that
	// with the default. Do not "simplify" this to a bool.
	Pull *bool `yaml:"pull"`
}

// BackupPolicy controls which folders this machine will back up into the
// Library (memql#4841).
//
//	backup:
//	  roots:
//	    - ~/Clients
//	    - /Volumes/Work
//
// DEFAULT-DENY, the same posture as apps.allow and for a stronger reason.
// A watched folder is arranged in the GRAPH -- somebody sets it up in a
// browser, on a different machine -- so the path in it is one the CLUSTER is
// naming on somebody else's computer. That is exactly the situation
// appsession's CheckWorkspace exists for, and it gets the same answer: the
// engine may ASK, and this machine decides. Without it, anyone who could
// write a watch row could point this cockpit at ~/.ssh and have it uploaded.
//
// An empty roots list is the state of every machine that has not been
// configured, including every machine upgrading into this feature, and it
// must not mean "all".
//
// A refusal is REPORTED rather than silent: the sweep answers
// originState=refused_by_policy, which the Files app renders as "this machine
// said no" with the repair -- add the path here -- named on screen. A machine
// that quietly ignored a watch would be indistinguishable from one that was
// offline.
type BackupPolicy struct {
	Roots []string `yaml:"roots"`
}

// HTTPPolicy controls workerHost.http_fetch.
type HTTPPolicy struct {
	AllowURLs       []string `yaml:"allow_urls"`
	DenyURLs        []string `yaml:"deny_urls"`
	MaxBodyBytes    int      `yaml:"max_body_bytes"`
	MaxRedirects    int      `yaml:"max_redirects"`
	BlockPrivateNet bool     `yaml:"block_private_net"`
}

// InferencePolicy is this machine's answer to "who may this GPU serve"
// (engine memql#5146, record D6).
//
// SHARING TAKES TWO CONSENTS AND THIS IS ONLY ONE OF THEM. The other is
// the owner's, set from the machine page and stored on the
// registration; a machine serves somebody else's prompt only when both
// say `cluster`. That split is the same rule that keeps `sharedInference`
// off the cockpit entirely: a machine cannot grant a permission on its
// owner's behalf, and one that could would be granting itself one.
//
// It is a string rather than a bool because the engine's half spells the
// same two words, and a bool here would have to be named for one of them
// -- `share: true` reads as a grant where `serve: cluster` reads as a
// setting, and only the second survives being read back a year later.
//
//	inference:
//	  serve: cluster    # or owner, which is the default
type InferencePolicy struct {
	Serve string `yaml:"serve"`
}

const (
	// ServeOwner: only this machine's owner. The default, and where an
	// unrecognised value lands.
	ServeOwner = "owner"
	// ServeCluster: anyone in the cluster, subject to the OWNER's
	// separate grant. This half alone grants nothing.
	ServeCluster = "cluster"
)

// rawPolicy is the YAML-unmarshal target.
type rawPolicy struct {
	Shell     ShellPolicy     `yaml:"shell"`
	FS        FSPolicy        `yaml:"fs"`
	HTTP      HTTPPolicy      `yaml:"http"`
	Apps      AppsPolicy      `yaml:"apps"`
	Backup    BackupPolicy    `yaml:"backup"`
	Models    ModelsPolicy    `yaml:"models"`
	Inference InferencePolicy `yaml:"inference"`
}

// DefaultPolicy returns the baseline allow/deny lists shipped with
// the cockpit binary.
func DefaultPolicy() *Policy {
	return &Policy{
		shell: ShellPolicy{
			Allow: []string{
				"git", "npm", "yarn", "pnpm", "pip", "pip3", "python", "python3",
				"node", "go", "cargo", "make", "ls", "cat", "grep", "find",
				"mkdir", "touch", "mv", "cp", "echo", "pwd", "head", "tail",
				"wc", "sort", "uniq", "diff", "tar", "unzip", "zip", "ssh-keygen",
				"docker", "kubectl",
				// macOS-specific launchers. `open` is the canonical
				// "launch this app / URL / document" command on macOS;
				// without it the agent can't fulfil "open Chrome" or
				// "open this folder in Finder" via shell, which is the
				// cleaner path than scripting cmd+space + type + return.
				// `osascript` is the AppleScript runner that bridges to
				// any Mac app's scripting dictionary -- "tell Mail to
				// send this", "tell Calendar to add an event", etc.
				// Both are user-level commands; neither escalates
				// privileges and the FS deny list still gates writes
				// to sensitive paths (~/.ssh, /etc/shadow, etc.).
				"open", "osascript",
			},
			Deny: []string{
				"rm", "dd", "mkfs", "sudo", "su", "chown", "chmod",
				"kill", "killall", "reboot", "shutdown", "halt", "poweroff",
				"curl", "wget", "fdisk", "format", "mount", "umount",
			},
			// Per-call rlimits. Earlier defaults left these as zero
			// (inherit parent), which gave a runaway shell exec the
			// full ulimit ceiling of the user session -- a
			// fork-bomb-shaped agent could exhaust CPU + memory of
			// the operator's laptop before the kill switch fired.
			// 5 minutes / 1 GiB / 1024 fds is generous for normal
			// dev work but caps the blast radius. Operators with
			// genuine heavier workloads can override via worker
			// policy.yaml.
			//
			// Note: RLIMIT_AS (memory) is a no-op on macOS today --
			// macOS lacks a portable equivalent; see
			// exec_unix_darwin.go. The cap still applies on Linux.
			MaxCPUSeconds: 300,
			MaxMemoryMB:   1024,
			MaxOpenFiles:  1024,
		},
		fs: FSPolicy{
			WorkspaceRoot: "",
			Deny: []string{
				"~/.ssh", "~/.aws", "~/.config", "~/.kube",
				"~/Library/Cookies", "~/Library/Application Support/Google/Chrome",
				"/etc/shadow", "/etc/passwd", "/private/etc/master.passwd",
			},
		},
		http: HTTPPolicy{
			MaxBodyBytes:    50 * 1024 * 1024,
			MaxRedirects:    5,
			BlockPrivateNet: true,
		},
	}
}

// LoadPolicy reads the YAML file at path and merges it on top of
// DefaultPolicy. Missing file returns DefaultPolicy.
func LoadPolicy(path string) (*Policy, error) {
	p := DefaultPolicy()
	p.configPath = path
	if err := p.reload(); err != nil {
		return p, err
	}
	return p, nil
}

// Reload re-reads the policy file. Called on SIGHUP from the worker
// runner.
func (p *Policy) Reload() error {
	if p == nil {
		return nil
	}
	return p.reload()
}

func (p *Policy) reload() error {
	if p == nil || p.configPath == "" {
		return nil
	}
	data, err := readFile(p.configPath)
	if err != nil {
		if errors.Is(err, errNotExist) {
			return nil
		}
		return fmt.Errorf("policy: read %s: %w", p.configPath, err)
	}
	var raw rawPolicy
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("policy: parse %s: %w", p.configPath, err)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.shell.Allow = mergeUnique(p.shell.Allow, raw.Shell.Allow)
	p.shell.Deny = mergeUnique(p.shell.Deny, raw.Shell.Deny)
	if raw.Shell.RunAsUser != "" {
		p.shell.RunAsUser = raw.Shell.RunAsUser
	}
	if raw.Shell.MaxCPUSeconds > 0 {
		p.shell.MaxCPUSeconds = raw.Shell.MaxCPUSeconds
	}
	if raw.Shell.MaxMemoryMB > 0 {
		p.shell.MaxMemoryMB = raw.Shell.MaxMemoryMB
	}
	if raw.Shell.MaxOpenFiles > 0 {
		p.shell.MaxOpenFiles = raw.Shell.MaxOpenFiles
	}
	if raw.FS.WorkspaceRoot != "" {
		p.fs.WorkspaceRoot = raw.FS.WorkspaceRoot
	}
	p.fs.Allow = mergeUnique(p.fs.Allow, raw.FS.Allow)
	p.fs.Deny = mergeUnique(p.fs.Deny, raw.FS.Deny)
	p.backup.Roots = mergeUnique(p.backup.Roots, raw.Backup.Roots)
	p.http.AllowURLs = mergeUnique(p.http.AllowURLs, raw.HTTP.AllowURLs)
	p.http.DenyURLs = mergeUnique(p.http.DenyURLs, raw.HTTP.DenyURLs)
	if raw.HTTP.MaxBodyBytes > 0 {
		p.http.MaxBodyBytes = raw.HTTP.MaxBodyBytes
	}
	if raw.HTTP.MaxRedirects > 0 {
		p.http.MaxRedirects = raw.HTTP.MaxRedirects
	}
	if raw.HTTP.BlockPrivateNet {
		p.http.BlockPrivateNet = true
	}
	// Apps merge the same way shell/fs allow lists do, so SIGHUP adds an
	// app without a worker restart. There is no baseline to merge onto:
	// DefaultPolicy leaves this empty, which is the default-deny above.
	p.apps.Allow = mergeUnique(p.apps.Allow, raw.Apps.Allow)
	// apps.levels REPLACES, for the reason models.runtimes does below: an
	// entry is a record, and merging two generations of one would run a
	// model from one file at an effort from another. A SIGHUP that removed
	// the block returns this machine to the built-in table.
	p.apps.Levels = raw.Apps.Levels
	// models.allow merges the way apps.allow does, so SIGHUP makes a
	// newly pulled model offerable without a worker restart.
	p.models.Allow = mergeUnique(p.models.Allow, raw.Models.Allow)
	// models.pull REPLACES rather than merges, for the reason the
	// runtimes below do and one of its own: the field is tri-state, so
	// "absent" is a meaningful value (the default, true). Merging would
	// make `pull: false` unremovable without a worker restart -- an
	// operator who deleted the line would keep the refusal and have
	// nothing left in the file that explained it.
	p.models.Pull = raw.Models.Pull
	// Runtimes REPLACE rather than merge, and the asymmetry is
	// deliberate: an allow entry is a bare name, where a runtime is a
	// record with a base URL, a key variable and a model list. Merging
	// two records that share a name produces a hybrid neither the
	// operator nor this code intended -- an endpoint moved to a new port
	// would keep answering on the old one until a restart.
	p.models.Runtimes = raw.Models.Runtimes
	// inference.serve REPLACES, so a SIGHUP that removed the key
	// returns this machine to `owner` rather than leaving a grant the
	// file no longer mentions. Merging a consent would make it
	// unrevokable without a restart, which is the wrong direction for
	// the one setting here that hands a stranger this machine's GPU.
	p.inference.Serve = raw.Inference.Serve
	return nil
}

// InferenceServe reports this machine's sharing consent: ServeOwner or
// ServeCluster.
//
// AN UNRECOGNISED VALUE IS ServeOwner, and it is neither an error nor a
// grant. A typo in policy.yaml must not widen a permission -- the
// fail-closed direction every other list in this file runs in -- and it
// must not stop a worker starting either, because a machine that
// refused to boot over a misspelled sharing preference is a machine
// nobody can reach to fix it.
func (p *Policy) InferenceServe() string {
	// A nil policy reports the closed default rather than panicking,
	// which is what every sibling accessor here does. Unreachable from
	// handleRun today; the guard costs a line and the alternative is a
	// panic on the Register path.
	if p == nil {
		return ServeOwner
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.inference.Serve == ServeCluster {
		return ServeCluster
	}
	return ServeOwner
}

// CheckShell returns nil if the supplied command is allowed.
func (p *Policy) CheckShell(cmd string) error {
	if p == nil {
		return errors.New("policy: not configured")
	}
	binary := firstToken(cmd)
	if binary == "" {
		return errors.New("policy: empty command")
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, deny := range p.shell.Deny {
		if strings.EqualFold(binary, deny) {
			return fmt.Errorf("shell deny list: %q", binary)
		}
	}
	for _, allow := range p.shell.Allow {
		if strings.EqualFold(binary, allow) {
			return nil
		}
	}
	return fmt.Errorf("shell allow list: %q not allowed", binary)
}

// CheckPath rejects paths that escape the workspace root or land
// inside a sensitive directory.
func (p *Policy) CheckPath(path string) error {
	if p == nil {
		return errors.New("policy: not configured")
	}
	cleaned, err := canonicalPath(path)
	if err != nil {
		return err
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, deny := range p.fs.Deny {
		if matchesPath(cleaned, expandHome(deny)) {
			return fmt.Errorf("fs deny list: %q under %q", cleaned, deny)
		}
	}
	if p.fs.WorkspaceRoot != "" {
		root := expandHome(p.fs.WorkspaceRoot)
		if !strings.HasPrefix(cleaned, root) {
			return fmt.Errorf("fs: path %q outside workspace root %q", cleaned, root)
		}
	}
	return nil
}

// CheckBackupPath decides whether this machine will back up a folder the
// cluster named (memql#4841).
//
// SEPARATE FROM CheckPath, and deliberately not layered on it. CheckPath
// answers "may a tool touch this", and its workspace root is a SINGLE
// directory an agent works inside; a backup is a standing arrangement over
// somebody's own documents, which live in several places and never inside a
// workspace root. Reusing it would have made the feature unusable and then
// tempted somebody to widen fs.workspace_root, which would widen every tool
// call on the machine at the same time.
//
// The fs DENY list still applies, because a directory somebody marked as
// never-touch is not a directory to upload either.
func (p *Policy) CheckBackupPath(path string) error {
	if p == nil {
		return errors.New("policy: not configured")
	}
	cleaned, err := canonicalPath(path)
	if err != nil {
		return err
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, deny := range p.fs.Deny {
		if matchesPath(cleaned, expandHome(deny)) {
			return fmt.Errorf("this machine's policy denies %q (fs.deny lists %q)", cleaned, deny)
		}
	}
	if len(p.backup.Roots) == 0 {
		return fmt.Errorf("this machine backs up nothing yet: add %q (or a folder above it) to backup.roots in policy.yaml", cleaned)
	}
	for _, root := range p.backup.Roots {
		if matchesPath(cleaned, expandHome(root)) {
			return nil
		}
	}
	return fmt.Errorf("this machine's policy does not list %q: add it (or a folder above it) to backup.roots in policy.yaml", cleaned)
}

// BackupRoots returns a copy of the folders this machine will back up.
// The copy matters for AppsAllow's reason: a SIGHUP can reload underneath a
// sweep that is already walking.
func (p *Policy) BackupRoots() []string {
	if p == nil {
		return nil
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	if len(p.backup.Roots) == 0 {
		return nil
	}
	out := make([]string, len(p.backup.Roots))
	copy(out, p.backup.Roots)
	return out
}

// CheckURL applies the http policy: allow/deny lists, SSRF
// protection (private network detection).
func (p *Policy) CheckURL(rawURL string) error {
	if p == nil {
		return errors.New("policy: not configured")
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("http: invalid url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("http: unsupported scheme %q", u.Scheme)
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, deny := range p.http.DenyURLs {
		if strings.HasPrefix(rawURL, deny) {
			return fmt.Errorf("http deny list: %q", rawURL)
		}
	}
	if len(p.http.AllowURLs) > 0 {
		matched := false
		for _, allow := range p.http.AllowURLs {
			if strings.HasPrefix(rawURL, allow) {
				matched = true
				break
			}
		}
		if !matched {
			return fmt.Errorf("http allow list: %q", rawURL)
		}
	}
	if p.http.BlockPrivateNet {
		host := u.Hostname()
		if isPrivateAddress(host) {
			return fmt.Errorf("http SSRF block: private/loopback address %q", host)
		}
	}
	return nil
}

// MaxBodyBytes returns the configured response body cap.
func (p *Policy) MaxBodyBytes() int {
	if p == nil {
		return 50 * 1024 * 1024
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.http.MaxBodyBytes <= 0 {
		return 50 * 1024 * 1024
	}
	return p.http.MaxBodyBytes
}

// MaxRedirects returns the configured redirect cap.
func (p *Policy) MaxRedirects() int {
	if p == nil {
		return 5
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.http.MaxRedirects <= 0 {
		return 5
	}
	return p.http.MaxRedirects
}

// AppsAllow returns a copy of the allowed app ids.
//
// The copy matters: the worker calls this on every heartbeat and hands
// the result to the detector, and a shared slice would race a SIGHUP
// reload mid-beat.
func (p *Policy) AppsAllow() []string {
	if p == nil {
		return nil
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	if len(p.apps.Allow) == 0 {
		return nil
	}
	out := make([]string, len(p.apps.Allow))
	copy(out, p.apps.Allow)
	return out
}

// AppLevels returns the owner's apps.levels entries for one app that
// stand, and -- per level -- the sentence refusing any entry the app would
// misread. No entries at all is the built-in table (the session lays these
// over harness.BuiltinLevels), which is what an absent block means.
//
// A REFUSED ENTRY REFUSES ITS LEVEL; it does not fall back to the built-in
// entry. An owner who pinned a cheaper model for a level did not agree to
// the expensive default because of a typo, and Claude Code in particular
// IGNORES an effort word it does not know rather than refusing it -- so the
// refusal here is the only one anybody would see. It is REPORTED on the
// session's End, naming the line to fix, where a quiet fallback would be
// indistinguishable from the owner's entry working.
//
// The maps are the caller's own, built fresh on every call, for
// AppsAllow's reason: a SIGHUP can reload underneath a session that is
// still deciding.
func (p *Policy) AppLevels(appID string) (harness.Table, map[string]string) {
	if p == nil {
		return nil, nil
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	read := readAppLevels(p.apps.Levels)
	id := normalAppID(appID)
	return read.table[id], read.refused[id]
}

// AppLevelProblems is every problem apps.levels has, as sentences an owner
// can act on: the refusals AppLevels reports, and the entries no session can
// reach -- an app this cockpit does not drive, a word that is not a level,
// the embeddings level -- which are ignored. The worker logs them when the
// file is read and `memql worker apps` prints them, because an entry that
// silently did nothing is indistinguishable from one that worked.
func (p *Policy) AppLevelProblems() []string {
	if p == nil {
		return nil
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	return readAppLevels(p.apps.Levels).problems
}

// appLevelsRead is apps.levels read against what this cockpit can drive.
type appLevelsRead struct {
	// table holds the entries that stand, per app id.
	table map[string]harness.Table
	// refused holds, per app id and level, the sentence refusing sessions
	// at that level.
	refused map[string]map[string]string
	// problems is every sentence, in a stable order.
	problems []string
}

// readAppLevels validates apps.levels. It is a pure function of the block
// as written, run on every read rather than cached, so there is no second
// copy of the answer for a reload to leave stale -- and it never fails: a
// shape it cannot read becomes a problem about that entry, never an error
// that would cost the rest of policy.yaml.
//
// Every judgement is borrowed rather than restated: the app ids are
// apps.Specs', the level words are the engine's (harness.CheckAppLevel), and
// what an app would misread is the harness's own check (harness.CheckKnobs,
// the same one every harness runs in Start). The keys are walked in sorted
// order so a problem list does not shuffle between two reads of one file,
// which would read as the file changing.
func readAppLevels(block yaml.Node) appLevelsRead {
	read := appLevelsRead{
		table:   map[string]harness.Table{},
		refused: map[string]map[string]string{},
	}
	root := resolveYAML(&block)
	if yamlAbsent(root) {
		return read
	}
	if root.Kind != yaml.MappingNode {
		read.problems = append(read.problems,
			"apps.levels: a mapping of apps belongs here (claude-code:, codex:), not "+describeYAML(root)+", so the block is ignored")
		return read
	}
	byApp := yamlMapping(root)
	for _, key := range sortedKeys(byApp) {
		appID := normalAppID(key)
		spec, ok := apps.SpecFor(appID)
		if !ok {
			read.problems = append(read.problems, fmt.Sprintf(
				"apps.levels.%s: this cockpit drives no app called %q (it drives %s), so the entry is ignored",
				key, key, knownAppIDs()))
			continue
		}
		levelsNode := byApp[key]
		if yamlAbsent(levelsNode) {
			continue
		}
		if levelsNode.Kind != yaml.MappingNode {
			read.problems = append(read.problems, fmt.Sprintf(
				"apps.levels.%s: a mapping of levels belongs here (fast:, strong:, reasoning:), not %s, so the entry is ignored",
				key, describeYAML(levelsNode)))
			continue
		}
		levels := yamlMapping(levelsNode)
		for _, level := range sortedKeys(levels) {
			where := "apps.levels." + key + "." + level
			if err := harness.CheckAppLevel(level); err != nil {
				read.problems = append(read.problems, fmt.Sprintf("%s: %v, so the entry is ignored", where, err))
				continue
			}
			knobs, err := readLevelEntry(levels[level])
			if err == nil {
				err = harness.CheckKnobs(spec.Harness, knobs)
			}
			if err != nil {
				sentence := fmt.Sprintf("%s: %v -- this machine refuses %s sessions for %s until the entry is fixed",
					where, err, level, appID)
				if read.refused[appID] == nil {
					read.refused[appID] = map[string]string{}
				}
				read.refused[appID][level] = sentence
				read.problems = append(read.problems, sentence)
				continue
			}
			if read.table[appID] == nil {
				read.table[appID] = harness.Table{}
			}
			read.table[appID][level] = knobs
		}
	}
	return read
}

// readLevelEntry reads one apps.levels entry: a mapping of `model` and
// `effort`, each a single word, either one optional. Null -- `fast:` with
// nothing after it -- reads as `fast: {}`, an entry with no knobs.
//
// Anything else is an error NAMING what was wrong: a scalar or a list where
// the mapping belongs (`reasoning: opus`), a key that is neither knob
// (`efort`), a knob that is not a single word. The caller refuses the level
// with it, because dropping the key and running the rest of the entry would
// replace the whole row with half of what the owner wrote.
func readLevelEntry(n *yaml.Node) (harness.Knobs, error) {
	n = resolveYAML(n)
	if yamlAbsent(n) {
		return harness.Knobs{}, nil
	}
	if n.Kind != yaml.MappingNode {
		return harness.Knobs{}, fmt.Errorf("an entry is a mapping of model and effort (for example {model: opus, effort: high}), not %s",
			describeYAML(n))
	}
	var k harness.Knobs
	entry := yamlMapping(n)
	for _, key := range sortedKeys(entry) {
		v := resolveYAML(entry[key])
		if key != "model" && key != "effort" {
			return harness.Knobs{}, fmt.Errorf("an entry takes model and effort, and %q is neither", key)
		}
		if v.Kind != yaml.ScalarNode {
			return harness.Knobs{}, fmt.Errorf("%s is %s, where a single word belongs", key, describeYAML(v))
		}
		word := strings.TrimSpace(v.Value)
		if yamlAbsent(v) {
			word = ""
		}
		if key == "model" {
			k.Model = word
		} else {
			k.Effort = word
		}
	}
	return k, nil
}

// yamlMapping returns a mapping node's pairs by key. yaml.v3 has already
// refused a duplicate key when it parsed the file, so a key appears once.
func yamlMapping(n *yaml.Node) map[string]*yaml.Node {
	out := make(map[string]*yaml.Node, len(n.Content)/2)
	for i := 0; i+1 < len(n.Content); i += 2 {
		out[n.Content[i].Value] = n.Content[i+1]
	}
	return out
}

// resolveYAML follows an alias (`*name`) to the node it names.
func resolveYAML(n *yaml.Node) *yaml.Node {
	for n != nil && n.Kind == yaml.AliasNode && n.Alias != nil {
		n = n.Alias
	}
	return n
}

// yamlAbsent is a key that is not there, or one written with nothing after
// it -- which YAML reads as null.
func yamlAbsent(n *yaml.Node) bool {
	return n == nil || n.Kind == 0 || (n.Kind == yaml.ScalarNode && n.Tag == "!!null")
}

// describeYAML names a node's shape for a sentence, quoting a scalar so the
// owner sees the very word they wrote.
func describeYAML(n *yaml.Node) string {
	switch n.Kind {
	case yaml.ScalarNode:
		return fmt.Sprintf("the single word %q", n.Value)
	case yaml.SequenceNode:
		return "a list"
	case yaml.MappingNode:
		return "a mapping"
	}
	return "something else"
}

// normalAppID reads an app id the way the detector reads apps.allow:
// without regard to case or surrounding space, so the two keys an owner
// writes side by side agree about what they name.
func normalAppID(id string) string {
	return strings.ToLower(strings.TrimSpace(id))
}

// knownAppIDs names the apps this cockpit drives, for a sentence.
func knownAppIDs() string {
	var ids []string
	for _, s := range apps.Specs() {
		ids = append(ids, s.ID)
	}
	return strings.Join(ids, ", ")
}

// ModelsAllow returns a copy of the allowed model ids. Empty is
// default-deny, not "all".
func (p *Policy) ModelsAllow() []string {
	if p == nil {
		return nil
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	if len(p.models.Allow) == 0 {
		return nil
	}
	out := make([]string, len(p.models.Allow))
	copy(out, p.models.Allow)
	return out
}

// ModelsPullAllowed reports whether this machine will pull a model when
// it is asked to -- by its own CLI, and by the engine over ModelPullStart
// (the arm in internal/worker/modelpull.go reads it before running).
//
// TRUE when the key is absent, which is the opposite of every other
// answer this file gives. The reasoning is on ModelsPolicy: a pull is the
// machine's own owner acting on their own machine, and a fleet-wide
// silent refusal is a worse failure than a pull somebody did not want,
// which the engine's owner-only gate already prevents.
//
// It returns the VALUE, never the pointer, for the reason ModelsAllow
// returns a copy: a SIGHUP reload replaces this field underneath a caller
// that is still deciding, and a caller holding the pointer would be
// reading a switch that flipped between its own two lines.
func (p *Policy) ModelsPullAllowed() bool {
	if p == nil {
		// A build that loaded no policy at all has refused nothing.
		// Answering false here would refuse a pull the owner asked for
		// with no line in any file to point at -- the same direction
		// MaxBodyBytes takes for a nil policy.
		return true
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.models.Pull == nil {
		return true
	}
	return *p.models.Pull
}

// ModelRuntimes returns a copy of the declared OpenAI-compatible
// runtimes. The nested model slices are copied too: a caller that
// appended to one would be editing the live policy under the lock this
// method just released.
func (p *Policy) ModelRuntimes() []models.DeclaredRuntime {
	if p == nil {
		return nil
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	if len(p.models.Runtimes) == 0 {
		return nil
	}
	out := make([]models.DeclaredRuntime, len(p.models.Runtimes))
	for i, rt := range p.models.Runtimes {
		out[i] = rt
		if len(rt.Models) > 0 {
			out[i].Models = make([]models.DeclaredModel, len(rt.Models))
			copy(out[i].Models, rt.Models)
		}
	}
	return out
}

// ShellLimits exposes the rlimit + privilege-drop knobs to the
// exec runner. Zero values mean "inherit from parent process".
type ShellLimits struct {
	RunAsUser     string
	MaxCPUSeconds int
	MaxMemoryMB   int
	MaxOpenFiles  int
}

// ShellLimits returns a copy of the active shell-policy limits.
func (p *Policy) ShellLimits() ShellLimits {
	if p == nil {
		return ShellLimits{}
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	return ShellLimits{
		RunAsUser:     p.shell.RunAsUser,
		MaxCPUSeconds: p.shell.MaxCPUSeconds,
		MaxMemoryMB:   p.shell.MaxMemoryMB,
		MaxOpenFiles:  p.shell.MaxOpenFiles,
	}
}

// WorkspaceRoot returns the configured fs workspace root (already
// expanded).
func (p *Policy) WorkspaceRoot() string {
	if p == nil {
		return ""
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.fs.WorkspaceRoot == "" {
		return ""
	}
	return expandHome(p.fs.WorkspaceRoot)
}

// -----------------------------------------------------------------------------
// helpers
// -----------------------------------------------------------------------------

func firstToken(cmd string) string {
	cmd = strings.TrimSpace(cmd)
	if cmd == "" {
		return ""
	}
	for i, r := range cmd {
		if r == ' ' || r == '\t' {
			return cmd[:i]
		}
	}
	return cmd
}

// sortedKeys returns a map's keys in order, for the walks whose output a
// person reads.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func mergeUnique(a, b []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(a)+len(b))
	add := func(s string) {
		if _, dup := seen[s]; dup {
			return
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	for _, v := range a {
		add(v)
	}
	for _, v := range b {
		add(v)
	}
	return out
}

func canonicalPath(p string) (string, error) {
	if p == "" {
		return "", errors.New("path: empty")
	}
	expanded := expandHome(p)
	abs, err := filepath.Abs(expanded)
	if err != nil {
		return "", fmt.Errorf("path: abs %q: %w", p, err)
	}
	return filepath.Clean(abs), nil
}

func expandHome(p string) string {
	if !strings.HasPrefix(p, "~") {
		return p
	}
	home := homeDir()
	if home == "" {
		return p
	}
	if p == "~" {
		return home
	}
	if strings.HasPrefix(p, "~/") {
		return filepath.Join(home, p[2:])
	}
	return p
}

func matchesPath(target, prefix string) bool {
	if prefix == "" {
		return false
	}
	cleanedTarget := filepath.Clean(target)
	cleanedPrefix := filepath.Clean(prefix)
	return cleanedTarget == cleanedPrefix || strings.HasPrefix(cleanedTarget, cleanedPrefix+string(filepath.Separator))
}

func isPrivateAddress(host string) bool {
	if host == "" {
		return false
	}
	if host == "localhost" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return isPrivateIP(ip)
	}
	addrs, err := net.LookupIP(host)
	if err != nil {
		// Conservative default: treat unresolvable hosts as
		// non-private (the actual fetch will fail anyway).
		return false
	}
	for _, ip := range addrs {
		if isPrivateIP(ip) {
			return true
		}
	}
	return false
}

func isPrivateIP(ip net.IP) bool {
	if ip == nil {
		return false
	}
	if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast() || ip.IsPrivate() ||
		ip.IsUnspecified() {
		return true
	}
	// Cloud metadata endpoints.
	metadataAddrs := []string{
		"169.254.169.254", // AWS / GCP / Azure IMDS
		"fd00:ec2::254",   // AWS IPv6 IMDS
	}
	str := ip.String()
	for _, m := range metadataAddrs {
		if str == m {
			return true
		}
	}
	return false
}
