package guard

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// recoverUnary turns a panic in a handler into one failed call. grpc-go does not recover handler panics:
// without this the process would exit, and the gateway (FailClosed) would then fail every MCP call for every
// user until the pod restarts. Only the type of the panic value is logged, never its text (it may hold content).
func recoverUnary(ctx context.Context, req any, info *grpc.UnaryServerInfo, next grpc.UnaryHandler) (resp any, err error) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("guard: handler panic; call failed", "method", info.FullMethod, "panic", fmt.Sprintf("%T", r), "stack", string(debug.Stack()))
			resp, err = nil, status.Error(codes.Internal, "ambit-guard: internal error")
		}
	}()
	return next(ctx, req)
}
