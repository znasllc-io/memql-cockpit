# Serialize Worker Stream Send Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Every write on the cockpit's one gRPC `WorkerService.Stream` goes through a single mutex, so no two goroutines ever call `SendMsg` (or `SendMsg` and `CloseSend`) on the stream at the same time.

**Architecture:** The lock lives in the SDK's `worker.Connection` (memql `sdk/go/worker/worker.go`), the type that owns the stream, copied from the engine's `streamSession.send` (`component/worker/server.go:1046-1057`: `sendMu` + sticky `sendErr`). The cockpit's `internal/worker.Connection` keeps no lock of its own; instead every one of its `Send*` helpers is routed through its `Send`, which is the only call to the SDK's `Send` in the repository, and a source-scan test plus a compile-time assertion keep it that way. The SDK's raw-stream accessor `Stream()` is deleted (no caller exists in memql, memql-cockpit or memql-bff-copresent), so there is no path around the lock by construction.

**Tech Stack:** Go 1.26.1 (`go` directive in both `go.mod`; cockpit CI toolchain go1.26.6), grpc-go v1.83.2 (`google.golang.org/grpc`), protobuf-go v1.36.12, the race detector (`go test -race`).

**Spec:** Audit finding C-1 in `the audit attached as a comment on znasllc-io/memql#5327 (finding C-1)` (section D "CRITICAL C-1"; architecture map in section A). That file lives in a session scratchpad and will not travel, so the finding and the writer enumeration are reproduced in "The defect" below with every line number re-verified against the trees named there.

**Trees:** cockpit `/home/znas/projects/memql/memql-cockpit`, `main` at `efa804e` (the audit cites `132934b`, the PR #422 merge, which is an ancestor on `main`; the commits between are the v0.13.4 release and two dependabot bumps and touch none of the files below). Engine `/home/znas/projects/memql/memql`, `main` at `5f98c76a1`. `go.work` at `/home/znas/projects/memql` lists both; the cockpit's `go.mod` carries `replace github.com/znasllc-io/memql => ../memql` and keeps it.

**Lifecycle of this file:** cockpit `CLAUDE.md` -- "the plans beside them are deleted by the PR that finishes them; the specs are the record". If this plan is committed, Task 4's cockpit PR removes it (`git rm docs/superpowers/plans/2026-09-13-serialize-worker-stream-send.md`).

## Global Constraints

- Two repositories, two commits minimum, two PRs. **The memql change lands first**: the cockpit consumes the SDK through `replace ../memql` locally and through the pinned sibling checkout in CI (`.github/memql-pin`, currently `275623b3d90d57368d0efec62261705db400a33d`). Task 4 moves the pin to the memql merge commit that carries Tasks 1-2.
- Keep `replace github.com/znasllc-io/memql => ../memql` in the cockpit `go.mod`. Do not add a `go.work` to the cockpit (rejected in `go.mod`'s comment block).
- grpc-go's contract, quoted from `google.golang.org/grpc@v1.83.2/stream.go` (`ClientStream.SendMsg`, `CloseSend`): "it is not safe to call SendMsg on the same stream in different goroutines. It is also not safe to call CloseSend concurrently with SendMsg." and "It is safe to have a goroutine calling SendMsg and another goroutine calling RecvMsg on the same stream at the same time." So `Recv` stays outside the lock.
- memql branch rules (memql `CLAUDE.md`, "Branch Workflow"): every change through a branch + PR; enqueue with the bare `gh pr merge <n> --repo znasllc-io/memql` (no `--merge`, no `--delete-branch`); stage files by explicit path, never `git add -A`; pre-release means no back-compat shims, so `Stream()` is deleted, not deprecated.
- memql SDK rules (`sdk/go/CLAUDE.md`): the exported SDK surface must not name `memqlv1.*` types except the declared transport seam; `worker.Connection` is in `protoSeamAllowlist` (root `sdk_proto_leak_test.go`) and the allowlist is checked in both directions, so the entry must keep matching at least one exported symbol (it will: `Send` and `Recv` remain).
- memql tests: `go test ./...` does not reach the engine, but `sdk/go/worker` is in the root module, so `go test ./sdk/go/worker/...` from the memql root is correct for this package; `make test` (`go test github.com/znasllc-io/memql/...`) is the whole tree.
- cockpit tests: single module, `go test ./...` is the whole suite (`make test`); `make lint` = `go fmt` + `go vet`.
- Commit identity `znas <znas@znas.io>` (check `git config user.email` before the first commit). Every commit message ends with:
  ```
  Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
  Claude-Session: https://claude.ai/code/session_01RTCw4Xs3am16N2ujcR56aP
  ```
  Every PR description ends with:
  ```
  🤖 Generated with [Claude Code](https://claude.com/claude-code)

  https://claude.ai/code/session_01RTCw4Xs3am16N2ujcR56aP
  ```
- No emojis in source, docs or test output (repository convention; the PR footer above is the one mandated exception).
- Commit message style: cockpit `fix(worker): ...` / `test(worker): ...` / `chore(worker): ...`; memql `fix(sdk): ...`.

---

## The defect, verified

`internal/worker/connect.go:184-189` (cockpit) and `sdk/go/worker/worker.go:193-198` (memql) are bare pass-throughs to `stream.Send`. Neither struct holds a mutex (`grep -n "sync.Mutex\|sendMu"` in both files: none). The stream is one `grpc.BidiStreamingClient` (`WorkerService_StreamClient` is a type alias for it, `component/grpc/gen/worker_grpc.pb.go:57`), and these goroutines write it concurrently, all through the cockpit `Connection`'s helpers and therefore through the SDK `Connection.Send`:

| Writer | Goroutine | Cockpit call site | Helper (connect.go) |
|---|---|---|---|
| Heartbeat every 15 s | `go r.heartbeatLoop(hbCtx, conn)` at `loop.go:254` | `loop.go:360` `conn.SendHeartbeat` | `SendHeartbeat :245-251` -> `c.conn.Send :246` |
| Pong | the recv goroutine (`runStream` loop `loop.go:256-289` -> `handleMessage :537` -> `answerPing :614`) | `loop.go:618` `conn.SendPong` | `SendPong :205-215` -> `c.Send :206` |
| Tool result | one goroutine per dispatch, `loop.go:545-549` -> `runToolDispatch :625` | `loop.go:630`, `:657` `conn.SendToolResult` | `SendToolResult :360-370` -> `c.conn.Send :367` |
| Model-call content deltas | the call's run goroutine (`modelcall/session.go:479` `run`) via `deltaStream.emit :852` | `session.go:874` `s.sender.SendModelCallDelta` | `SendModelCallDelta :339-350` -> `c.conn.Send :340` |
| Model-call keepalive deltas | the call's keepalive ticker goroutine, `session.go:594-605` via `deltaStream.keepalive :856` | `session.go:874` (same line; `deltaStream.mu` at `:812` guards only `seq`/clocks, not the stream) | same |
| Model-call end / refusal | run goroutine | `session.go:212`, `:224`, `:544` `sender.SendModelCallEnd` | `SendModelCallEnd :353-357` -> `c.conn.Send :354` |
| Pull progress / end | the pull goroutine `go m.run(...)` at `modelpull.go:242`, progress from the `inference.Pull` callback | `modelpull.go:258` `SendModelPullProgress`, `:392` `SendModelPullEnd` | `SendModelPullProgress :222-226`, `SendModelPullEnd :229-233` -> `c.Send` |
| App-session chunks / end | per-session goroutines started at `loop.go:561` `r.sessions.Start(ctx, conn, ...)` | `appsession/chunks.go:101`, `:143` `SendAppSessionChunk`; `:361` `SendAppSessionEnd` | `SendAppSessionChunk :313-324` -> `c.conn.Send :314`; `SendAppSessionEnd :327-331` -> `c.conn.Send :328` |
| Register | the `Connect` caller, before any other writer exists (sequential) | `connect.go:150` `c.conn.Send` in `register` | -- |
| **CloseSend** | `Runner.Close` from the Fleet's goroutine, `loop.go:243-245` `conn.Close()`, while every writer above may be mid-`Send` | `connect.go:373-378` -> SDK `Close :210-220` -> `stream.CloseSend :215` | -- |

The `Sender` interfaces the cockpit passes `conn` into are `modelcall.Sender` (`session.go:75-78`), `appsession.Sender` (`appsession/session.go:168-171`) and `pullSender` (`modelpull.go:82-85`); `loop.go:561`, `:568`, `:582` pass the bare `*Connection` for all three.

Callers of the SDK `Connection` methods: the cockpit `connect.go` only (`sdkworker.Dial :71`, then `c.conn.Send/Recv/Close`). `Stream()` (`worker.go:183-188`) has no caller anywhere (`grep -rn "\.Stream()"` over memql, memql-cockpit and memql-bff-copresent: none outside the accessor's own doc comment). The other memql users of `sdk/go/worker` (`component/identity/http/pair.go`, `component/identity/discovery.go`, `component/grpc/server.go`, `core/grpctls`, `worker_dial_tls_contract_test.go`) use `ParseClusterURL` / `BuildTLSConfig` only.

## Decisions

- **D1. A mutex, not a single writer goroutine.** A writer goroutine fed by a channel would need a bounded queue, a drop or block policy when the stream stalls, and a way to return each write's error to its caller (a tool result that failed to send is logged by name at `loop.go:657-661`; a pull progress frame that fails tells the puller the stream is going away at `modelpull.go:265-270`). A mutex gives every caller its own error synchronously, exactly as today, and is what the engine already does on the other end of the same stream.
- **D2. The lock lives in the SDK; the cockpit relies on it.** The SDK type owns the stream, and grpc's invariant is about that object. A second lock in the cockpit would be taken strictly outside the SDK's (cockpit `Send` -> SDK `Send`), so it could not deadlock, but it would protect nothing the SDK's lock does not and would have to be re-invented by every other worker host (`sdk/go/CLAUDE.md`: "No bespoke wire wrappers in the consumer"). What the cockpit proves instead: all of its writers reach the one seam (Task 3).
- **D3. Delete `Stream()`.** It is the only way to write the stream around the lock, it has no caller, and memql is pre-release ("fix both MemQL and the consumer at once and delete what is no longer needed"). A reflection test pins that no method or exported field can hand the raw stream out again (Task 2).
- **D4. Copy the engine's sticky `sendErr`, and add `closed`.** After one failed `Send` the stream is aborted (grpc contract); every later writer gets the first error without touching the stream, as `streamSession.send` does. `closed` is set by `Close` under the lock so a `Send` that lost the race to `Close` answers a named error rather than reaching a half-closed stream.
- **D5. `Close` keeps its order: `CloseSend` under the lock, then `ClientConn.Close`.** That preserves today's graceful half-close as the server sees it (a clean `io.EOF` on its `Recv`, not a transport reset). A `Close` can now wait for an in-flight `Send`; that wait is bounded by the keepalive (`DefaultKeepaliveTime` 30 s + `DefaultKeepaliveTimeout` 10 s) tearing down a transport whose peer stopped reading, and in the ordinary case is microseconds. Rejected: closing the `ClientConn` first to make `Close` never wait -- it changes the disconnect the engine records for every graceful shutdown.
- **D6. The cockpit test uses the cockpit's own `stream` seam, not a bufconn gRPC server.** A real in-process gRPC server would import `google.golang.org/grpc/test/bufconn` (moving grpc from indirect to direct in the cockpit `go.mod`) and the race detector is not guaranteed to observe grpc's internal races on a given run. The `stream` interface (`connect.go:55-59`) exists exactly so a test can stand where the SDK stands.

## File structure

memql:
- Modify `sdk/go/worker/worker.go` -- the lock, `errClosed`, `Close` under the lock (Task 1); remove `Stream()`, rewrite the package doc (Task 2).
- Create `sdk/go/worker/send_serialization_test.go` -- the overlap-detecting fake stream and three `-race` tests (Task 1).
- Create `sdk/go/worker/no_raw_stream_test.go` -- reflection gate: nothing exported hands out a `grpc.ClientStream` (Task 2).

memql-cockpit:
- Modify `internal/worker/connect.go` -- every helper routes through `Send`; compile-time assertion that the SDK type fills the seam (Task 3).
- Create `internal/worker/connect_send_test.go` -- the source-scan guard and the concurrent forwarding test (Task 3).
- Modify `.github/memql-pin` -- move to the memql merge commit (Task 4).
- Modify `.github/workflows/ci.yml:69` -- run the suite under `-race` (Task 4; verified race-clean today, 50 s wall locally).

---

### Task 1: SDK `Connection` serializes `Send` and `CloseSend` under one lock

**Files:**
- Modify: `/home/znas/projects/memql/memql/sdk/go/worker/worker.go:32-45` (imports), `:96-103` (struct), `:190-198` (`Send`), `:200-206` (`Recv`), `:208-220` (`Close`)
- Test: `/home/znas/projects/memql/memql/sdk/go/worker/send_serialization_test.go` (create)

**Interfaces:**
- Consumes: `memqlv1.WorkerService_StreamClient` (= `grpc.BidiStreamingClient[memqlv1.WorkerClientMessage, memqlv1.WorkerServerMessage]`), unchanged.
- Produces: `func (c *Connection) Send(msg *memqlv1.WorkerClientMessage) error` -- unchanged signature, now safe from any goroutine and serialized against `Close`; `func (c *Connection) Close()` -- unchanged signature, `CloseSend` taken under the same lock; package-private `var errClosed error` (`"sdk/worker: connection is closed"`), what `Send`/`Recv` answer on a nil or closed connection. Task 3's cockpit code and Task 2's test depend on the names `Send`, `Recv`, `Close` only.

- [ ] **Step 1: Write the failing tests**

Create `/home/znas/projects/memql/memql/sdk/go/worker/send_serialization_test.go`:

```go
package worker

import (
	"errors"
	"io"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/timestamppb"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

// overlapDetectingStream stands where grpc-go's clientStream would be and
// enforces the rule this file exists for, in grpc-go's own words
// (google.golang.org/grpc/stream.go, ClientStream): "it is not safe to
// call SendMsg on the same stream in different goroutines. It is also not
// safe to call CloseSend concurrently with SendMsg."
//
// Every Send and CloseSend bumps an in-flight counter on entry, yields so
// a racing writer gets its turn, and drops the counter on exit. An entry
// that finds the counter already non-zero is an OVERLAP, which is the
// defect, and it fails the test with or without -race. The frame log is
// appended WITHOUT a lock on purpose: under -race an unserialized pair of
// writers is reported as a data race on it, which is the same fact stated
// by the detector instead of the counter. A CloseSend lands in the same
// log as a nil frame, so a CloseSend racing a Send is a race on the same
// memory.
type overlapDetectingStream struct {
	// The embedded interface is nil. Header / Trailer / Context / SendMsg /
	// RecvMsg are never reached: Connection calls Send, Recv and CloseSend
	// and nothing else.
	grpc.ClientStream

	inFlight atomic.Int32
	overlaps atomic.Int32
	// sendErr, when set, is what Send answers instead of recording.
	sendErr error

	frames []*memqlv1.WorkerClientMessage // a nil entry is a CloseSend
}

func (s *overlapDetectingStream) enter() {
	if s.inFlight.Add(1) != 1 {
		s.overlaps.Add(1)
	}
	runtime.Gosched()
}

func (s *overlapDetectingStream) leave() { s.inFlight.Add(-1) }

func (s *overlapDetectingStream) Send(m *memqlv1.WorkerClientMessage) error {
	s.enter()
	defer s.leave()
	if s.sendErr != nil {
		return s.sendErr
	}
	s.frames = append(s.frames, m)
	return nil
}

func (s *overlapDetectingStream) CloseSend() error {
	s.enter()
	defer s.leave()
	s.frames = append(s.frames, nil)
	return nil
}

func (s *overlapDetectingStream) Recv() (*memqlv1.WorkerServerMessage, error) {
	return nil, io.EOF
}

func (s *overlapDetectingStream) closes() int {
	n := 0
	for _, f := range s.frames {
		if f == nil {
			n++
		}
	}
	return n
}

// The three writers the cockpit runs on one stream at once: the heartbeat
// goroutine, the recv goroutine answering a Ping, and a model call's delta
// stream (memql-cockpit internal/worker/loop.go and modelcall/session.go).
func heartbeatFrame(i int) *memqlv1.WorkerClientMessage {
	return &memqlv1.WorkerClientMessage{Payload: &memqlv1.WorkerClientMessage_Heartbeat{
		Heartbeat: &memqlv1.Heartbeat{Ts: timestamppb.Now(), ActiveCallsTotal: uint32(i)},
	}}
}

func pongFrame(i int) *memqlv1.WorkerClientMessage {
	return &memqlv1.WorkerClientMessage{Payload: &memqlv1.WorkerClientMessage_Pong{
		Pong: &memqlv1.Pong{RequestId: "ping-" + strconv.Itoa(i), SentAt: timestamppb.Now(), ReceivedAt: timestamppb.Now()},
	}}
}

func deltaFrame(i int) *memqlv1.WorkerClientMessage {
	return &memqlv1.WorkerClientMessage{Payload: &memqlv1.WorkerClientMessage_ModelCallDelta{
		ModelCallDelta: &memqlv1.ModelCallDelta{RequestId: "call-1", Seq: uint64(i), Content: "tok", Keepalive: i%7 == 0},
	}}
}

const framesPerWriter = 500

func TestSendIsSerializedAcrossWriters(t *testing.T) {
	stream := &overlapDetectingStream{}
	conn := &Connection{stream: stream}

	var wg sync.WaitGroup
	writer := func(frame func(int) *memqlv1.WorkerClientMessage) {
		defer wg.Done()
		for i := 0; i < framesPerWriter; i++ {
			if err := conn.Send(frame(i)); err != nil {
				t.Errorf("Send: %v", err)
				return
			}
		}
	}
	wg.Add(3)
	go writer(heartbeatFrame)
	go writer(pongFrame)
	go writer(deltaFrame)
	wg.Wait()

	if n := stream.overlaps.Load(); n != 0 {
		t.Fatalf("%d Send calls entered the stream while another was in flight; grpc-go forbids concurrent SendMsg on one stream", n)
	}
	if got, want := len(stream.frames), 3*framesPerWriter; got != want {
		t.Fatalf("stream recorded %d frames, want %d", got, want)
	}
}

func TestCloseSendIsSerializedWithSend(t *testing.T) {
	stream := &overlapDetectingStream{}
	conn := &Connection{stream: stream}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < framesPerWriter; i++ {
			if err := conn.Send(heartbeatFrame(i)); err != nil {
				// The connection was closed under this writer: the
				// expected end.
				return
			}
		}
	}()
	runtime.Gosched()
	conn.Close()
	wg.Wait()

	if n := stream.overlaps.Load(); n != 0 {
		t.Fatalf("CloseSend overlapped a Send %d time(s); grpc-go forbids CloseSend concurrently with SendMsg", n)
	}
	if got := stream.closes(); got != 1 {
		t.Fatalf("CloseSend was called %d times, want exactly 1", got)
	}
	if err := conn.Send(heartbeatFrame(0)); err == nil {
		t.Fatal("Send after Close succeeded; want an error, and no frame on a half-closed stream")
	}
}

func TestAFailedSendIsStickyAndNamesTheFirstError(t *testing.T) {
	boom := errors.New("transport is closing")
	stream := &overlapDetectingStream{}
	conn := &Connection{stream: stream}

	if err := conn.Send(heartbeatFrame(0)); err != nil {
		t.Fatalf("first Send: %v", err)
	}
	stream.sendErr = boom
	if err := conn.Send(heartbeatFrame(1)); !errors.Is(err, boom) {
		t.Fatalf("failing Send = %v, want %v", err, boom)
	}
	stream.sendErr = nil
	if err := conn.Send(heartbeatFrame(2)); !errors.Is(err, boom) {
		t.Fatalf("Send after a failure = %v, want the first error %v, sticky: on error SendMsg has aborted the stream", err, boom)
	}
	if got := len(stream.frames); got != 1 {
		t.Fatalf("stream recorded %d frames after the failure, want 1: a dead stream is not written again", got)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run (from `/home/znas/projects/memql/memql`):

```bash
go test -race -count=1 -run 'TestSendIsSerializedAcrossWriters|TestCloseSendIsSerializedWithSend|TestAFailedSendIsStickyAndNamesTheFirstError' ./sdk/go/worker/
```

Expected: FAIL. `TestSendIsSerializedAcrossWriters` prints `WARNING: DATA RACE` blocks naming `overlapDetectingStream.Send` from two goroutines, then `testing.go: race detected during execution of test`, and/or `N Send calls entered the stream while another was in flight`. `TestCloseSendIsSerializedWithSend` fails with `Send after Close succeeded` (and may also report the CloseSend overlap). `TestAFailedSendIsStickyAndNamesTheFirstError` fails with `Send after a failure = <nil>, want the first error transport is closing, sticky`.

Also run once without `-race` to confirm the overlap counter alone catches the first test:

```bash
go test -count=1 -run TestSendIsSerializedAcrossWriters ./sdk/go/worker/
```

Expected: FAIL with `... Send calls entered the stream while another was in flight`.

- [ ] **Step 3: Write the implementation**

In `/home/znas/projects/memql/memql/sdk/go/worker/worker.go`, change the import block (`:32-45`) to add `errors` and `sync`:

```go
import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/metadata"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)
```

Replace the `Connection` struct and its doc comment (`:96-103`) with:

```go
// Connection wraps a gRPC client conn + an open WorkerService bidi
// stream. Callers drive the worker protocol through Send / Recv; Close
// shuts both down.
//
// EVERY WRITE GOES THROUGH sendMu. grpc-go's contract (stream.go,
// ClientStream): "it is not safe to call SendMsg on the same stream in
// different goroutines. It is also not safe to call CloseSend
// concurrently with SendMsg." A worker host writes from many goroutines
// at once -- the cockpit's heartbeat ticker, its recv loop answering a
// Ping, one goroutine per tool dispatch, a model call's content and
// keepalive deltas, a pull's progress, an app session's chunks -- and
// this lock is the one place they are serialized. It is the client half
// of component/worker.streamSession.send's sendMu on the agent. Recv is
// deliberately outside it: one goroutine in SendMsg and another in
// RecvMsg is the one combination grpc-go permits.
type Connection struct {
	conn   *grpc.ClientConn
	client memqlv1.WorkerServiceClient
	stream memqlv1.WorkerService_StreamClient

	sendMu sync.Mutex
	// sendErr is the first error the stream answered a Send with, and
	// every later Send answers it without touching the stream: on error
	// SendMsg has aborted the stream, and a second writer learning that
	// from the same error is simpler than each writer learning it from a
	// different one. Mirrors streamSession.sendErr on the agent.
	sendErr error
	// closed is set by Close under sendMu, so a Send that lost the race
	// to Close answers errClosed rather than reaching a half-closed
	// stream.
	closed bool
}

// errClosed is what Send and Recv answer on a nil or closed Connection.
var errClosed = errors.New("sdk/worker: connection is closed")
```

Replace `Send` and `Recv` (`:190-206`) with:

```go
// Send writes one message on the worker side of the stream, serialized
// against every other Send and against Close. Safe from any goroutine.
func (c *Connection) Send(msg *memqlv1.WorkerClientMessage) error {
	if c == nil || c.stream == nil {
		return errClosed
	}
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	if c.closed {
		return errClosed
	}
	if c.sendErr != nil {
		return c.sendErr
	}
	if err := c.stream.Send(msg); err != nil {
		c.sendErr = err
		return err
	}
	return nil
}

// Recv blocks until the next inbound message lands. Not under sendMu:
// grpc-go permits one goroutine in SendMsg beside one in RecvMsg, and a
// Recv that waited on a writer would stall the cockpit's whole inbound
// loop behind a slow tool result.
func (c *Connection) Recv() (*memqlv1.WorkerServerMessage, error) {
	if c == nil || c.stream == nil {
		return nil, errClosed
	}
	return c.stream.Recv()
}
```

Replace `Close` (`:208-220`) with:

```go
// Close half-closes the stream (CloseSend) and closes the underlying
// gRPC connection. Safe to call on a nil receiver, idempotent, and safe
// to call while other goroutines are in Send: CloseSend is taken under
// sendMu, so it waits for an in-flight Send to return rather than
// running beside it. That wait is bounded by the transport -- a Send
// blocked on flow control returns once the keepalive
// (DefaultKeepaliveTime + DefaultKeepaliveTimeout) tears the transport
// down -- and in the ordinary case is microseconds. The ClientConn is
// closed after the half-close, outside the lock, so the server sees a
// clean end of the send direction before the transport goes.
func (c *Connection) Close() {
	if c == nil {
		return
	}
	c.sendMu.Lock()
	if !c.closed {
		c.closed = true
		if c.stream != nil {
			_ = c.stream.CloseSend()
		}
	}
	c.sendMu.Unlock()
	if c.conn != nil {
		_ = c.conn.Close()
	}
}
```

Leave `Dial` (`:115-178`) and `Stream()` (`:183-188`) untouched in this task; Task 2 removes `Stream()`.

- [ ] **Step 4: Run the tests to verify they pass**

Run (from `/home/znas/projects/memql/memql`):

```bash
gofmt -l sdk/go/worker/ && go vet ./sdk/go/worker/ && go test -race -count=1 ./sdk/go/worker/...
```

Expected: `gofmt -l` prints nothing; `go vet` prints nothing; `ok  	github.com/znasllc-io/memql/sdk/go/worker` with no `DATA RACE` output. Then run the root-package SDK surface gate, which walks `sdk/go`:

```bash
go test -count=1 -run TestSDKPublicSurfaceHasNoProtoLeak .
```

Expected: `ok  	github.com/znasllc-io/memql`.

- [ ] **Step 5: Commit**

Run (from `/home/znas/projects/memql/memql`):

```bash
git checkout -b fix/sdk-worker-serialize-send
git add sdk/go/worker/worker.go sdk/go/worker/send_serialization_test.go
git commit -m "fix(sdk): serialize every write on the worker stream under one lock

grpc-go forbids SendMsg from two goroutines on one stream, and CloseSend
beside a SendMsg. The cockpit writes its WorkerService stream from the
heartbeat ticker, the recv loop (Pong), every tool dispatch, every model
call's deltas and keepalives, every pull and every app session at once,
all through worker.Connection.Send, which was a bare pass-through. This
is the most plausible root of the unexplained stream-end codes the last
three cockpit reconnect fixes were chasing (cockpit audit C-1).

Connection.Send now holds sendMu around stream.Send with the agent's
sticky sendErr (component/worker.streamSession.send); Close takes
CloseSend under the same lock and marks the connection closed so a Send
that lost the race answers a named error. Recv stays outside the lock,
which is the one combination grpc-go permits.

The test stands an overlap-detecting fake where grpc's stream would be
and runs heartbeat, pong and delta writers at once under -race; a
Close-during-Send case and the sticky-error case pin the rest.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01RTCw4Xs3am16N2ujcR56aP"
```

---

### Task 2: SDK `Connection` hands out no raw stream

**Files:**
- Modify: `/home/znas/projects/memql/memql/sdk/go/worker/worker.go:1-30` (package doc), `:105-114` (`Dial` doc), `:180-188` (delete `Stream()`)
- Test: `/home/znas/projects/memql/memql/sdk/go/worker/no_raw_stream_test.go` (create)

**Interfaces:**
- Consumes: `*Connection` from Task 1 (`Send`, `Recv`, `Close`).
- Produces: the exported method set of `*worker.Connection` is exactly `Send`, `Recv`, `Close`. No exported symbol returns or exposes anything implementing `grpc.ClientStream`. Task 3's cockpit `stream` interface (`Send`, `Recv`, `Close`) is satisfied by it.

- [ ] **Step 1: Write the failing test**

Create `/home/znas/projects/memql/memql/sdk/go/worker/no_raw_stream_test.go`:

```go
package worker

import (
	"reflect"
	"testing"

	"google.golang.org/grpc"
)

// TestConnectionHandsOutNoRawStream pins the property the send lock
// depends on: the ONLY way to write the worker stream is Connection.Send,
// so there is no path around sendMu. It fails on any exported method
// that returns something implementing grpc.ClientStream -- which is what
// the retired Stream() accessor did -- and on any exported field of that
// shape.
func TestConnectionHandsOutNoRawStream(t *testing.T) {
	clientStream := reflect.TypeOf((*grpc.ClientStream)(nil)).Elem()
	typ := reflect.TypeOf((*Connection)(nil))

	for i := 0; i < typ.NumMethod(); i++ {
		m := typ.Method(i)
		for j := 0; j < m.Type.NumOut(); j++ {
			if out := m.Type.Out(j); out.Implements(clientStream) {
				t.Errorf("Connection.%s returns %s, which implements grpc.ClientStream: a caller holding it can SendMsg or CloseSend around sendMu. Remove the accessor; writes go through Connection.Send.", m.Name, out)
			}
		}
	}
	st := typ.Elem()
	for i := 0; i < st.NumField(); i++ {
		f := st.Field(i)
		if f.IsExported() && f.Type.Implements(clientStream) {
			t.Errorf("Connection.%s is an exported %s: unexport it; writes go through Connection.Send.", f.Name, f.Type)
		}
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run (from `/home/znas/projects/memql/memql`):

```bash
go test -count=1 -run TestConnectionHandsOutNoRawStream ./sdk/go/worker/
```

Expected: FAIL with `Connection.Stream returns grpc.BidiStreamingClient[github.com/znasllc-io/memql/component/grpc/gen.WorkerClientMessage,github.com/znasllc-io/memql/component/grpc/gen.WorkerServerMessage], which implements grpc.ClientStream ...`.

- [ ] **Step 3: Delete the accessor and rewrite the docs that name it**

In `/home/znas/projects/memql/memql/sdk/go/worker/worker.go`, replace the package doc comment (`:1-30`) with:

```go
// Package worker is the SDK's worker subpackage. It wraps the
// transport layer for dialing a memql cluster's WorkerService:
// gRPC connection setup, TLS configuration, auth-token plumbing,
// and the bidi stream opener.
//
// Scope. The SDK owns transport and the stream. The protocol (Register /
// RegisterAck / Heartbeat / ToolResult / ToolCall / ...) is expressed in
// the wire messages a caller passes to Connection.Send and reads from
// Connection.Recv; the stream itself is not handed out, because every
// write on it must go through the one lock Send holds (see Connection,
// and TestConnectionHandsOutNoRawStream). Wrapping the full envelope set
// in SDK-owned types is a separate effort -- the goal of this package is
// to stop the cockpit (and any future worker host) from re-implementing
// the dial code per consumer.
//
// Typical use:
//
//	conn, err := worker.Dial(ctx, worker.DialConfig{
//	    Endpoint: "host:443",
//	    UseTLS:   true,
//	    Token:    cfg.Token, // mql_wkr_...
//	    Logger:   logger,
//	})
//	if err != nil { ... }
//	defer conn.Close()
//
//	// conn.Send(register) / conn.Recv() -- the worker protocol, from as
//	// many goroutines as the host needs; Send is serialized inside.
//
// Surfaced by memql#117 (the issue requesting this SDK module) and
// the SDK-only rule in memql/sdk/go/CLAUDE.md.
package worker
```

Replace the `Dial` doc comment (`:105-114`) with:

```go
// Dial opens the gRPC connection, attaches the auth token to the
// stream metadata, and starts the WorkerService bidi stream. The
// caller is responsible for sending the initial Register message
// through the returned connection -- the SDK does not assume the worker
// protocol's lifecycle. See memql-cockpit's internal/worker/
// connect.go for the reference register / heartbeat / tool-result
// loop.
//
// On any error the partially-opened resources are cleaned up before
// the error is returned.
```

Delete the `Stream` method and its comment entirely (`:180-188`):

```go
// Stream returns the underlying bidi stream. Caller drives the
// worker protocol against it (Register, Heartbeat, ToolResult,
// etc.).
func (c *Connection) Stream() memqlv1.WorkerService_StreamClient {
	if c == nil {
		return nil
	}
	return c.stream
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run (from `/home/znas/projects/memql/memql`):

```bash
gofmt -l sdk/go/worker/ && go vet ./sdk/go/worker/ && go test -race -count=1 ./sdk/go/worker/... && go test -count=1 -run TestSDKPublicSurfaceHasNoProtoLeak . && go build ./... && grep -rn "\.Stream()" --include=*.go sdk/ component/identity component/grpc core/grpctls worker_dial_tls_contract_test.go
```

Expected: `ok` for both test invocations, `go build` silent, and the `grep` prints nothing (exit 1 from grep is the wanted outcome; the `&&` chain ends there). Then confirm the two sibling consumers still build against the changed SDK:

```bash
(cd /home/znas/projects/memql/memql-cockpit && go build ./... && go vet ./internal/worker/)
```

Expected: silent.

- [ ] **Step 5: Commit, open the memql PR, merge it, record the merge commit**

Run (from `/home/znas/projects/memql/memql`):

```bash
git add sdk/go/worker/worker.go sdk/go/worker/no_raw_stream_test.go
git commit -m "fix(sdk): the worker connection hands out no raw stream

Stream() was the one way to SendMsg or CloseSend around the lock
Connection.Send now holds, and it had no caller in memql,
memql-cockpit or memql-bff-copresent. Pre-release: deleted, not
deprecated. A reflection test fails on any exported method or field of
*Connection that implements grpc.ClientStream, so the escape hatch
cannot come back under another name.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01RTCw4Xs3am16N2ujcR56aP"
git push -u origin fix/sdk-worker-serialize-send
gh pr create --repo znasllc-io/memql --base main \
  --title "fix(sdk): serialize every write on the worker stream" \
  --body "$(cat <<'EOF'
## Problem

grpc-go forbids SendMsg on one stream from two goroutines, and CloseSend beside a SendMsg. The cockpit drives its WorkerService stream from the heartbeat ticker, the recv loop (Pong), one goroutine per tool dispatch, every model call's content and keepalive deltas, every pull's progress and every app session's chunks, all through sdk/go/worker.Connection.Send, which was a bare pass-through with no lock. Cockpit audit finding C-1; the most plausible root of the unexplained stream-end codes the last three cockpit reconnect fixes (memql-cockpit #418, #420, #422) were chasing.

## Change

- Connection.Send holds sendMu around stream.Send with the agent's sticky sendErr (component/worker.streamSession.send, the same lock on the other end of the same stream). Close takes CloseSend under the lock and marks the connection closed. Recv stays outside the lock.
- Stream() is deleted: it was the only way around the lock and had no caller. A reflection test fails on any exported symbol that hands out a grpc.ClientStream.
- Tests: heartbeat + pong + delta writers at once on an overlap-detecting fake stream under -race; Close during Send; sticky first error.

Wire unchanged. The cockpit PR that follows moves .github/memql-pin to this merge and routes every cockpit helper through the one seam.

🤖 Generated with [Claude Code](https://claude.com/claude-code)

https://claude.ai/code/session_01RTCw4Xs3am16N2ujcR56aP
EOF
)"
```

When CI is green, enqueue with the bare form (memql `CLAUDE.md`: no `--merge`, no `--delete-branch`; a `DIRTY` status means rebase on `origin/main` and force-push; `BEHIND` means `gh pr update-branch <n>`):

```bash
gh pr merge <n> --repo znasllc-io/memql
```

After the queue merges it (minutes; `is already queued to merge` is confirmation), record the merge commit and bring the local sibling to it -- Task 4 pins the cockpit to this sha and the cockpit's local build resolves against this checkout:

```bash
gh pr view <n> --repo znasllc-io/memql --json mergeCommit -q .mergeCommit.oid
git checkout main && git pull --ff-only origin main && git rev-parse HEAD
```

Expected: the two shas printed are identical. Write it down; it is `<memql-merge-sha>` in Task 4.

---

### Task 3: Cockpit `Connection` writes through one seam, and a guard that no writer bypasses it

**Files:**
- Modify: `/home/znas/projects/memql/memql-cockpit/internal/worker/connect.go:146-154` (`register`), `:183-189` (`Send`), `:245-251` (`SendHeartbeat`), `:313-324` (`SendAppSessionChunk`), `:327-331` (`SendAppSessionEnd`), `:339-350` (`SendModelCallDelta`), `:353-357` (`SendModelCallEnd`), `:360-370` (`SendToolResult`); add a compile-time assertion after the `stream` interface (`:55-59`)
- Test: `/home/znas/projects/memql/memql-cockpit/internal/worker/connect_send_test.go` (create)

This task builds and passes against the CURRENT pin (`275623b3d`) as well as against the merged SDK: nothing here calls a new SDK symbol. It may be done before Task 2 merges; only Task 4 waits on the memql merge.

**Interfaces:**
- Consumes: the cockpit `stream` interface (`connect.go:55-59`: `Send(*memqlv1.WorkerClientMessage) error`, `Recv() (*memqlv1.WorkerServerMessage, error)`, `Close()`), satisfied by `*sdkworker.Connection` after Task 1 (and today).
- Produces: `func (c *Connection) Send(msg *memqlv1.WorkerClientMessage) error` is the single call site of the SDK connection's `Send` in the repository; every `Send*` helper on the cockpit `Connection` (`SendPong`, `SendModelPullProgress`, `SendModelPullEnd`, `SendHeartbeat`, `SendAppSessionChunk`, `SendAppSessionEnd`, `SendModelCallDelta`, `SendModelCallEnd`, `SendToolResult`) routes through it. Signatures unchanged, so `modelcall.Sender`, `appsession.Sender` and `pullSender` are untouched.

- [ ] **Step 1: Write the failing test (the scan) and the pinning test (concurrent forwarding)**

Create `/home/znas/projects/memql/memql-cockpit/internal/worker/connect_send_test.go`:

```go
package worker

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"google.golang.org/protobuf/types/known/timestamppb"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

// TestEveryWorkerWriteGoesThroughConnectionSend is the cockpit half of
// audit finding C-1. grpc-go forbids concurrent SendMsg on one stream;
// the lock that serializes it lives in the SDK's Connection.Send (memql
// sdk/go/worker); and a cockpit write reaches that lock only by calling
// the SDK connection's Send. So exactly ONE place in this repository may
// call it -- Connection.Send in connect.go -- and every Send* helper
// routes through that. The other way around the lock, the SDK's
// raw-stream accessor, is deleted on the engine side; this guard keeps a
// call to it from coming back here while the pin still carries it.
//
// The needles are assembled from fragments so this file cannot match
// itself, and nothing is excluded but build and vendor directories --
// the same sweep as internal/access's
// TestNoCockpitCodePathNamesTheRetiredRoleEnum, for the same reason: an
// exclusion list is how a guard quietly stops covering the file somebody
// actually edits.
func TestEveryWorkerWriteGoesThroughConnectionSend(t *testing.T) {
	sdkSend := ".conn" + ".Send("
	rawStream := ".Stream" + "()"

	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	var sdkSendSites []string
	scanned := 0
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "bin", "dist", "vendor", "node_modules":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		body, readErr := os.ReadFile(path)
		if readErr != nil {
			t.Logf("skipping %s: %v", path, readErr)
			return nil
		}
		scanned++
		rel, _ := filepath.Rel(root, path)
		for n, line := range strings.Split(string(body), "\n") {
			if strings.Contains(line, rawStream) {
				t.Errorf("%s:%d holds the SDK's raw stream (%s). Write through Connection.Send; the lock is there.", rel, n+1, strings.TrimSpace(line))
			}
			if strings.Contains(line, sdkSend) {
				sdkSendSites = append(sdkSendSites, rel+":"+strconv.Itoa(n+1))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if scanned == 0 {
		t.Fatal("scanned no Go files; the walk root is wrong")
	}
	want := filepath.Join("internal", "worker", "connect.go")
	if len(sdkSendSites) != 1 || !strings.HasPrefix(sdkSendSites[0], want+":") {
		t.Fatalf("the SDK connection's Send is called at %d site(s) %v; want exactly one, in %s (Connection.Send). Route every helper through c.Send.", len(sdkSendSites), sdkSendSites, want)
	}
}

// seamStream stands where the SDK's connection would be. It locks, as
// the SDK does, and records: the test below proves that every kind of
// write the runner makes -- from the goroutines that make them, all at
// once -- lands on the seam as one frame of the expected kind, and that
// this Connection keeps no unsynchronized state on the way (run under
// -race). Serialization itself is proven where the lock is, in the SDK
// (memql sdk/go/worker/send_serialization_test.go).
type seamStream struct {
	mu     sync.Mutex
	frames []*memqlv1.WorkerClientMessage
}

func (s *seamStream) Send(m *memqlv1.WorkerClientMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.frames = append(s.frames, m)
	return nil
}

func (s *seamStream) Recv() (*memqlv1.WorkerServerMessage, error) {
	return nil, errors.New("seamStream: nothing to receive")
}

func (s *seamStream) Close() {}

func (s *seamStream) count(is func(*memqlv1.WorkerClientMessage) bool) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, f := range s.frames {
		if is(f) {
			n++
		}
	}
	return n
}

func TestEveryWriterKindReachesTheSeamConcurrently(t *testing.T) {
	seam := &seamStream{}
	conn := &Connection{conn: seam}

	type writer struct {
		name  string
		write func(i int) error
		is    func(m *memqlv1.WorkerClientMessage) bool
	}
	// One entry per Send* helper on Connection, in the order they appear
	// in connect.go. A helper added there without a row here still
	// reaches the seam (the scan test above guarantees the route); this
	// list is what proves the frame that arrives is the frame that was
	// asked for.
	writers := []writer{
		{"heartbeat", func(i int) error { return conn.SendHeartbeat(uint32(i), nil, nil, nil) },
			func(m *memqlv1.WorkerClientMessage) bool { return m.GetHeartbeat() != nil }},
		{"pong", func(i int) error { return conn.SendPong("ping-"+strconv.Itoa(i), timestamppb.Now()) },
			func(m *memqlv1.WorkerClientMessage) bool { return m.GetPong() != nil }},
		{"model pull progress", func(i int) error {
			return conn.SendModelPullProgress(&memqlv1.ModelPullProgress{RequestId: "pull-1", Status: "pulling", CompletedBytes: uint64(i)})
		}, func(m *memqlv1.WorkerClientMessage) bool { return m.GetModelPullProgress() != nil }},
		{"model pull end", func(i int) error {
			return conn.SendModelPullEnd(&memqlv1.ModelPullEnd{RequestId: "pull-" + strconv.Itoa(i), Ok: true})
		}, func(m *memqlv1.WorkerClientMessage) bool { return m.GetModelPullEnd() != nil }},
		{"app session chunk", func(i int) error { return conn.SendAppSessionChunk("sess-1", "stdout", []byte("x"), uint64(i)) },
			func(m *memqlv1.WorkerClientMessage) bool { return m.GetAppSessionChunk() != nil }},
		{"app session end", func(i int) error {
			return conn.SendAppSessionEnd(&memqlv1.AppSessionEnd{SessionId: "sess-" + strconv.Itoa(i)})
		}, func(m *memqlv1.WorkerClientMessage) bool { return m.GetAppSessionEnd() != nil }},
		{"model call delta", func(i int) error { return conn.SendModelCallDelta("call-1", uint64(i), "tok", i%7 == 0) },
			func(m *memqlv1.WorkerClientMessage) bool { return m.GetModelCallDelta() != nil }},
		{"model call end", func(i int) error {
			return conn.SendModelCallEnd(&memqlv1.ModelCallEnd{RequestId: "call-" + strconv.Itoa(i), FinishReason: "stop"})
		}, func(m *memqlv1.WorkerClientMessage) bool { return m.GetModelCallEnd() != nil }},
		{"tool result", func(i int) error {
			return conn.SendToolResult("call-"+strconv.Itoa(i), &memqlv1.Success{ExitCode: 0}, nil)
		}, func(m *memqlv1.WorkerClientMessage) bool { return m.GetToolResult() != nil }},
	}

	const perWriter = 200
	var wg sync.WaitGroup
	for _, w := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				if err := w.write(i); err != nil {
					t.Errorf("%s: %v", w.name, err)
					return
				}
			}
		}()
	}
	wg.Wait()

	for _, w := range writers {
		if got := seam.count(w.is); got != perWriter {
			t.Errorf("%s: %d frames reached the seam, want %d", w.name, got, perWriter)
		}
	}
	if got, want := seam.count(func(*memqlv1.WorkerClientMessage) bool { return true }), perWriter*len(writers); got != want {
		t.Errorf("seam saw %d frames in total, want %d: a helper wrote something it was not asked to", got, want)
	}
}
```

- [ ] **Step 2: Run the tests to verify the scan fails and the forwarding test pins**

Run (from `/home/znas/projects/memql/memql-cockpit`):

```bash
go test -race -count=1 -run 'TestEveryWorkerWriteGoesThroughConnectionSend|TestEveryWriterKindReachesTheSeamConcurrently' ./internal/worker/
```

Expected: `TestEveryWorkerWriteGoesThroughConnectionSend` FAILS with `the SDK connection's Send is called at 8 site(s) [internal/worker/connect.go:150 internal/worker/connect.go:188 internal/worker/connect.go:246 internal/worker/connect.go:314 internal/worker/connect.go:328 internal/worker/connect.go:340 internal/worker/connect.go:354 internal/worker/connect.go:367]; want exactly one ...`. `TestEveryWriterKindReachesTheSeamConcurrently` PASSES already: it pins forwarding, which is true today, and its `-race` value is in Step 4 and in CI afterwards. No `DATA RACE` output.

- [ ] **Step 3: Route every helper through `Send`, and record at compile time that the SDK fills the seam**

In `/home/znas/projects/memql/memql-cockpit/internal/worker/connect.go`, immediately after the `stream` interface (after `:59`), add:

```go
// The SDK's connection is what fills the seam in production, and this is
// the compile-time record of it: the lock that serializes every write on
// the gRPC stream lives in (*sdkworker.Connection).Send, and the seam's
// Send is that method.
var _ stream = (*sdkworker.Connection)(nil)
```

In `register` (`:150`), change `c.conn.Send(` to `c.Send(`:

```go
	if err := c.Send(&memqlv1.WorkerClientMessage{
		Payload: &memqlv1.WorkerClientMessage_Register{Register: register},
	}); err != nil {
		return fmt.Errorf("worker.register: send: %w", err)
	}
```

Replace `Send` and its comment (`:183-189`) with:

```go
// Send writes a single message on the worker side of the stream.
//
// THIS IS THE ONE SEAM. Every Send* helper below routes through here,
// and this is the only call to the SDK connection's Send in the
// repository (TestEveryWorkerWriteGoesThroughConnectionSend). The SDK's
// Send holds the lock that serializes every write on the gRPC stream --
// grpc-go forbids SendMsg from two goroutines on one stream, and this
// worker writes from the heartbeat goroutine, the recv goroutine (Pong),
// every tool dispatch, every model call's deltas and keepalives, every
// pull's progress and every app session's chunks at once. The lock lives
// in the SDK rather than here because the SDK owns the stream: a second
// lock at this layer would protect nothing the SDK's does not, and would
// have to be re-invented by every other worker host.
func (c *Connection) Send(msg *memqlv1.WorkerClientMessage) error {
	if c == nil || c.conn == nil {
		return errors.New("worker: connection is closed")
	}
	return c.conn.Send(msg)
}
```

In each of the six helpers that still call `c.conn.Send(` directly, change it to `c.Send(`; the bodies are otherwise unchanged. After the edit they read:

```go
func (c *Connection) SendHeartbeat(active uint32, perCap map[string]uint32, inventory []apps.Info, hw *hardware.Inventory) error {
	return c.Send(&memqlv1.WorkerClientMessage{
		Payload: &memqlv1.WorkerClientMessage_Heartbeat{
			Heartbeat: buildHeartbeat(active, perCap, inventory, hw),
		},
	})
}
```

```go
func (c *Connection) SendAppSessionChunk(sessionID, stream string, data []byte, seq uint64) error {
	return c.Send(&memqlv1.WorkerClientMessage{
		Payload: &memqlv1.WorkerClientMessage_AppSessionChunk{
			AppSessionChunk: &memqlv1.AppSessionChunk{
				SessionId: sessionID,
				Stream:    stream,
				Data:      data,
				Seq:       seq,
			},
		},
	})
}
```

```go
func (c *Connection) SendAppSessionEnd(end *memqlv1.AppSessionEnd) error {
	return c.Send(&memqlv1.WorkerClientMessage{
		Payload: &memqlv1.WorkerClientMessage_AppSessionEnd{AppSessionEnd: end},
	})
}
```

```go
func (c *Connection) SendModelCallDelta(requestID string, seq uint64, content string, keepalive bool) error {
	return c.Send(&memqlv1.WorkerClientMessage{
		Payload: &memqlv1.WorkerClientMessage_ModelCallDelta{
			ModelCallDelta: &memqlv1.ModelCallDelta{
				RequestId: requestID,
				Seq:       seq,
				Content:   content,
				Keepalive: keepalive,
			},
		},
	})
}
```

```go
func (c *Connection) SendModelCallEnd(end *memqlv1.ModelCallEnd) error {
	return c.Send(&memqlv1.WorkerClientMessage{
		Payload: &memqlv1.WorkerClientMessage_ModelCallEnd{ModelCallEnd: end},
	})
}
```

```go
func (c *Connection) SendToolResult(callId string, success *memqlv1.Success, failure *memqlv1.Failure) error {
	res := &memqlv1.ToolResult{CallId: callId}
	if success != nil {
		res.Payload = &memqlv1.ToolResult_Success{Success: success}
	} else if failure != nil {
		res.Payload = &memqlv1.ToolResult_Failure{Failure: failure}
	}
	return c.Send(&memqlv1.WorkerClientMessage{
		Payload: &memqlv1.WorkerClientMessage_ToolResult{ToolResult: res},
	})
}
```

`SendPong`, `SendModelPullProgress` and `SendModelPullEnd` already call `c.Send` and are untouched. The doc comments on every helper stay as they are.

- [ ] **Step 4: Run the tests to verify they pass**

Run (from `/home/znas/projects/memql/memql-cockpit`):

```bash
gofmt -l internal/worker/ && go vet ./... && go test -race -count=1 ./internal/worker/... && grep -rn "\.conn\.Send(\|\.Stream()" --include=*.go . 
```

Expected: `gofmt -l` prints nothing; `go vet` prints nothing; `ok` for `internal/worker`, `internal/worker/appsession`, `internal/worker/modelcall` and the rest of the subtree with no `DATA RACE`; the `grep` prints exactly one line, `./internal/worker/connect.go:<line>:	return c.conn.Send(msg)` (the body of `Connection.Send`).

- [ ] **Step 5: Commit**

Run (from `/home/znas/projects/memql/memql-cockpit`):

```bash
git checkout -b fix/worker-serialize-stream-send
git add internal/worker/connect.go internal/worker/connect_send_test.go
git commit -m "fix(worker): every write reaches the SDK's send lock through one seam

grpc-go forbids SendMsg from two goroutines on one stream. This worker
writes its stream from the heartbeat goroutine, the recv goroutine
(Pong), every tool dispatch, every model call's deltas and keepalives,
every pull's progress and every app session's chunks at once, and until
memql's sdk/go/worker.Connection.Send grew its lock nothing serialized
them (audit C-1; the likely root of the stream-end codes #418, #420 and
#422 were chasing).

The lock lives in the SDK, which owns the stream. This side makes the
route to it single: Connection.Send is now the only call to the SDK
connection's Send in the repository, every Send* helper goes through
it, and a compile-time assertion records that *sdkworker.Connection is
what fills the seam. TestEveryWorkerWriteGoesThroughConnectionSend
walks every Go file and fails on a second call site or on a hold of the
SDK's raw stream; TestEveryWriterKindReachesTheSeamConcurrently drives
all nine writer kinds at once under -race.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01RTCw4Xs3am16N2ujcR56aP"
```

---

### Task 4: Cockpit pin moves past the SDK lock; the suite runs under `-race` in CI

Requires `<memql-merge-sha>` from Task 2 Step 5.

**Files:**
- Modify: `/home/znas/projects/memql/memql-cockpit/.github/memql-pin` (append one paragraph before the sha line; replace the last line)
- Modify: `/home/znas/projects/memql/memql-cockpit/.github/workflows/ci.yml:67-69` (the `go test` step)
- Modify (only if it changes): `/home/znas/projects/memql/memql-cockpit/go.mod`, `go.sum` via `go mod tidy`
- Delete (if this plan was committed): `/home/znas/projects/memql/memql-cockpit/docs/superpowers/plans/2026-09-13-serialize-worker-stream-send.md`

**Interfaces:**
- Consumes: memql `main` at `<memql-merge-sha>`, which carries `sdk/go/worker.Connection` with `sendMu` and without `Stream()`.
- Produces: cockpit CI resolves `../memql` at that sha, so the lock the cockpit relies on is the one CI tests against; the `test` job runs `go test -race`.

- [ ] **Step 1: Verify the local sibling is at the merge and the cockpit builds against it**

Run:

```bash
git -C /home/znas/projects/memql/memql rev-parse HEAD
cd /home/znas/projects/memql/memql-cockpit && go build ./... && go vet ./... && go test -race -count=1 -timeout=600s ./...
```

Expected: the first line prints `<memql-merge-sha>`; build and vet silent; every package `ok` (50 s wall at `efa804e`, measured 2026-09-13), no `DATA RACE`. If the sibling is not at the merge: `git -C /home/znas/projects/memql/memql checkout main && git -C /home/znas/projects/memql/memql pull --ff-only origin main`.

- [ ] **Step 2: Move the pin**

Edit `/home/znas/projects/memql/memql-cockpit/.github/memql-pin`. The file is a block of `#` comment lines followed by a single bare sha on the last line (`275623b3d90d57368d0efec62261705db400a33d`). Insert this paragraph after the last existing comment line and before the sha line, then replace the sha line with `<memql-merge-sha>`:

```
# BUMPED 2026-09-13 to memql main at the merge of the worker-stream send
# lock (memql PR fix/sdk-worker-serialize-send; cockpit audit C-1).
# sdk/go/worker.Connection.Send now serializes every write on the
# WorkerService stream under one mutex and Close takes CloseSend under the
# same lock; Stream(), the one way around it, is gone. This repository's
# Connection routes every Send* helper through the SDK's Send and never
# held the raw stream, so the bump is the fix landing, not a code change
# here. No module landed between the previous pin and this one: go.mod
# and go.sum are unchanged by `go mod tidy`.
```

Then:

```bash
cd /home/znas/projects/memql/memql-cockpit && tail -1 .github/memql-pin && go mod tidy && git status --short go.mod go.sum
```

Expected: `tail` prints `<memql-merge-sha>` and nothing else on that line; `git status --short go.mod go.sum` prints nothing. If it prints a change, a memql module landed between the pins: keep the change (that is what "add the module's require and replace in the same commit" means), and add the module's name to the pin paragraph.

- [ ] **Step 3: Run the cockpit test job under `-race`**

Edit `/home/znas/projects/memql/memql-cockpit/.github/workflows/ci.yml` lines 67-69, which read:

```yaml
      - name: go test
        working-directory: memql-cockpit
        run: go test -count=1 -timeout=300s ./...
```

to:

```yaml
      - name: go test
        working-directory: memql-cockpit
        # -race: the worker holds one gRPC stream that many goroutines
        # write (heartbeat, pong, tool results, model deltas, pulls, app
        # sessions). The lock is in the SDK; the forwarding test in
        # internal/worker/connect_send_test.go has nothing to catch
        # without the detector. The suite was race-clean and 50s wall
        # when this went in; the timeout is doubled for the detector's
        # overhead on the runner.
        run: go test -race -count=1 -timeout=600s ./...
```

Verify the YAML still parses and the flag is what runs:

```bash
grep -n "go test -race -count=1 -timeout=600s ./..." .github/workflows/ci.yml
```

Expected: one line, `ci.yml:76` (the `run:` line, after the comment).

- [ ] **Step 4: Run the whole cockpit suite the way CI now will**

Run (from `/home/znas/projects/memql/memql-cockpit`):

```bash
go test -race -count=1 -timeout=600s ./... && bash scripts/install/lib_test.sh
```

Expected: every package `ok`, no `DATA RACE`; the installer library tests pass as they do on `main`.

- [ ] **Step 5: Commit, open the cockpit PR, merge**

If this plan file was committed to the cockpit earlier, include its removal (cockpit `CLAUDE.md`: a finished plan is deleted by the PR that finishes it):

```bash
cd /home/znas/projects/memql/memql-cockpit
git rm --quiet docs/superpowers/plans/2026-09-13-serialize-worker-stream-send.md 2>/dev/null || true
git add .github/memql-pin .github/workflows/ci.yml go.mod go.sum
git commit -m "chore(worker): bump the memql pin past the SDK send lock; run the suite under -race

The pin moves to the memql merge that gives sdk/go/worker.Connection.Send
its mutex and deletes Stream(). Nothing here changes for it: every
cockpit helper already routes through the one seam (previous commit).
No module landed between the pins, so go.mod and go.sum are unchanged.

CI's go test gains -race. The forwarding test in internal/worker has
nothing to catch without the detector, and the suite is race-clean.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01RTCw4Xs3am16N2ujcR56aP"
git push -u origin fix/worker-serialize-stream-send
gh pr create --repo znasllc-io/memql-cockpit --base main \
  --title "fix(worker): serialize every write on the cluster stream" \
  --body "$(cat <<'EOF'
## Problem

grpc-go forbids SendMsg on one stream from two goroutines, and CloseSend beside a SendMsg. The worker writes its WorkerService stream from the heartbeat ticker, the recv loop (Pong), one goroutine per tool dispatch, every model call's content and keepalive deltas, every pull's progress and every app session's chunks -- and nothing serialized them. Audit finding C-1; the most plausible root of the unexplained stream-end codes #418, #420 and #422 were chasing.

## Change

- The lock lives in the SDK (memql PR fix/sdk-worker-serialize-send, merged as <memql-merge-sha>): `sdk/go/worker.Connection.Send` holds a mutex, `Close` takes `CloseSend` under it, and `Stream()` is gone.
- Here, `Connection.Send` becomes the only call to the SDK's `Send` in the repository; every `Send*` helper routes through it; a compile-time assertion records that `*sdkworker.Connection` fills the seam.
- `TestEveryWorkerWriteGoesThroughConnectionSend` walks every Go file and fails on a second call site or a hold of the raw stream. `TestEveryWriterKindReachesTheSeamConcurrently` drives all nine writer kinds at once under `-race`.
- `.github/memql-pin` moves to that merge; `go mod tidy` changes nothing. CI's `go test` gains `-race` (suite race-clean, 50s wall).

Wire unchanged.

🤖 Generated with [Claude Code](https://claude.com/claude-code)

https://claude.ai/code/session_01RTCw4Xs3am16N2ujcR56aP
EOF
)"
```

Replace `<memql-merge-sha>` in the body with the recorded sha before running. When the cockpit CI (`build`, `test`, `fuzz`, `vet`, `computeruse` vet, `memql-pin-guard`) is green:

```bash
gh pr merge <n> --repo znasllc-io/memql-cockpit --merge
```

(The cockpit's history is ordinary merge commits, `Merge pull request #N from znasllc-io/<branch>`; it has no merge queue.)

---

## Verification matrix

| Where | Command | Proves |
|---|---|---|
| memql root | `gofmt -l sdk/go/worker/` | formatted |
| memql root | `go vet ./sdk/go/worker/` | vet-clean (including `copylocks` on the new mutex field) |
| memql root | `go test -race -count=1 ./sdk/go/worker/...` | heartbeat + pong + delta writers never overlap; Close never overlaps Send; first error is sticky; no exported raw-stream escape |
| memql root | `go test -count=1 -run TestSDKPublicSurfaceHasNoProtoLeak .` | `worker.Connection` allowlist entry still matches (`Send`, `Recv`) and nothing new leaks |
| memql root | `make test` (before opening the PR; CI runs it) | whole tree |
| cockpit | `gofmt -l internal/worker/ && go vet ./...` | formatted, vet-clean |
| cockpit | `go test -race -count=1 ./internal/worker/...` | one seam; nine writer kinds concurrently; existing recv/pong/pull tests still pass |
| cockpit | `go test -race -count=1 -timeout=600s ./...` | whole suite race-clean, what CI runs after Task 4 |
| cockpit | `grep -rn "\.conn\.Send(\|\.Stream()" --include=*.go .` | exactly one line, in `internal/worker/connect.go` (`Connection.Send`) |
| both | `grep -rn "\.Stream()" --include=*.go /home/znas/projects/memql/memql/sdk /home/znas/projects/memql/memql-bff-copresent` | nothing |

## Self-review

**Spec coverage** (the request's five items against the tasks): (1) failing `-race` test on a fake stream with heartbeat + pong + delta writers -- Task 1 Step 1 (`overlapDetectingStream`, `TestSendIsSerializedAcrossWriters`), run with `go test -race` in Step 2. (2) Minimal SDK fix: mutex around `stream.Send`, `CloseSend` under the lock -- Task 1 Step 3; cockpit relies on the SDK lock with a forwarding test rather than a second lock -- D2 and Task 3, no nested locking exists so no double-lock deadlock is possible. (3) No caller bypasses `Send` by holding the raw stream -- Task 2 deletes `Stream()` and pins it by reflection; Task 3's scan test fails on `.Stream()` or a second `.conn.Send(` anywhere in the cockpit; the verification matrix carries the greps. (4) Exact commands: `go test ./...` (cockpit, and `-race` in CI after Task 4), `go test ./sdk/go/worker/...` (engine), `go vet` and `-race` for both -- in every task's Step 4 and the matrix. (5) Commit steps per repo, memql first, pin bump in the cockpit -- Task 1/2 Step 5 and Task 3/4 Step 5; the order dependency is stated at the top of Task 4. Every writer named in the audit (heartbeat `loop.go:254->360`, Ping answer `:618`, dispatch goroutines `:545->657`, modelcall deltas/keepalives `session.go:594` and `deltaStream :808`, the `Sender` passed at `loop.go:568`, pull progress `modelpull.go:246+`, app-session chunks `loop.go:561`) appears in "The defect, verified" with the helper it reaches, and every helper routes through `Connection.Send` after Task 3.

**Placeholder scan:** the only angle-bracketed tokens are `<n>` (a PR number the executor reads off `gh pr create`'s output) and `<memql-merge-sha>` (recorded in Task 2 Step 5 and consumed in Task 4); both are named where they are produced. No "TBD", no "similar to", no described-but-unshown code.

**Type consistency:** the SDK fake is `overlapDetectingStream` with `Send`, `CloseSend`, `Recv`, `closes()`, `overlaps`, `frames`, `sendErr` throughout Task 1; `errClosed` is declared in Task 1 Step 3 and referenced only by SDK code (the tests assert `err == nil`/`errors.Is(err, boom)` and never name it, so they compile before the fix). The cockpit fake is `seamStream` with `Send`/`Recv`/`Close`/`count`; the cockpit `stream` interface is `Send`/`Recv`/`Close` as in `connect.go:55-59`; the compile-time assertion uses the existing import alias `sdkworker`. Helper signatures in Task 3's forwarding test match `connect.go` exactly (`SendHeartbeat(uint32, map[string]uint32, []apps.Info, *hardware.Inventory)`, `SendPong(string, *timestamppb.Timestamp)`, `SendModelPullProgress(*memqlv1.ModelPullProgress)`, `SendModelPullEnd(*memqlv1.ModelPullEnd)`, `SendAppSessionChunk(string, string, []byte, uint64)`, `SendAppSessionEnd(*memqlv1.AppSessionEnd)`, `SendModelCallDelta(string, uint64, string, bool)`, `SendModelCallEnd(*memqlv1.ModelCallEnd)`, `SendToolResult(string, *memqlv1.Success, *memqlv1.Failure)`), and the proto fields set (`Heartbeat.ActiveCallsTotal uint32`, `Pong.RequestId/SentAt/ReceivedAt`, `ModelCallDelta.RequestId/Seq/Content/Keepalive`, `ModelPullProgress.RequestId/Status/CompletedBytes uint64`, `ModelPullEnd.RequestId/Ok`, `AppSessionEnd.SessionId`, `ModelCallEnd.RequestId/FinishReason`, `Success.ExitCode int32`) were checked against `component/grpc/gen/worker.pb.go`.
