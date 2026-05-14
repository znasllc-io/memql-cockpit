// Package client manages the gRPC connection to a memQL cluster.
// It provides a persistent bidirectional stream with automatic correlation
// of request/response messages.
package client

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
	memqlv1 "github.com/visionarys-io/memql/component/grpc/gen"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
)

// Connection holds an active gRPC stream to a memQL cluster.
type Connection struct {
	conn       *grpc.ClientConn
	client     memqlv1.MemqlServiceClient
	stream     memqlv1.MemqlService_StreamClient
	dispatcher *Dispatcher
	logger     *slog.Logger

	// Server info from handshake.
	NodeId  string
	Version string
}

// ConnectConfig configures a new gRPC connection.
type ConnectConfig struct {
	Endpoint string
	Token    string // JWT bearer token (empty for no-auth mode)
	Logger   *slog.Logger
}

// Connect dials the gRPC endpoint, opens a bidirectional stream,
// and performs the ClientHello/ServerHello handshake.
func Connect(ctx context.Context, cfg ConnectConfig) (*Connection, error) {
	conn, err := grpc.NewClient(cfg.Endpoint,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", cfg.Endpoint, err)
	}

	// The stream context must outlive the connect timeout.
	// Use a background context for the stream's lifetime, with metadata if needed.
	streamCtx := context.Background()
	if cfg.Token != "" {
		md := metadata.Pairs("authorization", "Bearer "+cfg.Token)
		streamCtx = metadata.NewOutgoingContext(streamCtx, md)
	}

	client := memqlv1.NewMemqlServiceClient(conn)
	stream, err := client.Stream(streamCtx)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("open stream: %w", err)
	}

	c := &Connection{
		conn:   conn,
		client: client,
		stream: stream,
		logger: cfg.Logger,
	}

	// Start the response dispatcher.
	c.dispatcher = NewDispatcher(stream, cfg.Logger)
	go c.dispatcher.Run()

	// Perform handshake.
	if err := c.handshake(ctx); err != nil {
		c.Close()
		return nil, fmt.Errorf("handshake: %w", err)
	}

	return c, nil
}

// handshake sends ClientHello and waits for ServerHello.
func (c *Connection) handshake(ctx context.Context) error {
	helloMsg := &memqlv1.MemqlClientMessage{
		MessageId: uuid.NewString(),
		Payload: &memqlv1.MemqlClientMessage_ClientHello{
			ClientHello: &memqlv1.ClientHello{
				ClientId:   "memql-cockpit",
				SdkName:    "memql",
				SdkVersion: "0.1.0",
			},
		},
	}

	resp, err := c.dispatcher.SendAndWait(ctx, helloMsg)
	if err != nil {
		return fmt.Errorf("send ClientHello: %w", err)
	}

	if hello := resp.GetServerHello(); hello != nil {
		c.NodeId = hello.GetNodeId()
		c.Version = hello.GetVersion()
		if c.logger != nil {
			c.logger.Info("connected to memQL node",
				"nodeId", c.NodeId,
				"version", c.Version,
			)
		}
	}

	return nil
}

// Dispatcher returns the message dispatcher for sending/receiving messages.
func (c *Connection) Dispatcher() *Dispatcher {
	return c.dispatcher
}

// Close shuts down the stream and connection.
func (c *Connection) Close() {
	if c.dispatcher != nil {
		c.dispatcher.Stop()
	}
	if c.stream != nil {
		c.stream.CloseSend()
	}
	if c.conn != nil {
		c.conn.Close()
	}
}
