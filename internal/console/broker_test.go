package console

import (
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
	_ = c.Get(t.Context(), client.ObjectKey{Namespace: testNS, Name: "jira"}, &conn)
	conn.Spec.ServerURL = serverURL // set before connecting: the grant records the URL it was issued for
	_ = c.Update(t.Context(), &conn)
	connect(t, conns, testAuthOnly(), "alice@acme.test", "alice")
	_ = c.Create(t.Context(), &v1.Server{ObjectMeta: metav1.ObjectMeta{Name: "jira-mcp", Namespace: testNS},
		Spec: v1.ServerSpec{URL: "https://mcp.jira.test/mcp", Connection: "jira", Transport: "streamable-http"}})
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
		"/v1/connection/jira/server/jira-mcp/" + controller.DestinationHash("https://old.jira.test/mcp"), // old gateway config
		"/v1/connection/jira/server/other/" + controller.DestinationHash("https://mcp.jira.test/mcp"),    // server not using jira
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
