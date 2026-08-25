package appsession

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// library.go moves a session's inputs in and its outputs back out
// (memql-cockpit#349), authenticating with the SESSION credential.
//
// That credential is user-scoped: its `sub` is the owning user, so these
// calls see exactly what that user could see in their browser. A refusal
// therefore says something about the USER's access, not about the
// cockpit's configuration, and the error text has to make that
// distinction or an operator spends the afternoon checking a worker token
// that was never the problem.
//
// One wrinkle the issue's wording did not anticipate, recorded here
// because it changes the error text: the engine's download route answers
// **404 on deny**, deliberately, so that a URL cannot be used to probe
// which artifact ids exist. A denied read and a missing artifact are
// therefore indistinguishable on the wire, and this says so rather than
// asserting one of them.

const (
	// libraryTimeout bounds one artifact transfer. Generous, because a
	// Library file can be a site bundle; bounded, because a session that
	// hangs on an input never starts and never fails either.
	libraryTimeout = 10 * time.Minute

	// maxPushFileBytes is the per-file ceiling on what a run's output may
	// push. The engine's own cap is 256 MB; this is deliberately lower.
	// A delegated run produces work product, and a 200 MB artifact from a
	// coding agent is a build directory somebody forgot to exclude.
	maxPushFileBytes int64 = 64 << 20

	// maxPushFiles bounds how many produced files one session pushes.
	maxPushFiles = 32
)

// pushExcludedDirs never travel back as artifacts. These are caches and
// dependency trees: they are enormous, they are reproducible from the
// files that ARE pushed, and pushing them would bury the actual output.
var pushExcludedDirs = map[string]bool{
	".git": true, ".hg": true, ".svn": true,
	"node_modules": true, "vendor": true, "target": true,
	"dist": true, "build": true, ".next": true, ".cache": true,
	".venv": true, "venv": true, "__pycache__": true,
	".terraform": true, ".gradle": true, ".idea": true,
}

// Library is the Library API client for one session.
//
// The credential is held behind a mutex because renewal replaces it
// mid-session, and the transcript push at the end must use whatever the
// current one is rather than the one the session opened with.
type Library struct {
	baseURL string
	client  *http.Client

	mu         sync.RWMutex
	credential string
}

// NewLibrary builds a client against a cluster origin.
func NewLibrary(baseURL, credential string, client *http.Client) *Library {
	if client == nil {
		client = &http.Client{Timeout: libraryTimeout}
	}
	return &Library{
		baseURL:    strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		client:     client,
		credential: credential,
	}
}

// SetCredential swaps the bearer after a renewal.
func (l *Library) SetCredential(credential string) {
	if l == nil || strings.TrimSpace(credential) == "" {
		return
	}
	l.mu.Lock()
	l.credential = credential
	l.mu.Unlock()
}

func (l *Library) bearer() string {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.credential
}

// LibraryBaseURL derives the Library origin from the worker's
// cluster_url.
//
// The Library's two byte-bearing routes are served by the same front door
// the worker already dials -- there is no second host to configure, and
// inventing one would be a knob an operator has to keep in sync with
// something they cannot see.
func LibraryBaseURL(clusterURL string) (string, error) {
	raw := strings.TrimSpace(clusterURL)
	if raw == "" {
		return "", errors.New("library: cluster_url is empty")
	}
	if !strings.Contains(raw, "://") {
		// A bare host:port, which is how the worker config may carry a
		// gRPC target. https is the only sane assumption for a route
		// that carries a bearer.
		host := strings.TrimSuffix(raw, ":443")
		return "https://" + host, nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("library: parse cluster_url: %w", err)
	}
	if u.Host == "" {
		return "", fmt.Errorf("library: cluster_url has no host: %s", raw)
	}
	scheme := strings.ToLower(u.Scheme)
	switch scheme {
	case "grpc", "http":
		scheme = "http"
	default:
		scheme = "https"
	}
	host := strings.TrimSuffix(u.Host, ":443")
	return scheme + "://" + host, nil
}

// Pull fetches one artifact's bytes into dir and returns the path.
//
// Every input has to land BEFORE the app starts. An agent that begins
// work and finds its inputs half-arrived produces confidently wrong
// output rather than an error, and nothing downstream can tell the
// difference.
func (l *Library) Pull(ctx context.Context, id, dir string) (string, error) {
	if strings.TrimSpace(id) == "" {
		return "", errors.New("library: empty artifact id")
	}
	endpoint := l.baseURL + "/artifacts/" + url.PathEscape(id) + "/content"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", fmt.Errorf("library: pull %s: %w", id, err)
	}
	req.Header.Set("Authorization", "Bearer "+l.bearer())

	resp, err := l.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("library: pull %s: %w", id, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", pullStatusError(id, resp)
	}

	name := downloadFileName(resp, id)
	if err := os.MkdirAll(dir, configDirMode); err != nil {
		return "", fmt.Errorf("library: pull %s: %w", id, err)
	}
	path := filepath.Join(dir, name)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return "", fmt.Errorf("library: pull %s: %w", id, err)
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		_ = os.Remove(path)
		return "", fmt.Errorf("library: pull %s: %w", id, err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return "", fmt.Errorf("library: pull %s: %w", id, err)
	}
	return path, nil
}

// pullStatusError turns a refusal into text that names the right party.
//
// The credential acts as the OWNING USER, so most refusals are statements
// about that user's access rather than about this machine. Getting this
// wrong sends an operator to check a worker token that is working fine.
func pullStatusError(id string, resp *http.Response) error {
	switch resp.StatusCode {
	case http.StatusUnauthorized:
		return fmt.Errorf("library: input %s: the session credential was rejected (401) -- "+
			"this is the cockpit's side: the bearer is expired or malformed", id)
	case http.StatusForbidden:
		return fmt.Errorf("library: input %s: the owning user cannot reach this artifact (403) -- "+
			"this is an access question for that user, not a cockpit misconfiguration", id)
	case http.StatusNotFound:
		// The route answers 404 on deny by design, so a URL cannot probe
		// which ids exist. Both readings are stated because the wire
		// genuinely does not distinguish them.
		return fmt.Errorf("library: input %s: not found (404) -- either the artifact does not exist, "+
			"or the owning user cannot reach it; the Library answers 404 for both so a link cannot "+
			"be used to probe which ids exist", id)
	default:
		return fmt.Errorf("library: input %s: %s", id, strings.TrimSpace(resp.Status))
	}
}

// downloadFileName picks the on-disk name for a pulled artifact, from the
// server's Content-Disposition when it offered one.
func downloadFileName(resp *http.Response, id string) string {
	if cd := resp.Header.Get("Content-Disposition"); cd != "" {
		if _, params, err := mime.ParseMediaType(cd); err == nil {
			if name := sanitizeFileName(params["filename"]); name != "" {
				return name
			}
		}
	}
	if name := sanitizeFileName(id); name != "" {
		return name
	}
	return "artifact"
}

// PushResult is one uploaded artifact.
type PushResult struct {
	ArtifactId string `json:"artifactId"`
	FileId     string `json:"fileId"`
}

// Push uploads one file and returns its artifact id.
func (l *Library) Push(ctx context.Context, path, name string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("library: push %s: %w", filepath.Base(path), err)
	}
	defer f.Close()
	return l.pushReader(ctx, f, name)
}

// PushBytes uploads an in-memory body -- the transcript path, which never
// needs to touch disk.
func (l *Library) PushBytes(ctx context.Context, body []byte, name string) (string, error) {
	return l.pushReader(ctx, bytes.NewReader(body), name)
}

func (l *Library) pushReader(ctx context.Context, r io.Reader, name string) (string, error) {
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	part, err := writer.CreateFormFile(libraryFormFileKey, name)
	if err != nil {
		return "", fmt.Errorf("library: push %s: %w", name, err)
	}
	if _, err := io.Copy(part, r); err != nil {
		return "", fmt.Errorf("library: push %s: %w", name, err)
	}
	if err := writer.WriteField(libraryFormNameKey, name); err != nil {
		return "", fmt.Errorf("library: push %s: %w", name, err)
	}
	if err := writer.Close(); err != nil {
		return "", fmt.Errorf("library: push %s: %w", name, err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, l.baseURL+"/artifacts", &buf)
	if err != nil {
		return "", fmt.Errorf("library: push %s: %w", name, err)
	}
	req.Header.Set("Authorization", "Bearer "+l.bearer())
	req.Header.Set("Content-Type", writer.FormDataContentType())

	resp, err := l.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("library: push %s: %w", name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		if resp.StatusCode == http.StatusForbidden {
			return "", fmt.Errorf("library: push %s: the owning user cannot write to the Library (403) -- "+
				"an access question for that user, not a cockpit misconfiguration", name)
		}
		return "", fmt.Errorf("library: push %s: %s", name, strings.TrimSpace(resp.Status))
	}
	var out PushResult
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("library: push %s: decode response: %w", name, err)
	}
	if strings.TrimSpace(out.ArtifactId) == "" {
		return "", fmt.Errorf("library: push %s: server returned no artifact id", name)
	}
	return out.ArtifactId, nil
}

// The multipart field names the Library's upload route reads.
const (
	libraryFormFileKey = "file"
	libraryFormNameKey = "name"
)

// producedFile is one candidate for the push at session end.
type producedFile struct {
	path string
	rel  string
	size int64
}

// collectProduced walks the workspace for files the run created or
// changed, measured against a snapshot taken before it started.
//
// Bounded on purpose, and the caller LOGS what was dropped. A silent
// truncation reads downstream as "this is everything the run produced",
// which is the one thing a record must never be wrong about.
func collectProduced(workspace string, before map[string]fileStamp) ([]producedFile, []string) {
	var out []producedFile
	var skipped []string

	_ = filepath.WalkDir(workspace, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		name := d.Name()
		if d.IsDir() {
			if path == workspace {
				return nil
			}
			if pushExcludedDirs[name] {
				return filepath.SkipDir
			}
			// The session's own scaffolding is not output.
			if name == ".memql-session" {
				return filepath.SkipDir
			}
			return nil
		}
		if name == ".mcp.json" || strings.HasSuffix(name, backupSuffix) {
			return nil
		}
		info, statErr := d.Info()
		if statErr != nil || !info.Mode().IsRegular() {
			return nil
		}
		rel, relErr := filepath.Rel(workspace, path)
		if relErr != nil {
			return nil
		}
		if prior, existed := before[rel]; existed && prior.matches(info) {
			return nil
		}
		if info.Size() > maxPushFileBytes {
			skipped = append(skipped, fmt.Sprintf("%s (%d bytes, over the %d-byte per-file cap)",
				rel, info.Size(), maxPushFileBytes))
			return nil
		}
		out = append(out, producedFile{path: path, rel: rel, size: info.Size()})
		return nil
	})

	// Deterministic order, so two runs that produced the same files
	// report their ids in the same order.
	sort.Slice(out, func(i, j int) bool { return out[i].rel < out[j].rel })
	if len(out) > maxPushFiles {
		for _, f := range out[maxPushFiles:] {
			skipped = append(skipped, fmt.Sprintf("%s (over the %d-file cap)", f.rel, maxPushFiles))
		}
		out = out[:maxPushFiles]
	}
	return out, skipped
}

// artifactName flattens a workspace-relative path into one path segment.
//
// The Library stores an artifact under the LAST segment of the name it is
// given -- multipart's own reader applies filepath.Base to a part
// filename, and the storage key is a single segment either way. So a
// nested path sent verbatim arrives as its basename, and a run that wrote
// both `api/schema.json` and `web/schema.json` would push two artifacts
// indistinguishably named `schema.json`.
//
// Flattening keeps the location in the name instead of throwing it away.
// The separator is doubled so it cannot be confused with an underscore
// the file itself had.
func artifactName(rel string) string {
	flat := strings.ReplaceAll(filepath.ToSlash(rel), "/", "__")
	if name := sanitizeFileName(flat); name != "" {
		return name
	}
	return "artifact"
}

// fileStamp identifies a file well enough to notice a rewrite.
type fileStamp struct {
	size    int64
	modTime time.Time
}

func (s fileStamp) matches(info os.FileInfo) bool {
	return s.size == info.Size() && s.modTime.Equal(info.ModTime())
}

// snapshotWorkspace records the workspace as it stood before the run, so
// the push at the end carries what the run PRODUCED rather than
// everything that happens to be in the directory.
func snapshotWorkspace(workspace string) map[string]fileStamp {
	out := make(map[string]fileStamp)
	_ = filepath.WalkDir(workspace, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if path != workspace && (pushExcludedDirs[d.Name()] || d.Name() == ".memql-session") {
				return filepath.SkipDir
			}
			return nil
		}
		info, statErr := d.Info()
		if statErr != nil || !info.Mode().IsRegular() {
			return nil
		}
		rel, relErr := filepath.Rel(workspace, path)
		if relErr != nil {
			return nil
		}
		out[rel] = fileStamp{size: info.Size(), modTime: info.ModTime()}
		return nil
	})
	return out
}

// sanitizeFileName reduces a server-supplied name to a single safe path
// segment. A Content-Disposition filename is attacker-influenced input in
// the general case, and this one lands inside somebody's workspace.
func sanitizeFileName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	name = filepath.Base(filepath.FromSlash(name))
	if name == "." || name == ".." || name == string(filepath.Separator) {
		return ""
	}
	name = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || r == '/' || r == '\\' {
			return '_'
		}
		return r
	}, name)
	if len(name) > 200 {
		name = name[:200]
	}
	return name
}
