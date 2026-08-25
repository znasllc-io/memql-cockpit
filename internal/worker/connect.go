package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"runtime"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	sdkworker "github.com/znasllc-io/memql/sdk/go/worker"

	"github.com/znasllc-io/memql-cockpit/internal/worker/tools"
)

// Connection wraps the bidi gRPC stream the worker maintains against
// the cluster. Transport (dial / TLS / stream opener) is handled by
// the SDK's worker module; this struct adds the worker-protocol
// lifecycle on top: Register / RegisterAck capture, heartbeat /
// tool-result helpers, and the registration metadata the runner
// reads after Connect.
type Connection struct {
	conn   *sdkworker.Connection
	logger *slog.Logger

	RegistrationId string
	OwnerUserId    string
	RegisteredAt   time.Time
}

// Connect dials the cluster (via the SDK), opens the stream, and
// sends the worker-protocol Register handshake. Returns the live
// connection and the registration metadata pulled from the
// RegisterAck.
func Connect(ctx context.Context, cfg Config, logger *slog.Logger) (*Connection, error) {
	endpoint, useTLS, err := sdkworker.ParseClusterURL(cfg.ClusterURL)
	if err != nil {
		return nil, err
	}

	sdkConn, err := sdkworker.Dial(ctx, sdkworker.DialConfig{
		Endpoint: endpoint,
		UseTLS:   useTLS,
		Token:    cfg.Token,
		Logger:   logger,
	})
	if err != nil {
		return nil, err
	}

	c := &Connection{
		conn:   sdkConn,
		logger: logger,
	}

	if err := c.register(ctx, cfg); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

// buildRegister assembles the worker-protocol Register handshake
// message. Pulled out of register() so tests can assert the wire
// shape -- in particular that capability_descriptor_json always
// satisfies the server-side validation rules (memql#1331: raw size,
// schemaVersion, action-name pattern) -- without a live stream.
func buildRegister(cfg Config) *memqlv1.Register {
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
	// Capability descriptor (memql-cockpit#166): the same JSON the
	// workerComputer.capabilities action returns, sent up front so
	// the server knows the action surface at registration time. The
	// proto field is optional -- on the (never-expected) marshal
	// failure we register without it rather than fail the handshake;
	// the server treats omission as valid.
	if descJSON, err := tools.CapabilityDescriptorJSON(); err == nil {
		register.CapabilityDescriptorJson = descJSON
	}
	return register
}

// register sends the Register message and waits for the RegisterAck.
func (c *Connection) register(ctx context.Context, cfg Config) error {
	register := buildRegister(cfg)
	if err := c.conn.Send(&memqlv1.WorkerClientMessage{
		Payload: &memqlv1.WorkerClientMessage_Register{Register: register},
	}); err != nil {
		return fmt.Errorf("worker.register: send: %w", err)
	}

	resp, err := c.conn.Recv()
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
	return c.conn.Send(msg)
}

// Recv blocks until the next inbound message lands.
func (c *Connection) Recv() (*memqlv1.WorkerServerMessage, error) {
	return c.conn.Recv()
}

// SendHeartbeat emits a Heartbeat envelope.
func (c *Connection) SendHeartbeat(active uint32, perCap map[string]uint32) error {
	return c.conn.Send(&memqlv1.WorkerClientMessage{
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
	return c.conn.Send(&memqlv1.WorkerClientMessage{
		Payload: &memqlv1.WorkerClientMessage_ToolResult{ToolResult: res},
	})
}

// Close terminates the stream and the underlying SDK connection.
func (c *Connection) Close() {
	if c == nil {
		return
	}
	c.conn.Close()
}

func cockpitVersion() string { return "0.9.0" }
func cockpitBuildTag() string {
	if buildTagOverride != "" {
		return buildTagOverride
	}
	return "headless"
}

// buildTagOverride is set at link/init time by the computeruse-tagged
// platform layer to "computeruse". The default build leaves it empty so
// cockpitBuildTag() reports "headless".
var buildTagOverride string

// probePermissions performs the platform-specific permissions
// probe. The MVP no-op returns "unknown" everywhere; Phase 4 wires
// macOS TCC + Linux X11 detection.
func probePermissions() *memqlv1.PermissionStatus {
	return &memqlv1.PermissionStatus{
		Detail: "permission probe not yet implemented (MVP)",
	}
}
