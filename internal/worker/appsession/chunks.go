package appsession

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"

	"github.com/znasllc-io/memql-cockpit/internal/worker/apps"
)

// chunks.go owns everything that leaves the session: the ordered chunks,
// the transcript that becomes an artifact, the usage the app reported
// about itself, and the single End.

// chunkSendAttempts is how many times one chunk is retried before the
// session gives up on the stream.
const chunkSendAttempts = 3

// chunkRetryDelay is the pause between those attempts.
const chunkRetryDelay = 200 * time.Millisecond

// openTranscript starts the on-disk transcript.
//
// It goes to a file rather than a buffer because a run can be an hour
// long and its output is not bounded by anything the cockpit controls;
// holding all of it in memory would make a chatty agent an OOM on
// somebody's laptop. It lives inside the session scaffolding, so it is
// excluded from the produced-file sweep and removed with the session.
func (s *session) openTranscript(workspace string) error {
	path := filepath.Join(workspace, transcriptRel)
	if err := os.MkdirAll(filepath.Dir(path), configDirMode); err != nil {
		return fmt.Errorf("app session: transcript: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("app session: transcript: %w", err)
	}
	s.mu.Lock()
	s.transcript = f
	s.mu.Unlock()
	return nil
}

// emitStdout returns the stdout emitter for one app.
//
// For an app whose headless mode emits newline-delimited JSON, a line
// that parses as an object goes out as an `event` chunk -- the engine
// maps those to progress events -- and anything else goes out as plain
// stdout narration.
//
// It never synthesises an event out of parsed narration. A live view that
// is confidently wrong about what the agent did is worse than a plain
// one: the plain one is obviously prose, and the wrong one is indexed,
// rendered as structure, and believed.
func (s *session) emitStdout(spec apps.Spec) func(string, []byte) error {
	if !spec.StreamsJSON {
		return s.emitChunk
	}
	return func(_ string, data []byte) error {
		trimmed := bytes.TrimSpace(data)
		if len(trimmed) == 0 {
			return nil
		}
		if trimmed[0] == '{' && json.Valid(trimmed) {
			s.recordUsage(trimmed)
			return s.emitChunk(StreamEvent, data)
		}
		return s.emitChunk(StreamStdout, data)
	}
}

// emitChunk records one piece of output and sends it.
//
// The seq is taken ONCE, before the first send attempt, and reused on
// every retry. The engine drops out-of-order and duplicate chunks rather
// than appending them, so renumbering a retry does not "fix" anything --
// it produces a gap the reader cannot see and a record that no longer
// matches what the app printed.
func (s *session) emitChunk(stream string, data []byte) error {
	if len(data) == 0 {
		return nil
	}
	clean := s.redact.apply(data)

	// The transcript artifact carries the FULL output, whatever the
	// engine's row-level cap is. That is the division of labour the
	// engine documents: it keeps a bounded transcript and marks it
	// truncated, and the complete one is expected to be an artifact.
	s.mu.Lock()
	transcript := s.transcript
	s.mu.Unlock()
	if transcript != nil {
		_, _ = fmt.Fprintf(transcript, "[%s] %s", stream, clean)
		if len(clean) > 0 && clean[len(clean)-1] != '\n' {
			_, _ = transcript.Write([]byte("\n"))
		}
	}

	if s.transcriptCapReached(int64(len(clean))) {
		return nil
	}

	s.seqMu.Lock()
	s.seq++
	seq := s.seq
	s.seqMu.Unlock()

	var lastErr error
	for attempt := range chunkSendAttempts {
		if err := s.sender.SendAppSessionChunk(s.id, stream, clean, seq); err != nil {
			lastErr = err
			if attempt+1 < chunkSendAttempts {
				time.Sleep(chunkRetryDelay)
			}
			continue
		}
		return nil
	}
	return lastErr
}

// transcriptCapReached applies limits.max_transcript_bytes to what is
// STREAMED, and says so once when it bites.
//
// The limit bounds what the engine will keep on the session row, so
// sending past it spends bandwidth on bytes that get dropped. Stopping
// silently, though, would leave a reader believing the run went quiet --
// so the cap emits one notice naming itself and pointing at the artifact
// that does have the rest.
func (s *session) transcriptCapReached(n int64) bool {
	max := s.start.GetLimits().GetMaxTranscriptBytes()
	if max <= 0 {
		return false
	}
	s.seqMu.Lock()
	if s.capped {
		s.seqMu.Unlock()
		return true
	}
	s.streamed += n
	if s.streamed <= max {
		s.seqMu.Unlock()
		return false
	}
	s.capped = true
	s.seq++
	seq := s.seq
	s.seqMu.Unlock()

	notice := fmt.Sprintf("[memql] live transcript truncated at limits.max_transcript_bytes (%d); "+
		"the full transcript is pushed to the Library as an artifact at the end of this session\n", max)
	_ = s.sender.SendAppSessionChunk(s.id, StreamStderr, []byte(notice), seq)
	return true
}

// resultEvent is the subset of an app's own final event this reads.
//
// Only what the app STATED. Nothing here is computed from token counts,
// model names or elapsed time: the engine records known=false as billing
// "unknown", which is honest, and a synthesised number would be recorded
// as measured in a ledger somebody bills from.
type resultEvent struct {
	Type      string  `json:"type"`
	CostUSD   float64 `json:"total_cost_usd"`
	SessionId string  `json:"session_id"`
	Usage     *struct {
		InputTokens  int64 `json:"input_tokens"`
		OutputTokens int64 `json:"output_tokens"`
	} `json:"usage"`
}

// recordUsage captures what the app said about its own spend.
func (s *session) recordUsage(line []byte) {
	var ev resultEvent
	if err := json.Unmarshal(line, &ev); err != nil {
		return
	}
	if strings.TrimSpace(ev.SessionId) != "" {
		s.usageMu.Lock()
		s.appRef = ev.SessionId
		s.usageMu.Unlock()
	}
	if ev.Type != "result" {
		return
	}
	if ev.Usage == nil && ev.CostUSD == 0 {
		// A result event that reported no numbers is not a report.
		return
	}
	usage := &memqlv1.AppSessionUsage{CostUsd: ev.CostUSD, Known: true}
	if ev.Usage != nil {
		usage.InputTokens = ev.Usage.InputTokens
		usage.OutputTokens = ev.Usage.OutputTokens
	}
	s.usageMu.Lock()
	s.usage = usage
	s.usageMu.Unlock()
}

// reportedUsage returns what the app told us, or an explicit
// known=false.
//
// The zero-with-known=false case is the important one and is built
// deliberately rather than left nil: it is the difference between "this
// run was free" and "nobody knows what this run cost", and only one of
// those is true.
func (s *session) reportedUsage() *memqlv1.AppSessionUsage {
	s.usageMu.Lock()
	defer s.usageMu.Unlock()
	if s.usage != nil {
		return s.usage
	}
	return &memqlv1.AppSessionUsage{Known: false}
}

// pushOutputs uploads what the run produced plus the full transcript, and
// returns the artifact ids.
func (s *session) pushOutputs(ctx context.Context) ([]string, error) {
	s.mu.Lock()
	library, transcript, before := s.library, s.transcript, s.before
	s.transcript = nil
	s.mu.Unlock()

	transcriptPath := ""
	if transcript != nil {
		transcriptPath = transcript.Name()
		_ = transcript.Close()
	}
	if library == nil {
		return nil, nil
	}

	// The session's own context is already cancelled by the time this
	// runs on the cancel path, and the outputs of a cancelled run are
	// still worth keeping -- an hour of somebody's subscription went
	// into them. So the push gets its own bounded context.
	pushCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), libraryTimeout)
	defer cancel()

	var ids []string
	var failures []string

	workspace := strings.TrimSpace(s.start.GetWorkspace())
	if workspace != "" && before != nil {
		produced, skipped := collectProduced(workspace, before)
		for _, note := range skipped {
			// Never a silent truncation: a bounded list that reads as
			// complete is worse than a shorter one that says so.
			s.logger.Warn("app session output not pushed", "file", note)
			_ = s.emitChunk(StreamStderr, []byte("[memql] not pushed to the Library: "+note+"\n"))
		}
		for _, f := range produced {
			id, err := library.Push(pushCtx, f.path, artifactName(f.rel))
			if err != nil {
				failures = append(failures, err.Error())
				continue
			}
			ids = append(ids, id)
		}
	}

	if transcriptPath != "" {
		body, err := os.ReadFile(transcriptPath)
		if err == nil {
			name := "app-session-" + sanitizeLedgerName(s.id) + "-transcript.log"
			if id, pushErr := library.PushBytes(pushCtx, body, name); pushErr != nil {
				failures = append(failures, pushErr.Error())
			} else {
				ids = append(ids, id)
			}
		}
		_ = os.Remove(transcriptPath)
		_ = os.Remove(filepath.Dir(transcriptPath))
	}

	if len(failures) > 0 {
		return ids, fmt.Errorf("app session: pushing outputs to the Library failed: %s",
			strings.Join(failures, "; "))
	}
	return ids, nil
}

// sendEnd closes the session on the wire, exactly once.
//
// exit_code is the app's REAL code. The engine reads a non-zero exit as a
// FAILED run rather than an ended one, so normalising a 2 to a 1 -- or
// worse, to a 0 -- misfiles the outcome in a record that is read back
// later by people deciding whether the thing worked.
func (s *session) sendEnd(code int, message string, artifacts []string) {
	s.usageMu.Lock()
	ref := s.appRef
	s.usageMu.Unlock()
	if strings.TrimSpace(ref) == "" {
		ref = s.start.GetAppSessionRef()
	}

	end := &memqlv1.AppSessionEnd{
		SessionId:           s.id,
		ExitCode:            int32(code),
		Usage:               s.reportedUsage(),
		AppSessionRef:       ref,
		ProducedArtifactIds: artifacts,
		Error:               s.redact.apply2(message),
	}
	if err := s.sender.SendAppSessionEnd(end); err != nil {
		s.logger.Warn("app session end could not be sent", "error", err)
		return
	}
	s.logger.Info("app session ended",
		"exit_code", code,
		"artifacts", len(artifacts),
		"usage_known", end.GetUsage().GetKnown(),
		"error", message != "",
	)
}
