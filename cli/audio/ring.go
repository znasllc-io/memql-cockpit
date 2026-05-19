//go:build voice

package audio

import (
	"io"
	"sync"
)

// ringReader is an unbounded-write / bounded-storage byte buffer that
// implements io.Reader. The audio callback writes from the malgo
// thread; consumers Read from any goroutine. Oldest bytes are
// dropped if the consumer falls behind, bounded to the configured
// capacity. close() unblocks any pending Read with io.EOF.
type ringReader struct {
	mu       sync.Mutex
	cond     *sync.Cond
	buf      []byte
	cap      int
	closed   bool
}

func newRingReader(capacity int) *ringReader {
	r := &ringReader{cap: capacity}
	r.cond = sync.NewCond(&r.mu)
	return r
}

func (r *ringReader) write(p []byte) {
	r.mu.Lock()
	r.buf = append(r.buf, p...)
	if len(r.buf) > r.cap {
		// Drop oldest bytes -- audio is real-time; stale frames are
		// useless. Logging this on every overrun would be noisy; the
		// caller is expected to drain via voice.PushToTalk's pump
		// goroutine which keeps up with 16kHz mono PCM16 trivially.
		r.buf = r.buf[len(r.buf)-r.cap:]
	}
	r.cond.Signal()
	r.mu.Unlock()
}

func (r *ringReader) Read(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for len(r.buf) == 0 && !r.closed {
		r.cond.Wait()
	}
	if len(r.buf) == 0 && r.closed {
		return 0, io.EOF
	}
	n := copy(p, r.buf)
	r.buf = r.buf[n:]
	return n, nil
}

func (r *ringReader) close() {
	r.mu.Lock()
	r.closed = true
	r.cond.Broadcast()
	r.mu.Unlock()
}
