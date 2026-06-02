package cluster

// gRPC status extraction for the deploy-control modal's error mapping.
// The SDK's DeployControlClient wraps the raw gRPC status error with
// fmt.Errorf("...: %w", err), so the underlying status is recoverable
// via status.FromError walking the wrap chain. We keep this in its own
// file so the codes/status imports stay isolated from the modal logic.

import (
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// permissionDeniedCode is the gRPC code the server returns for a
// non-admin caller attempting a deploy-control action.
const permissionDeniedCode = codes.PermissionDenied

// grpcStatusFromError returns the gRPC status code embedded in err (or
// any error it wraps), and whether one was found. status.FromError
// unwraps via the GRPCStatus() / errors.As path.
func grpcStatusFromError(err error) (codes.Code, bool) {
	if err == nil {
		return codes.OK, false
	}
	if st, ok := status.FromError(err); ok {
		return st.Code(), true
	}
	return codes.Unknown, false
}
