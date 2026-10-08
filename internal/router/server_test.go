package router

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/craftsman-apps/ambit/internal/routerconfig"
	extproc "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func serve(t *testing.T, h *Handler, drain time.Duration) (*Server, extproc.ExternalProcessorClient) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := NewServer(h, drain)
	go func() { _ = srv.Serve(lis) }()
	conn, err := grpc.NewClient("passthrough:///"+lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return srv, extproc.NewExternalProcessorClient(conn)
}

func headersOnly(cfg string) *extproc.ProcessingRequest {
	return &extproc.ProcessingRequest{Attributes: attrs([]string{"eng"}, cfg), Request: &extproc.ProcessingRequest_RequestHeaders{RequestHeaders: &extproc.HttpHeaders{}}}
}

func newStreamErr(t *testing.T, c extproc.ExternalProcessorClient, cfg string) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	s, err := c.Process(ctx)
	if err != nil {
		return err
	}
	if err := s.Send(headersOnly(cfg)); err != nil {
		return err
	}
	_, err = s.Recv()
	return err
}

// The gateway has sent the headers and is still receiving the client's upload when the pod is told to stop: the
// stream must survive and its body phase must complete within the drain budget.
func TestShutdownWaitsForUpload(t *testing.T) {
	cfg := cfgOf(auto) // timeout 200 ms; the classifier answers in 50 ms
	h := handler(&fakeLLM{answer: "coding", delay: 50 * time.Millisecond}, cfg)
	srv, c := serve(t, h, 2*time.Second)
	if !srv.Ready(h.Store) {
		t.Fatal("ready with a loaded configuration")
	}
	s, err := c.Process(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Send(headersOnly(cfg.Hash))
	if _, err := s.Recv(); err != nil {
		t.Fatal(err)
	}
	stopped := make(chan struct{})
	go func() { srv.Shutdown(); close(stopped) }()
	time.Sleep(300 * time.Millisecond) // the upload continues while the server drains
	if srv.Ready(h.Store) {
		t.Fatal("not ready once shutdown started")
	}
	if err := newStreamErr(t, c, cfg.Hash); err == nil {
		t.Fatal("a new stream must be refused during shutdown")
	}
	_ = s.Send(&extproc.ProcessingRequest{Request: &extproc.ProcessingRequest_RequestBody{RequestBody: &extproc.HttpBody{Body: body("auto", "code"), EndOfStream: true}}})
	r, err := s.Recv()
	if err != nil || headersOf(r)["x-ambit-route"] != "coding" {
		t.Fatalf("held stream must finish: %v %v", err, r)
	}
	_ = s.CloseSend()
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("GracefulStop must return once the held stream ended")
	}
}

func TestShutdownStopsAfterDrain(t *testing.T) {
	cfg := cfgOf(auto)
	h := handler(&fakeLLM{answer: "coding"}, cfg)
	srv, c := serve(t, h, 200*time.Millisecond)
	s, err := c.Process(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Send(headersOnly(cfg.Hash))
	if _, err := s.Recv(); err != nil {
		t.Fatal(err)
	}
	// no body: the stream would stay open forever
	t0 := time.Now()
	srv.Shutdown()
	if d := time.Since(t0); d < 200*time.Millisecond || d > 450*time.Millisecond {
		t.Fatalf("Shutdown took %v, want the 200 ms drain budget", d)
	}
	if _, err := s.Recv(); err == nil {
		t.Fatal("the held stream must be closed after the drain budget")
	}
}

func TestNotReadyWithoutConfiguration(t *testing.T) {
	srv := NewServer(&Handler{Store: &Store{}}, time.Second)
	st := &Store{}
	if srv.Ready(st) {
		t.Fatal("nothing loaded")
	}
	st.Set(routerconfig.Config{}, nil) // a deleted ConfigMap: empty configuration, no hash
	if srv.Ready(st) {
		t.Fatal("an empty configuration is not ready")
	}
}
