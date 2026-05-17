package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"runtime"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/types/known/timestamppb"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

// Connection wraps the bidi gRPC stream the worker maintains against
// the cluster.
type Connection struct {
	conn   *grpc.ClientConn
	client memqlv1.WorkerServiceClient
	stream memqlv1.WorkerService_StreamClient
	logger *slog.Logger

	RegistrationId string
	OwnerUserId    string
	RegisteredAt   time.Time
}

// Connect dials the cluster, opens the stream, and registers the
// worker. Returns the live connection and the registration metadata
// pulled from the RegisterAck.
func Connect(ctx context.Context, cfg Config, logger *slog.Logger) (*Connection, error) {
	endpoint, useTLS, err := parseClusterURL(cfg.ClusterURL)
	if err != nil {
		return nil, err
	}
	if logger != nil {
		// Log dial intent at INFO so the operator can see in the
		// cockpit terminal exactly which cluster + transport this
		// worker is connecting to. Without this the only visible
		// signal was "worker registered with cluster" AFTER the
		// stream + RegisterAck round-trip succeeded -- when the dial
		// hung or the stream errored before ack, the user saw nothing.
		logger.Info("worker connecting to cluster",
			"endpoint", endpoint,
			"tls", useTLS,
		)
	}
	// Message-size limits: gRPC's default cap is 4 MiB, which is too
	// tight for workerComputer.screenshot results -- a retina-display
	// PNG base64-encoded commonly lands at 5-10 MiB. The 4 MiB
	// default caused mid-stream RST_STREAM (server-side) on every
	// screenshot, which surfaced on the cockpit side as "worker
	// stream ended; will reconnect" and on the agent side as
	// errorCode=worker_disconnected. Bumping both directions to
	// 32 MiB covers a 6K capture with headroom; the agent gRPC
	// server has a matching bump in component/grpc/server.go.
	const maxWorkerMessageSize = 32 * 1024 * 1024
	dialOpts := []grpc.DialOption{
		grpc.WithDefaultCallOptions(
			grpc.MaxCallRecvMsgSize(maxWorkerMessageSize),
			grpc.MaxCallSendMsgSize(maxWorkerMessageSize),
		),
	}
	if useTLS {
		dialOpts = append(dialOpts, grpc.WithTransportCredentials(credentials.NewTLS(nil)))
	} else {
		dialOpts = append(dialOpts, grpc.WithTransportCredentials(insecure.NewCredentials()))
	}

	conn, err := grpc.NewClient(endpoint, dialOpts...)
	if err != nil {
		return nil, fmt.Errorf("worker.connect: dial %s: %w", endpoint, err)
	}

	// IMPORTANT: derive the stream context from the caller's ctx so
	// stream.Recv() unblocks when the runner's parent ctx is
	// cancelled (Ctrl+C / Ctrl+Q / SIGTERM). The previous version
	// used context.Background() here, which left the stream blocked
	// in Recv() forever after shutdown -- the user saw "worker
	// shutting down" but the process never actually exited.
	streamCtx := metadata.AppendToOutgoingContext(ctx, "authorization", "Worker "+cfg.Token)

	client := memqlv1.NewWorkerServiceClient(conn)
	stream, err := client.Stream(streamCtx)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("worker.connect: open stream: %w", err)
	}

	c := &Connection{
		conn:   conn,
		client: client,
		stream: stream,
		logger: logger,
	}

	if err := c.register(ctx, cfg); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

// register sends the Register message and waits for the RegisterAck.
func (c *Connection) register(ctx context.Context, cfg Config) error {
	hostname, _ := os.Hostname()
	register := &memqlv1.Register{
		Name:         cfg.Name,
		Capabilities: cfg.Capabilities,
		Labels:       cfg.Labels,
		Concurrency:  cfg.Concurrency,
		Platform: &memqlv1.PlatformInfo{
			Os:       runtime.GOOS,
			Arch:     runtime.GOARCH,
			Hostname: hostname,
		},
		Permissions: probePermissions(),
		Version:     cockpitVersion(),
		BuildTag:    cockpitBuildTag(),
	}
	if err := c.stream.Send(&memqlv1.WorkerClientMessage{
		Payload: &memqlv1.WorkerClientMessage_Register{Register: register},
	}); err != nil {
		return fmt.Errorf("worker.register: send: %w", err)
	}

	resp, err := c.stream.Recv()
	if err != nil {
		return fmt.Errorf("worker.register: recv: %w", err)
	}
	if errMsg := resp.GetRegisterError(); errMsg != nil {
		return fmt.Errorf("worker.register: %s: %s", errMsg.GetCode(), errMsg.GetMessage())
	}
	ack := resp.GetRegisterAck()
	if ack == nil {
		return errors.New("worker.register: server returned no ack")
	}
	c.RegistrationId = ack.GetRegistrationId()
	c.OwnerUserId = ack.GetOwnerUserId()
	if ts := ack.GetRegisteredAt(); ts != nil {
		c.RegisteredAt = ts.AsTime()
	}
	if c.logger != nil {
		c.logger.Info("worker registered with cluster",
			"registration_id", c.RegistrationId,
			"owner_user_id", c.OwnerUserId,
		)
	}
	return nil
}

// Send writes a single message on the worker side of the stream.
func (c *Connection) Send(msg *memqlv1.WorkerClientMessage) error {
	return c.stream.Send(msg)
}

// Recv blocks until the next inbound message lands.
func (c *Connection) Recv() (*memqlv1.WorkerServerMessage, error) {
	return c.stream.Recv()
}

// SendHeartbeat emits a Heartbeat envelope.
func (c *Connection) SendHeartbeat(active uint32, perCap map[string]uint32) error {
	return c.stream.Send(&memqlv1.WorkerClientMessage{
		Payload: &memqlv1.WorkerClientMessage_Heartbeat{
			Heartbeat: &memqlv1.Heartbeat{
				Ts:                       timestamppb.Now(),
				ActiveCallsTotal:         active,
				ActiveCallsPerCapability: perCap,
			},
		},
	})
}

// SendToolResult emits a ToolResult envelope.
func (c *Connection) SendToolResult(callId string, success *memqlv1.Success, failure *memqlv1.Failure) error {
	res := &memqlv1.ToolResult{CallId: callId}
	if success != nil {
		res.Payload = &memqlv1.ToolResult_Success{Success: success}
	} else if failure != nil {
		res.Payload = &memqlv1.ToolResult_Failure{Failure: failure}
	}
	return c.stream.Send(&memqlv1.WorkerClientMessage{
		Payload: &memqlv1.WorkerClientMessage_ToolResult{ToolResult: res},
	})
}

// Close terminates the stream and connection.
func (c *Connection) Close() {
	if c == nil {
		return
	}
	if c.stream != nil {
		_ = c.stream.CloseSend()
	}
	if c.conn != nil {
		_ = c.conn.Close()
	}
}

// parseClusterURL accepts http://, https://, grpc://, grpcs://, or
// bare host:port. Returns the gRPC dial address (host:port) and a
// flag indicating whether TLS is required.
func parseClusterURL(raw string) (endpoint string, useTLS bool, err error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", false, errors.New("worker.connect: cluster_url is empty")
	}
	if !strings.Contains(raw, "://") {
		return raw, false, nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", false, fmt.Errorf("worker.connect: parse cluster_url: %w", err)
	}
	host := u.Host
	if host == "" {
		return "", false, fmt.Errorf("worker.connect: cluster_url missing host: %s", raw)
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "grpc":
		return host, false, nil
	case "https", "grpcs":
		// Default https URLs target :443 -- gRPC default is :443.
		if !strings.Contains(host, ":") {
			host = host + ":443"
		}
		return host, true, nil
	default:
		return host, false, nil
	}
}

func cockpitVersion() string { return "0.1.0" }
func cockpitBuildTag() string {
	if buildTagOverride != "" {
		return buildTagOverride
	}
	return "nogui"
}

// buildTagOverride is set at link/init time by the gui-tagged
// platform layer to "gui". The default build leaves it empty so
// cockpitBuildTag() reports "nogui".
var buildTagOverride string

// probePermissions performs the platform-specific permissions
// probe. The MVP no-op returns "unknown" everywhere; Phase 4 wires
// macOS TCC + Linux X11 detection.
func probePermissions() *memqlv1.PermissionStatus {
	return &memqlv1.PermissionStatus{
		Detail: "permission probe not yet implemented (MVP)",
	}
}
