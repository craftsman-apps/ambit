package console

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"

	v1 "github.com/craftsman-apps/ambit/api/v1alpha1"
	"github.com/craftsman-apps/ambit/internal/controller"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func brokerFixture(t *testing.T, serverURL string) *Broker {
	saas := newFakeSaaS(t)
	conns, c := connFixture(t, saas)
	var conn v1.Connection
	if err := c.Get(t.Context(), client.ObjectKey{Namespace: testNS, Name: "jira"}, &conn); err != nil {
		t.Fatal(err)
	}
	conn.Spec.ServerURL = serverURL // set before connecting: the grant records the URL it was issued for
	if err := c.Update(t.Context(), &conn); err != nil {
		t.Fatal(err)
	}
	connect(t, conns, testAuthOnly(), "alice@acme.test", "alice")
	for _, s := range []v1.Server{
		{ObjectMeta: metav1.ObjectMeta{Name: "jira-mcp", Namespace: testNS},
			Spec: v1.ServerSpec{URL: "https://mcp.jira.test/mcp", Connection: "jira", Transport: "streamable-http"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "other-conn", Namespace: testNS}, // uses a different Connection
			Spec: v1.ServerSpec{URL: "https://mcp.jira.test/mcp", Connection: "github", Transport: "streamable-http"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "jira-evil", Namespace: testNS}, // uses jira, but another URL
			Spec: v1.ServerSpec{URL: "https://evil.test/mcp", Connection: "jira", Transport: "streamable-http"}},
	} {
		if err := c.Create(t.Context(), &s); err != nil {
			t.Fatal(err)
		}
	}
	return &Broker{Conns: conns}
}

func ask(b *Broker, path string) string {
	req := httptest.NewRequest("GET", path, nil)
	req.Header.Set(userHeader, "alice@acme.test")
	rec := httptest.NewRecorder()
	b.Handler().ServeHTTP(rec, req)
	return rec.Header().Get("Authorization")
}

func TestBrokerGivesTokenForBoundDestination(t *testing.T) {
	b := brokerFixture(t, "https://mcp.jira.test/mcp")
	if h := ask(b, "/v1/connection/jira/server/jira-mcp/"+controller.DestinationHash("https://mcp.jira.test/mcp")); h != "Bearer at-alice" {
		t.Fatalf("token refused: %q", h)
	}
}

func TestBrokerRefusesStaleDestination(t *testing.T) {
	b := brokerFixture(t, "https://mcp.jira.test/mcp")
	for _, p := range []string{
		"/v1/connection/jira/server/jira-mcp/" + controller.DestinationHash("https://old.jira.test/mcp"),   // old gateway config
		"/v1/connection/jira/server/other/" + controller.DestinationHash("https://mcp.jira.test/mcp"),      // no such server
		"/v1/connection/jira/server/other-conn/" + controller.DestinationHash("https://mcp.jira.test/mcp"), // server uses another Connection
		"/v1/connection/jira/server/jira-evil/" + controller.DestinationHash("https://evil.test/mcp"),      // own hash, but not jira's serverURL
		"/v1/connection/jira", // legacy path for a bound Connection
	} {
		if h := ask(b, p); h != "" {
			t.Fatalf("%s: token given", p)
		}
	}
}

func TestBrokerLegacyPathForUnboundConnection(t *testing.T) {
	b := brokerFixture(t, "")
	if h := ask(b, "/v1/connection/jira"); h != "Bearer at-alice" {
		t.Fatalf("legacy path refused: %q", h)
	}
}

// failingGet fails the first Get of a Connection with a non-NotFound error.
type failingGet struct {
	client.Client
	failed bool
}

func (f *failingGet) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if _, ok := obj.(*v1.Connection); ok && !f.failed {
		f.failed = true
		return errors.New("the API server is unavailable")
	}
	return f.Client.Get(ctx, key, obj, opts...)
}

func TestBrokerLegacyPathRefusesWhenConnectionLookupFails(t *testing.T) {
	b := brokerFixture(t, "https://mcp.jira.test/mcp")
	b.Conns.Client = &failingGet{Client: b.Conns.Client}
	if h := ask(b, "/v1/connection/jira"); h != "" {
		t.Fatalf("token given after a failed lookup: %q", h)
	}
}
