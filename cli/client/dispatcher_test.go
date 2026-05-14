package client

import (
	"context"
	"testing"
	"time"

	memqlv1 "github.com/visionarys-io/memql/component/grpc/gen"
	"google.golang.org/grpc/metadata"
)

// mockStream implements the minimal MemqlService_StreamClient interface for testing.
type mockStream struct {
	sendCh chan *memqlv1.MemqlClientMessage
	recvCh chan *memqlv1.MemqlServerMessage
	closed bool
}

func newMockStream() *mockStream {
	return &mockStream{
		sendCh: make(chan *memqlv1.MemqlClientMessage, 10),
		recvCh: make(chan *memqlv1.MemqlServerMessage, 10),
	}
}

func (m *mockStream) Send(msg *memqlv1.MemqlClientMessage) error {
	m.sendCh <- msg
	return nil
}

func (m *mockStream) Recv() (*memqlv1.MemqlServerMessage, error) {
	msg, ok := <-m.recvCh
	if !ok {
		return nil, context.Canceled
	}
	return msg, nil
}

// Implement remaining grpc.ClientStream methods as no-ops.
func (m *mockStream) Header() (metadata.MD, error)          { return nil, nil }
func (m *mockStream) Trailer() metadata.MD                  { return nil }
func (m *mockStream) CloseSend() error                      { close(m.recvCh); return nil }
func (m *mockStream) Context() context.Context               { return context.Background() }
func (m *mockStream) SendMsg(any) error                      { return nil }
func (m *mockStream) RecvMsg(any) error                      { return nil }

func TestDispatcherCorrelation(t *testing.T) {
	stream := newMockStream()
	d := NewDispatcher(stream, nil)
	go d.Run()
	defer d.Stop()

	// Send a message and simulate a correlated response.
	msg := &memqlv1.MemqlClientMessage{
		MessageId: "test-123",
		Payload: &memqlv1.MemqlClientMessage_ClientHello{
			ClientHello: &memqlv1.ClientHello{ClientId: "test"},
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// Start SendAndWait in a goroutine.
	respCh := make(chan *memqlv1.MemqlServerMessage, 1)
	errCh := make(chan error, 1)
	go func() {
		resp, err := d.SendAndWait(ctx, msg)
		respCh <- resp
		errCh <- err
	}()

	// Read the sent message.
	sent := <-stream.sendCh
	if sent.GetMessageId() != "test-123" {
		t.Fatalf("expected message_id 'test-123', got %q", sent.GetMessageId())
	}

	// Inject a correlated response.
	stream.recvCh <- &memqlv1.MemqlServerMessage{
		CorrelateTo: "test-123",
		Payload: &memqlv1.MemqlServerMessage_ServerHello{
			ServerHello: &memqlv1.ServerHello{
				NodeId:  "node-1",
				Version: "1.0",
			},
		},
	}

	// Check the response.
	resp := <-respCh
	err := <-errCh
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.GetServerHello().GetNodeId() != "node-1" {
		t.Errorf("expected node_id 'node-1', got %q", resp.GetServerHello().GetNodeId())
	}
}

func TestDispatcherUncorrelatedEvents(t *testing.T) {
	stream := newMockStream()
	d := NewDispatcher(stream, nil)
	go d.Run()
	defer d.Stop()

	// Inject an uncorrelated event.
	stream.recvCh <- &memqlv1.MemqlServerMessage{
		Payload: &memqlv1.MemqlServerMessage_Heartbeat{
			Heartbeat: &memqlv1.HeartbeatMsg{},
		},
	}

	// Should appear on the events channel.
	select {
	case msg := <-d.Events():
		if msg.GetHeartbeat() == nil {
			t.Error("expected heartbeat event")
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for event")
	}
}

func TestDispatcherSendAssignsId(t *testing.T) {
	stream := newMockStream()
	d := NewDispatcher(stream, nil)

	msg := &memqlv1.MemqlClientMessage{}
	id, err := d.Send(msg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id == "" {
		t.Error("expected non-empty message_id")
	}

	sent := <-stream.sendCh
	if sent.GetMessageId() != id {
		t.Errorf("sent message_id %q does not match returned id %q", sent.GetMessageId(), id)
	}
}
