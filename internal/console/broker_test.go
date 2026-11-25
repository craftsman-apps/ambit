package console

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"

	v1 "github.com/craftsman-apps/ambit/api/v1alpha1"
	"github.com/craftsman-apps/ambit/internal/controller"
	corev1 "k8s.io/api/core/v1"
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
	connect(t, conns, testAuthOnly(), "user01@example.com", "user01")
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

func ask(b *Broker, path string) string { return askAs(b, path, "-") }

// askAs asks as the gateway does: user01, with X-Ambit-Agent set to agent ("-": a person's own key or token; "none": no header).
func askAs(b *Broker, path, agent string) string {
	h, _ := askFull(b, path, agent)
	return h
}

func askFull(b *Broker, path, agent string) (token, why string) {
	req := httptest.NewRequest("GET", path, nil)
	req.Header.Set(userHeader, "user01@example.com")
	if agent != "none" {
		req.Header.Set(agentHeader, agent)
	}
	rec := httptest.NewRecorder()
	b.Handler().ServeHTTP(rec, req)
	return rec.Header().Get("Authorization"), rec.Header().Get("X-Ambit-Connect")
}

func TestBrokerGivesTokenForBoundDestination(t *testing.T) {
	b := brokerFixture(t, "https://mcp.jira.test/mcp")
	if h := ask(b, "/v1/connection/jira/server/jira-mcp/"+controller.DestinationHash("https://mcp.jira.test/mcp")); h != "Bearer at-user01" {
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
	if h := ask(b, "/v1/connection/jira"); h != "Bearer at-user01" {
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

// An image server is bound by the address of its ToolHive proxy.
func TestBrokerGivesTokenToImageServerBoundToItsProxy(t *testing.T) {
	proxy := "http://mcp-gws-proxy." + testNS + ".svc:8080/mcp"
	b := brokerFixture(t, proxy)
	img := v1.Server{ObjectMeta: metav1.ObjectMeta{Name: "gws", Namespace: testNS},
		Spec: v1.ServerSpec{Image: "ghcr.io/x/y@sha256:" + "a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0", Connection: "jira", Transport: "streamable-http"}}
	if err := b.Conns.Client.Create(t.Context(), &img); err != nil {
		t.Fatal(err)
	}
	if h := ask(b, "/v1/connection/jira/server/gws/"+controller.DestinationHash(proxy)); h != "Bearer at-user01" {
		t.Fatalf("token refused for the image server: %q", h)
	}
	if h := ask(b, "/v1/connection/jira/server/gws/"+controller.DestinationHash("https://evil.test/mcp")); h != "" {
		t.Fatal("token given for another destination")
	}
}

// Connection consent (docs/agents.md): an agent gets its owner's connection token only when the owner ticked that
// connection for it, on every request, and every doubt gives no token.
func TestBrokerChecksAgentConsent(t *testing.T) {
	b := brokerFixture(t, "https://mcp.jira.test/mcp")
	path := "/v1/connection/jira/server/jira-mcp/" + controller.DestinationHash("https://mcp.jira.test/mcp")
	svc := &KeyService{Client: b.Conns.Client, Namespace: testNS, RecordKey: testRecordKey}
	b.Agents = svc
	put := func(a Agent) {
		t.Helper()
		if a.State == "" {
			a.State = StateActive
		}
		a.Mode = cmpOr(a.Mode, ModePerson)
		a.Owner = cmpOr(a.Owner, "user01@example.com")
		a.DisplayName, a.Rev = "x", 1
		if err := b.Conns.Client.Create(t.Context(), svc.record(a)); err != nil {
			t.Fatal(err)
		}
	}
	put(Agent{Name: "allowed", ConnectionConsent: []string{"jira"}})
	put(Agent{Name: "nothing"})
	put(Agent{Name: "othermcp", ConnectionConsent: []string{"github"}})
	put(Agent{Name: "stopped", State: StateStopped, ConnectionConsent: []string{"jira"}})
	put(Agent{Name: "stopping", State: StateStopping, ConnectionConsent: []string{"jira"}})
	put(Agent{Name: "itself", Mode: ModeItself, ConnectionConsent: []string{"jira"}})
	put(Agent{Name: "theirs", Owner: "user02@example.com", ConnectionConsent: []string{"jira"}})
	for name, want := range map[string]string{"-": "Bearer at-user01", "allowed": "Bearer at-user01", "nothing": "", "othermcp": "", "stopped": "", "stopping": "",
		"itself": "", "theirs": "", "unknown": "", "": "", "ALLOWED": "", "allowed ": "Bearer at-user01"} {
		if got := askAs(b, path, name); got != want {
			t.Errorf("agent %q: %q, want %q", name, got, want)
		}
	}
	if got, why := askFull(b, path, "none"); got != "" || why == "" {
		t.Errorf("no header: %q %q", got, why)
	}
	if _, why := askFull(b, path, "nothing"); why == "" {
		t.Error("a refused agent gets no explanation")
	}
	// two header values: the gateway sends one; anything else is refused
	req := httptest.NewRequest("GET", path, nil)
	req.Header.Set(userHeader, "user01@example.com")
	req.Header.Add(agentHeader, "-")
	req.Header.Add(agentHeader, "allowed")
	rec := httptest.NewRecorder()
	b.Handler().ServeHTTP(rec, req)
	if rec.Header().Get("Authorization") != "" {
		t.Error("two agent headers were accepted")
	}
	// an unreadable record: planted without the console's signature, or damaged
	cm := svc.record(Agent{Name: "planted", DisplayName: "x", Owner: "user01@example.com", Mode: ModePerson, State: StateActive, Rev: 1, ConnectionConsent: []string{"jira"}})
	delete(cm.Annotations, agentSigAnn)
	if err := b.Conns.Client.Create(t.Context(), cm); err != nil {
		t.Fatal(err)
	}
	if got := askAs(b, path, "planted"); got != "" {
		t.Errorf("a record without the console's signature gave a token: %q", got)
	}
	// the record is read on every request: removing the consent takes effect at once
	var cur corev1.ConfigMap
	_ = b.Conns.Client.Get(t.Context(), client.ObjectKey{Namespace: testNS, Name: agentRecordPrefix + "allowed"}, &cur)
	a, _ := svc.agentFromRecord(&cur)
	a.ConnectionConsent = nil
	next := svc.record(a)
	next.ResourceVersion = cur.ResourceVersion
	if err := b.Conns.Client.Update(t.Context(), next); err != nil {
		t.Fatal(err)
	}
	if got := askAs(b, path, "allowed"); got != "" {
		t.Errorf("a removed consent still gives a token: %q", got)
	}
	// no reader: no agent gets a token, a person still does
	b.Agents = nil
	if askAs(b, path, "allowed") != "" || askAs(b, path, "-") == "" {
		t.Error("without a record reader")
	}
	// the legacy path checks too
	b2 := brokerFixture(t, "")
	b2.Agents = svc
	if askAs(b2, "/v1/connection/jira", "nothing") != "" || askAs(b2, "/v1/connection/jira", "-") == "" {
		t.Error("legacy path")
	}
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
