package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"runtime"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	sdkworker "github.com/znasllc-io/memql/sdk/go/worker"

	"github.com/znasllc-io/memql-cockpit/internal/worker/apps"
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
func Connect(ctx context.Context, cfg Config, inventory []apps.Info, logger *slog.Logger) (*Connection, error) {
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

	if err := c.register(ctx, cfg, inventory); err != nil {
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
func buildRegister(cfg Config, inventory []apps.Info) *memqlv1.Register {
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
		// Local apps (memql-cockpit#346). The engine derives `app:<id>`
		// routing labels from entries that are BOTH allowed and signed
		// in, and has no other way to learn any of this -- it cannot
		// dial this machine.
		Apps: appsToProto(inventory),
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
func (c *Connection) register(ctx context.Context, cfg Config, inventory []apps.Info) error {
	register := buildRegister(cfg, inventory)
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

// SendHeartbeat emits a Heartbeat envelope carrying the current app
// inventory.
//
// apps_present is ALWAYS true here, and that is load-bearing. proto3
// cannot tell an empty repeated field from an absent one, so the engine
// reads apps_present=false as "this build does not report apps" and
// leaves the stored inventory alone -- correct for an older cockpit, and
// wrong for this one on a machine that just uninstalled its last app.
// Once a build supports the field, every beat asserts the full truth,
// including "none".
func (c *Connection) SendHeartbeat(active uint32, perCap map[string]uint32, inventory []apps.Info) error {
	return c.conn.Send(&memqlv1.WorkerClientMessage{
		Payload: &memqlv1.WorkerClientMessage_Heartbeat{
			Heartbeat: buildHeartbeat(active, perCap, inventory),
		},
	})
}

// buildHeartbeat assembles the Heartbeat message. Pulled out of
// SendHeartbeat for the same reason buildRegister is pulled out of
// register: the wire shape is assertable without a live stream.
func buildHeartbeat(active uint32, perCap map[string]uint32, inventory []apps.Info) *memqlv1.Heartbeat {
	return &memqlv1.Heartbeat{
		Ts:                       timestamppb.Now(),
		ActiveCallsTotal:         active,
		ActiveCallsPerCapability: perCap,
		Apps:                     appsToProto(inventory),
		AppsPresent:              true,
	}
}

// SendAppSessionChunk emits one piece of app-session output.
//
// seq is assigned by the session and passed through unchanged, including
// on a retry: the engine drops out-of-order and duplicate chunks rather
// than appending them, so renumbering a resend would open a gap in the
// transcript that no later reader could detect.
func (c *Connection) SendAppSessionChunk(sessionID, stream string, data []byte, seq uint64) error {
	return c.conn.Send(&memqlv1.WorkerClientMessage{
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

// SendAppSessionEnd closes an app session on the wire.
func (c *Connection) SendAppSessionEnd(end *memqlv1.AppSessionEnd) error {
	return c.conn.Send(&memqlv1.WorkerClientMessage{
		Payload: &memqlv1.WorkerClientMessage_AppSessionEnd{AppSessionEnd: end},
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

// SetVersion tells the worker which version to register as.
//
// The binary's version is stamped at build time into `main.version` by
// `-ldflags -X`, and this package cannot read that -- so main hands it
// over at startup. Without this the worker registered a SECOND,
// hand-maintained constant, which had already drifted a minor version
// behind the one `memql --version` printed.
//
// That drift is the same defect TestVersionIsSettableByLdflags exists to
// prevent, one layer further out: the number the cluster stores for a
// machine, shows on /machines, and reads when deciding whether a cockpit
// is new enough to drive an app. A plausible-but-wrong version there is
// invisible in exactly the way a plausible-but-wrong version in
// `--version` was.
func SetVersion(v string) {
	if strings.TrimSpace(v) == "" {
		return
	}
	cockpitVersionValue = v
}

// cockpitVersionValue defaults to the VERSION file's contents so a build
// that never calls SetVersion -- a test, or `go run` -- reports something
// truthful rather than empty.
var cockpitVersionValue = "0.10.0"

func cockpitVersion() string { return cockpitVersionValue }
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
