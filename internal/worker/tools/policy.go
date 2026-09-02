package tools

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"path/filepath"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"

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
type AppsPolicy struct {
	Allow []string `yaml:"allow"`
}

// ModelsPolicy controls which local models this machine will serve, and
// which OpenAI-compatible runtimes it knows about (memql-cockpit#359).
//
//	models:
//	  allow:
//	    - llama3.1:8b
//	    - nomic-embed-text
//	  runtimes:
//	    - name: lmstudio
//	      base_url: http://127.0.0.1:1234/v1
//	      models:
//	        - id: qwen2.5-7b-instruct
//	          context_window: 32768
//	          structured_output: true
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
type ModelsPolicy struct {
	Allow    []string                 `yaml:"allow"`
	Runtimes []models.DeclaredRuntime `yaml:"runtimes"`
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

// rawPolicy is the YAML-unmarshal target.
type rawPolicy struct {
	Shell  ShellPolicy  `yaml:"shell"`
	FS     FSPolicy     `yaml:"fs"`
	HTTP   HTTPPolicy   `yaml:"http"`
	Apps   AppsPolicy   `yaml:"apps"`
	Backup BackupPolicy `yaml:"backup"`
	Models ModelsPolicy `yaml:"models"`
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
	// models.allow merges the way apps.allow does, so SIGHUP makes a
	// newly pulled model offerable without a worker restart.
	p.models.Allow = mergeUnique(p.models.Allow, raw.Models.Allow)
	// Runtimes REPLACE rather than merge, and the asymmetry is
	// deliberate: an allow entry is a bare name, where a runtime is a
	// record with a base URL, a key variable and a model list. Merging
	// two records that share a name produces a hybrid neither the
	// operator nor this code intended -- an endpoint moved to a new port
	// would keep answering on the old one until a restart.
	p.models.Runtimes = raw.Models.Runtimes
	return nil
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
