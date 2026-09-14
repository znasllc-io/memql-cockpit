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
// audit finding C-1 (memql-cockpit#426). grpc-go forbids concurrent
// SendMsg on one stream; the lock that serializes it lives in the SDK's
// Connection.Send (memql sdk/go/worker); and a cockpit write reaches that
// lock only by calling the SDK connection's Send. So exactly ONE place in
// this repository may call it -- Connection.Send in connect.go -- and
// every Send* helper routes through that. The other way around the lock,
// the SDK's raw-stream accessor, is gone on the engine side; this guard
// keeps a call to it from coming back here against an older pin.
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

// seamStream stands where the SDK's connection would be. It locks, as the
// SDK does, and records: the test below proves that every kind of write
// the runner makes -- from the goroutines that make them, all at once --
// lands on the seam as one frame of the expected kind, and that this
// Connection keeps no unsynchronized state on the way (run under -race).
// Serialization itself is proven where the lock is, in the SDK (memql
// sdk/go/worker/send_serialization_test.go).
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
		{"pong", func(i int) error { return conn.SendPong("ping-"+strconv.Itoa(i), timestamppb.Now()) },
			func(m *memqlv1.WorkerClientMessage) bool { return m.GetPong() != nil }},
		{"model pull progress", func(i int) error {
			return conn.SendModelPullProgress(&memqlv1.ModelPullProgress{RequestId: "pull-1", Status: "pulling", CompletedBytes: uint64(i)})
		}, func(m *memqlv1.WorkerClientMessage) bool { return m.GetModelPullProgress() != nil }},
		{"model pull end", func(i int) error {
			return conn.SendModelPullEnd(&memqlv1.ModelPullEnd{RequestId: "pull-" + strconv.Itoa(i), Ok: true})
		}, func(m *memqlv1.WorkerClientMessage) bool { return m.GetModelPullEnd() != nil }},
		{"heartbeat", func(i int) error { return conn.SendHeartbeat(uint32(i), nil, nil, nil) },
			func(m *memqlv1.WorkerClientMessage) bool { return m.GetHeartbeat() != nil }},
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
