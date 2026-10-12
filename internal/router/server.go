package router

import (
	"net"
	"sync/atomic"
	"time"

	extproc "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"google.golang.org/grpc"
)

// Server is the gRPC side of ambit-router, with the shutdown the spec asks for ("Shutdown").
type Server struct {
	grpc    *grpc.Server
	drain   time.Duration
	closing atomic.Bool
}

func NewServer(h *Handler, drain time.Duration) *Server {
	if drain <= 0 {
		drain = 25 * time.Second
	}
	g := grpc.NewServer()
	extproc.RegisterExternalProcessorServer(g, h)
	return &Server{grpc: g, drain: drain}
}

func (s *Server) Serve(lis net.Listener) error { return s.grpc.Serve(lis) }

// Shutdown runs on SIGTERM, after Kubernetes' preStop sleep (the pod is already out of the Service endpoints):
// readiness turns false, GracefulStop sends GOAWAY and lets held streams finish, and Stop closes what is left
// after the drain budget.
func (s *Server) Shutdown() {
	s.closing.Store(true)
	done := make(chan struct{})
	go func() { s.grpc.GracefulStop(); close(done) }()
	select {
	case <-done:
	case <-time.After(s.drain):
		s.grpc.Stop()
	}
}

// Ready: a configuration with a hash is loaded (a deleted ConfigMap leaves an empty one: not ready, so the pod
// leaves the endpoints and the gateway fails open), and shutdown has not started.
func (s *Server) Ready(st *Store) bool {
	snap := st.Snapshot()
	return !s.closing.Load() && snap != nil && snap.Config.Hash != ""
}
