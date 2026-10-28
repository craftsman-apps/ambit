package guard

import (
	"context"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestRecoverUnaryTurnsPanicIntoOneFailedCall(t *testing.T) {
	info := &grpc.UnaryServerInfo{FullMethod: "/test/Panic"}
	_, err := recoverUnary(t.Context(), nil, info, func(context.Context, any) (any, error) { panic("secret@example.com") })
	if status.Code(err) != codes.Internal || err.Error() == "" {
		t.Fatalf("want Internal, got %v", err)
	}
	if got := err.Error(); got != status.Error(codes.Internal, "ambit-guard: internal error").Error() {
		t.Fatalf("the error must not carry the panic value: %q", got)
	}
	// The process lives: the next call works.
	resp, err := recoverUnary(t.Context(), nil, info, func(context.Context, any) (any, error) { return "ok", nil })
	if err != nil || resp != "ok" {
		t.Fatalf("next call: %v %v", resp, err)
	}
}
