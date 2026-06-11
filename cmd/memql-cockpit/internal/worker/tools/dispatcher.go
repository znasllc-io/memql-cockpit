// Package tools holds the cockpit-side tool implementations the
// worker dispatches against. Headless tools (workerHost.*) are
// always built. GUI tools (workerComputer.*) live behind a build
// tag and reduce to "Unimplemented" on the default build.
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"runtime/debug"
	"strings"
	"time"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"

	"github.com/znasllc-io/memql-cockpit/cmd/memql-cockpit/worker/consent"
)

// ConsentGate is the narrow interface the dispatcher uses to ask
// the consent manager whether a tool dispatch may run right now.
// Pulled out as an interface so tests can wire a stub without
// pulling in the full consent package surface.
//
// AllowsAt carries the cursor position so the strict-mode region
// exemption (memql-cockpit#131) can fire for workerComputer.mouse_click.
// Allows is the no-cursor convenience for every other call.
type ConsentGate interface {
	Allows(tool, action string) consent.Decision
	AllowsAt(tool, action string, cursor consent.CursorPoint) consent.Decision
}

// Dispatcher is the cockpit-side tool dispatcher. Implements
// internal/worker.ToolDispatcher.
type Dispatcher struct {
	logger  *slog.Logger
	policy  *Policy
	consent ConsentGate
}

// NewDispatcher constructs a Dispatcher. The policy controls
// allow/deny decisions for shell exec / fs / http tools.
//
// When consentGate is nil, the dispatcher behaves the pre-#64 way
// (every dispatched call runs). When non-nil, every call passes
// through the gate first; a denied decision short-circuits with a
// `consent_required` failure carrying the gate's reason string.
func NewDispatcher(logger *slog.Logger, policy *Policy, consentGate ConsentGate) *Dispatcher {
	if logger == nil {
		logger = slog.Default()
	}
	if policy == nil {
		policy = DefaultPolicy()
	}
	return &Dispatcher{logger: logger, policy: policy, consent: consentGate}
}

// Dispatch routes a ToolDispatch to its handler.
//
// Verbose telemetry per call: entry log carries tool + action +
// args-byte count + a redacted args preview; exit log carries
// outcome (success / errorCode) + duration + result-bytes when
// successful. This is the operator's primary surface for
// "what is the worker actually doing right now?" Until we
// upgrade the cockpit to a TUI dashboard, the slog stream IS
// the dashboard.
//
// Panic recovery: robotgo (and any cgo-backed tool path) can
// panic on permission-denial / environment-mismatch corner
// cases. Without recovery the panic propagates up the worker's
// goroutine and tears down the gRPC stream, surfacing on the
// agent side as the cryptic `worker_disconnected` error. The
// recover here turns a panic into a structured Failure with
// the stack on the cockpit's stderr so the operator can see
// what went wrong without the worker dying.
func (d *Dispatcher) Dispatch(ctx context.Context, dispatch *memqlv1.ToolDispatch) (success *memqlv1.Success, failure *memqlv1.Failure) {
	if dispatch == nil {
		return nil, &memqlv1.Failure{ErrorCode: "bad_request", ErrorMessage: "nil dispatch"}
	}

	tool := dispatch.GetTool()
	action := dispatch.GetAction()
	rawArgs := dispatch.GetArgsJson()
	callId := dispatch.GetCallId()
	startedAt := time.Now()

	defer func() {
		if rec := recover(); rec != nil {
			stack := string(debug.Stack())
			d.logger.Error("worker tool dispatch PANIC -- recovered to keep the worker stream alive",
				"tool", tool,
				"action", action,
				"call_id", callId,
				"panic", fmt.Sprintf("%v", rec),
				"stack", stack,
			)
			success = nil
			failure = &memqlv1.Failure{
				ErrorCode:    "tool_panic",
				ErrorMessage: fmt.Sprintf("worker tool %s.%s panicked: %v", tool, action, rec),
			}
		}
	}()

	d.logger.Info("worker tool dispatch START",
		"tool", tool,
		"action", action,
		"call_id", callId,
		"args_bytes", len(rawArgs),
		"args_preview", redactedArgsPreview(rawArgs),
	)

	args, err := decodeArgs(rawArgs, action)
	if err != nil {
		d.logger.Warn("worker tool dispatch FAIL (bad_request: decode args)",
			"tool", tool,
			"action", action,
			"call_id", callId,
			"error", err,
		)
		return nil, &memqlv1.Failure{ErrorCode: "bad_request", ErrorMessage: err.Error()}
	}

	// Consent gate (memql-cockpit#64). Every workerHost /
	// workerComputer call passes through the per-host consent
	// manager. Without an active operator-granted window the call
	// short-circuits with `consent_required` carrying the gate's
	// hint about how to open one. When d.consent == nil (no gate
	// wired, e.g. legacy tests) we behave the pre-#64 way.
	if d.consent != nil {
		// For workerComputer.mouse_click, resolve the live cursor
		// position so the strict-mode region exemption can fire
		// (memql-cockpit#131). mouse_click carries no coordinates of
		// its own -- positioning happens via a preceding mouse_move
		// -- so the region check reads where the cursor actually is
		// at dispatch time. cursorLocation() is GUI-build-backed;
		// the headless stub returns Known=false, which AllowsAt
		// treats as out-of-region (falls through to the gate).
		var dec consent.Decision
		if tool == "workerComputer" && action == "mouse_click" {
			cx, cy, ok := cursorLocation()
			dec = d.consent.AllowsAt(tool, action, consent.CursorPoint{X: cx, Y: cy, Known: ok})
		} else {
			dec = d.consent.Allows(tool, action)
		}
		if !dec.Allowed {
			elapsedMs := time.Since(startedAt).Milliseconds()
			d.logger.Warn("worker tool dispatch FAIL (consent_required)",
				"tool", tool,
				"action", action,
				"call_id", callId,
				"reason", dec.Reason,
				"elapsed_ms", elapsedMs,
			)
			return nil, &memqlv1.Failure{
				ErrorCode:    "consent_required",
				ErrorMessage: dec.Reason,
			}
		}
	}

	switch tool {
	case "workerHost":
		success, failure = d.dispatchHost(ctx, action, args)
	case "workerComputer":
		// `capabilities` is build-agnostic introspection
		// (memql-cockpit#162): route it BEFORE the per-build
		// dispatchComputer so the headless build answers honestly
		// (guiAvailable=false, actions=[]) instead of rejecting
		// with gui_unavailable. It is the one workerComputer action
		// that must work on every build variant.
		if action == computerCapabilitiesAction {
			success, failure = runComputerCapabilities()
		} else {
			success, failure = d.dispatchComputer(ctx, action, args)
		}
	default:
		failure = &memqlv1.Failure{ErrorCode: "unknown_tool", ErrorMessage: fmt.Sprintf("unknown tool %q", tool)}
	}

	elapsedMs := time.Since(startedAt).Milliseconds()
	if failure != nil {
		d.logger.Warn("worker tool dispatch FAIL",
			"tool", tool,
			"action", action,
			"call_id", callId,
			"error_code", failure.GetErrorCode(),
			"error_message", failure.GetErrorMessage(),
			"elapsed_ms", elapsedMs,
		)
	} else if success != nil {
		d.logger.Info("worker tool dispatch OK",
			"tool", tool,
			"action", action,
			"call_id", callId,
			"result_bytes", len(success.GetResultJson()),
			"output_preview", success.GetOutputPreview(),
			"elapsed_ms", elapsedMs,
		)
	}
	return success, failure
}

// redactedArgsPreview returns a short, redacted preview of the raw
// args JSON for log lines. Strips known secret-shaped fields
// (token / password / authorization headers, anything under the
// "secret" key family) and clamps the rest to 256 bytes so the
// log line stays readable. Use this in entry-logs only -- the
// full args land in the policy layer's per-action audit if needed.
func redactedArgsPreview(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	var probe map[string]any
	if err := json.Unmarshal(raw, &probe); err != nil {
		// Not JSON or malformed -- return a clamped raw string.
		s := string(raw)
		if len(s) > 256 {
			s = s[:256] + "...(truncated)"
		}
		return s
	}
	redactSecrets(probe)
	out, _ := json.Marshal(probe)
	if len(out) > 256 {
		return string(out[:256]) + "...(truncated)"
	}
	return string(out)
}

// redactSecrets walks a decoded JSON map in place and replaces any
// value under a known-secret-shaped key with "[REDACTED]". Conservative
// match: case-insensitive substring on token / password / authorization
// / secret / api_key.
func redactSecrets(m map[string]any) {
	for k, v := range m {
		if isSecretKey(k) {
			m[k] = "[REDACTED]"
			continue
		}
		switch nested := v.(type) {
		case map[string]any:
			redactSecrets(nested)
		case []any:
			for _, item := range nested {
				if im, ok := item.(map[string]any); ok {
					redactSecrets(im)
				}
			}
		}
	}
}

func isSecretKey(k string) bool {
	lower := strings.ToLower(k)
	for _, needle := range []string{"token", "password", "authorization", "secret", "api_key", "apikey"} {
		if strings.Contains(lower, needle) {
			return true
		}
	}
	return false
}

func (d *Dispatcher) dispatchHost(ctx context.Context, action string, args map[string]any) (*memqlv1.Success, *memqlv1.Failure) {
	switch action {
	case "exec":
		return runExec(ctx, args, d.policy)
	case "fs_read":
		return runFsRead(ctx, args, d.policy)
	case "fs_write":
		return runFsWrite(ctx, args, d.policy)
	case "fs_list":
		return runFsList(ctx, args, d.policy)
	case "fs_stat":
		return runFsStat(ctx, args, d.policy)
	case "http_fetch":
		return runHTTPFetch(ctx, args, d.policy)
	}
	return nil, &memqlv1.Failure{
		ErrorCode:    "unknown_action",
		ErrorMessage: fmt.Sprintf("workerHost.%s not implemented", action),
	}
}

// decodeArgs unwraps the discriminated-union envelope:
//
//	{"action": "exec", "exec": { ... }}
//
// and returns the inner per-action map. Falls back to the outer
// object when the inner key is absent.
func decodeArgs(raw []byte, action string) (map[string]any, error) {
	if len(raw) == 0 {
		return map[string]any{}, nil
	}
	var outer map[string]any
	if err := json.Unmarshal(raw, &outer); err != nil {
		return nil, fmt.Errorf("decode args: %w", err)
	}
	if inner, ok := outer[action].(map[string]any); ok {
		return inner, nil
	}
	// Treat the outer map (minus the action discriminator) as the args.
	out := make(map[string]any, len(outer))
	for k, v := range outer {
		if k == "action" {
			continue
		}
		out[k] = v
	}
	return out, nil
}

func argString(args map[string]any, key string) string {
	if v, ok := args[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

func argStringList(args map[string]any, key string) []string {
	v, ok := args[key]
	if !ok {
		return nil
	}
	list, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(list))
	for _, item := range list {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func argInt(args map[string]any, key string, def int) int {
	v, ok := args[key]
	if !ok {
		return def
	}
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	}
	return def
}

func argMap(args map[string]any, key string) map[string]any {
	v, ok := args[key]
	if !ok {
		return nil
	}
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return nil
}

func successJSON(payload map[string]any, exit int, bytesIn, bytesOut int, preview string) *memqlv1.Success {
	body, _ := json.Marshal(payload)
	return &memqlv1.Success{
		ResultJson:    body,
		ExitCode:      int32(exit),
		BytesIn:       uint64(bytesIn),
		BytesOut:      uint64(bytesOut),
		OutputPreview: clampPreview(preview),
	}
}

func failure(code, message string) *memqlv1.Failure {
	return &memqlv1.Failure{ErrorCode: code, ErrorMessage: message}
}

func clampPreview(s string) string {
	const limit = 1024
	if len(s) <= limit {
		return s
	}
	return s[:limit]
}

func cleanString(s string) string { return strings.TrimSpace(s) }
