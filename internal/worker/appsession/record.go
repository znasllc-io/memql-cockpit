package appsession

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/znasllc-io/memql-cockpit/internal/worker/harness"
)

// record.go is the session's half of the recording (memql-cockpit#440).
//
// The harness decides WHICH files a call touched, out of the app's own
// protocol; this file decides whether their bytes leave this machine, and
// then sends the call on as an `event` chunk the transcript cap never
// swallows. The split is the point: what a protocol says and what this
// machine allows are different questions, and only the second is the
// machine owner's.

// sessionScaffoldDir is the session's own directory inside the workspace:
// the transcript, and for Codex the per-session CODEX_HOME with the bearer
// in its config.toml and the user's auth.json linked into it.
const sessionScaffoldDir = ".memql-session"

// The recording's ceilings.
const (
	// maxInlineFileBytes is the largest file whose bytes travel inside its
	// action -- the line length this package's process reader and the
	// harness already stop at.
	maxInlineFileBytes int64 = 1 << 20
	// maxInlineEventBytes bounds what one action's files carry inline
	// together. The stream is shared with every other message this machine
	// sends, and a file change touching forty files must not become one
	// forty-megabyte message holding up a heartbeat.
	maxInlineEventBytes int64 = 4 << 20
	// maxDigestFileBytes is the largest file the recording hashes; past it
	// the size is reported and nothing is read. The same ceiling the
	// produced-file push draws.
	maxDigestFileBytes = maxPushFileBytes
)

// sessionSink is the harness.Sink every turn of a session writes to.
type sessionSink struct{ s *session }

// Chunk sends narration the way it always went: redacted, numbered, and
// under the transcript cap.
func (k sessionSink) Chunk(stream string, data []byte) {
	_ = k.s.emitChunk(stream, data)
}

// Record sends one action.
//
// UNCAPPED, and that is why the recording has a method of its own.
// limits.max_transcript_bytes bounds the NARRATION the engine keeps on the
// session row; an action is not narration, it is the record of what the
// app did, and dropping the fortieth call because the app was chatty
// about the first thirty-nine would record a session that stopped doing
// things halfway through (emitRecord).
func (k sessionSink) Record(a harness.Action) {
	k.s.recordAction(a)
}

// recordAction reads the files the action names, under this machine's
// policy, and sends it.
func (s *session) recordAction(a harness.Action) {
	s.contentPolicy().fill(a.Contents)
	body, err := json.Marshal(a)
	if err != nil {
		s.logger.Warn("an app action could not be encoded for the recording",
			"id", a.ID, "seq", a.Seq, "error", err)
		return
	}
	// A send that fails for good leaves a hole in the action seq, and the
	// seq is dense, so that hole is how the engine learns a call is missing.
	if err := s.emitRecord(append(body, '\n')); err != nil {
		s.logger.Warn("an app action could not be sent", "id", a.ID, "seq", a.Seq, "error", err)
	}
}

func (s *session) contentPolicy() *contentPolicy {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.policy
}

// contentPolicy decides which of the files an app names this machine
// reads back into the recording.
//
// THE APP NAMES THE PATH, AND THE APP IS DRIVEN BY A PROMPT FROM SOMEWHERE
// ELSE. So the path is untrusted input, the stance CheckWorkspace takes
// toward the workspace itself: a file is read only if it resolves --
// symlinks followed -- to somewhere inside this session's workspace, is
// not the session's own scaffolding (the MCP configuration carrying the
// per-run bearer, the transcript, Codex's per-session home with the
// user's auth.json linked into it), and is a regular file, checked on the
// OPEN descriptor so a path swapped for a pipe cannot hang the session.
// Anything else is recorded by its path and the reason, and nothing is
// read.
//
// What this does NOT claim: an app that wants a file off this machine can
// print it into its own tool results, which travel verbatim as they
// always have. The policy's job is narrower and still worth doing -- the
// recording itself never widens what leaves, by reading a file the app
// named but did not reach, or one a symlink led out of the workspace.
type contentPolicy struct {
	root     string
	excluded []string
}

// newContentPolicy builds the policy for a workspace, never reading the
// paths in excluded or anything beneath them. Every path is compared
// resolved, so the answer is the same on macOS, where /tmp is itself a
// link.
func newContentPolicy(workspace string, excluded ...string) *contentPolicy {
	p := &contentPolicy{root: resolvedPath(workspace)}
	for _, e := range excluded {
		if strings.TrimSpace(e) != "" {
			p.excluded = append(p.excluded, resolvedPath(e))
		}
	}
	return p
}

// resolve returns the file to open for path, or the reason not to.
func (p *contentPolicy) resolve(path string) (string, string) {
	if !filepath.IsAbs(path) {
		path = filepath.Join(p.root, path)
	}
	real := resolvedPath(path)
	if !within(p.root, real) {
		return "", harness.OmittedOutsideWorkspace
	}
	for _, e := range p.excluded {
		if within(e, real) {
			return "", harness.OmittedScaffolding
		}
	}
	if strings.HasSuffix(real, backupSuffix) {
		// A configuration the session moved aside is the person's own,
		// held out of the way until the session restores it -- not part of
		// the run.
		return "", harness.OmittedScaffolding
	}
	return real, ""
}

// fill reads the files an action names, in place, under one inline
// budget for the whole action.
func (p *contentPolicy) fill(contents []harness.Content) {
	if p == nil {
		return
	}
	budget := maxInlineEventBytes
	for i := range contents {
		p.read(&contents[i], &budget)
	}
}

func (p *contentPolicy) read(c *harness.Content, budget *int64) {
	if c.Op == harness.ContentDelete {
		// Nothing is left at the path to read, by definition.
		return
	}
	real, why := p.resolve(c.Path)
	if why != "" {
		c.Omitted = why
		return
	}
	got := readForRecord(real, maxInlineFileBytes)
	c.Bytes, c.Digest, c.Omitted = got.size, got.digest, got.omitted
	switch {
	case got.omitted != "":
	case got.data == nil:
		c.Omitted = harness.OmittedOverCeiling
	case int64(len(got.data)) > *budget:
		c.Omitted = harness.OmittedOverBudget
	default:
		*budget -= int64(len(got.data))
		c.Encoding, c.Data = encodeInline(got.data)
	}
}

// fileRead is what readForRecord learned about one file.
type fileRead struct {
	size   *int64
	digest string
	// data is the file's bytes when there were at most keepMax of them.
	data    []byte
	omitted string
}

// readForRecord hashes one file and keeps its bytes when they fit.
func readForRecord(path string, keepMax int64) fileRead {
	f, err := openForRecord(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fileRead{omitted: harness.OmittedNotFound}
		}
		return fileRead{omitted: harness.OmittedUnreadable}
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return fileRead{omitted: harness.OmittedUnreadable}
	}
	if !info.Mode().IsRegular() {
		return fileRead{omitted: harness.OmittedNotRegular}
	}
	size := info.Size()
	if size > maxDigestFileBytes {
		return fileRead{size: &size, omitted: harness.OmittedTooLarge}
	}
	keep := &capBuffer{max: keepMax}
	if size > keepMax {
		keep.max = 0
	}
	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(hash, keep), io.LimitReader(f, maxDigestFileBytes+1))
	if err != nil {
		return fileRead{omitted: harness.OmittedUnreadable}
	}
	if n > maxDigestFileBytes {
		// It grew while it was being read.
		return fileRead{size: &n, omitted: harness.OmittedTooLarge}
	}
	out := fileRead{size: &n, digest: harness.DigestPrefix + hex.EncodeToString(hash.Sum(nil))}
	if n <= keepMax && int64(len(keep.buf)) == n {
		out.data = keep.buf
		if out.data == nil {
			out.data = []byte{}
		}
	}
	return out
}

// encodeInline carries bytes as the text they are, when they are text,
// and as base64 otherwise.
func encodeInline(data []byte) (string, *string) {
	if utf8.Valid(data) {
		s := string(data)
		return harness.EncodingUTF8, &s
	}
	s := base64.StdEncoding.EncodeToString(data)
	return harness.EncodingBase64, &s
}

// capBuffer keeps at most max bytes and reports every write whole, so the
// hash beside it in a MultiWriter still sees the entire file.
type capBuffer struct {
	buf []byte
	max int64
}

func (b *capBuffer) Write(p []byte) (int, error) {
	if room := b.max - int64(len(b.buf)); room > 0 {
		if int64(len(p)) > room {
			b.buf = append(b.buf, p[:room]...)
		} else {
			b.buf = append(b.buf, p...)
		}
	}
	return len(p), nil
}

// resolvedPath resolves symlinks as far as the path exists and keeps the
// rest as written, so a file that does not exist yet still compares
// against the directory it would be in.
func resolvedPath(path string) string {
	clean := filepath.Clean(path)
	if real, err := filepath.EvalSymlinks(clean); err == nil {
		return real
	}
	parent := filepath.Dir(clean)
	if parent == clean {
		return clean
	}
	return filepath.Join(resolvedPath(parent), filepath.Base(clean))
}

// within reports whether path is root or lies beneath it. Both must be
// resolved: the test is lexical, and only sound on paths with no links
// left in them.
func within(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}
