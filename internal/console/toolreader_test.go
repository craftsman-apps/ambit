package console

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	v1 "github.com/craftsman-apps/ambit/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const jiraMCP = "https://mcp.jira.test/mcp"

func readerFixture(t *testing.T) (*ToolReader, client.Client, *[]string) {
	saas := newFakeSaaS(t)
	conns, c := connFixture(t, saas)
	var conn v1.Connection
	_ = c.Get(t.Context(), client.ObjectKey{Namespace: testNS, Name: "jira"}, &conn)
	conn.Spec.ServerURL = jiraMCP
	_ = c.Update(t.Context(), &conn)
	connect(t, conns, testAuthOnly(), "admin@acme.test", "admin")
	connect(t, conns, testAuthOnly(), "lee@acme.test", "lee")
	_ = c.Create(t.Context(), &v1.Server{ObjectMeta: metav1.ObjectMeta{Name: "jira-mcp", Namespace: testNS, UID: "uid-jira-mcp"},
		Spec: v1.ServerSpec{URL: jiraMCP, Connection: "jira", Transport: "streamable-http"}})
	var mu sync.Mutex
	var used []string
	tr := &ToolReader{Conns: conns, Namespace: testNS, TTL: time.Minute,
		List: func(_ context.Context, _ *http.Client, endpoint, _, bearer string) ([]v1.Tool, error) {
			mu.Lock()
			used = append(used, endpoint+" "+bearer)
			mu.Unlock()
			return []v1.Tool{{Name: "get_issue"}}, nil
		}}
	return tr, c, &used
}

func TestReaderReadsWithOwnGrantOnly(t *testing.T) {
	tr, _, used := readerFixture(t)
	if _, err := tr.Tools(t.Context(), "jira-mcp"); !errors.Is(err, ErrNoReader) {
		t.Fatalf("no reader yet: %v", err)
	}
	if err := tr.SetReader(t.Context(), "admin@acme.test", "jira-mcp"); err != nil {
		t.Fatal(err)
	}
	res, err := tr.Tools(t.Context(), "jira-mcp")
	if err != nil || res.Reader != "admin@acme.test" || len(res.Tools) != 1 || (*used)[0] != jiraMCP+" at-admin" {
		t.Fatalf("res %+v err %v used %v", res, err, *used)
	}
	if err := tr.SetReader(t.Context(), "lee@acme.test", "jira-mcp"); err != nil { // a new reader replaces the old one
		t.Fatal(err)
	}
	if who, ok, _ := tr.Reader(t.Context(), "jira-mcp"); !ok || who != "lee@acme.test" {
		t.Fatalf("reader %s", who)
	}
	if g, _ := tr.Conns.Store.Get(t.Context(), "admin@acme.test", "jira"); len(g.ToolReaderFor) != 0 {
		t.Fatal("previous reader's consent kept")
	}
}

func TestReaderNeverSendsTokenToAnotherURL(t *testing.T) {
	tr, c, used := readerFixture(t)
	_ = tr.SetReader(t.Context(), "admin@acme.test", "jira-mcp")
	_, _ = tr.Tools(t.Context(), "jira-mcp") // cached
	var s v1.Server
	_ = c.Get(t.Context(), client.ObjectKey{Namespace: testNS, Name: "jira-mcp"}, &s)
	s.Spec.URL = "https://evil.test/mcp" // edited after consent
	_ = c.Update(t.Context(), &s)
	_, err := tr.Tools(t.Context(), "jira-mcp")
	if err == nil || !strings.Contains(err.Error(), "not bound") {
		t.Fatalf("err %v", err)
	}
	for _, u := range *used {
		if strings.HasPrefix(u, "https://evil.test") {
			t.Fatal("token sent to the new URL")
		}
	}
}

func TestDisconnectAndOffboardingEndConsentEvenWhenCached(t *testing.T) {
	tr, c, _ := readerFixture(t)
	_ = tr.SetReader(t.Context(), "admin@acme.test", "jira-mcp")
	_, _ = tr.Tools(t.Context(), "jira-mcp") // fills the cache
	g, _ := tr.Conns.Store.Get(t.Context(), "admin@acme.test", "jira")
	_, _ = tr.Conns.Revoke(t.Context(), g) // the reader disconnects
	if _, err := tr.Tools(t.Context(), "jira-mcp"); !errors.Is(err, ErrNoReader) {
		t.Fatalf("cached list used after disconnect: %v", err)
	}
	connect(t, tr.Conns, testAuthOnly(), "lee@acme.test", "lee")
	_ = tr.SetReader(t.Context(), "lee@acme.test", "jira-mcp")
	_, _ = tr.Tools(t.Context(), "jira-mcp")
	_ = c.Create(t.Context(), &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: OffboardedConfigMap, Namespace: testNS},
		Data: map[string]string{"emails": `["lee@acme.test"]`}}) // the format controller.OffboardedEmails reads
	if _, err := tr.Tools(t.Context(), "jira-mcp"); !errors.Is(err, ErrNoReader) {
		t.Fatalf("cached list used after offboarding: %v", err)
	}
}

func TestReconnectEndsConsent(t *testing.T) {
	tr, _, _ := readerFixture(t)
	_ = tr.SetReader(t.Context(), "admin@acme.test", "jira-mcp")
	connect(t, tr.Conns, testAuthOnly(), "admin@acme.test", "admin") // a new grant carries no consent
	if _, ok, _ := tr.Reader(t.Context(), "jira-mcp"); ok {
		t.Fatal("consent survived a reconnect")
	}
}

func TestConsentCannotChangeDuringARead(t *testing.T) {
	tr, _, _ := readerFixture(t)
	_ = tr.SetReader(t.Context(), "admin@acme.test", "jira-mcp")
	started, release := make(chan struct{}), make(chan struct{})
	tr.List = func(context.Context, *http.Client, string, string, string) ([]v1.Tool, error) {
		close(started)
		<-release
		return []v1.Tool{{Name: "stale"}}, nil
	}
	errc := make(chan error, 1)
	go func() { _, err := tr.Tools(t.Context(), "jira-mcp"); errc <- err }()
	<-started
	cleared := make(chan struct{})
	go func() { _ = tr.ClearReader(t.Context(), "jira-mcp"); close(cleared) }()
	select {
	case <-cleared:
		t.Fatal("consent changed while a read with it was running")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	<-errc
	<-cleared
	if _, err := tr.Tools(t.Context(), "jira-mcp"); !errors.Is(err, ErrNoReader) {
		t.Fatalf("reader still used after ClearReader: %v", err)
	}
	tr.mu.Lock()
	n := len(tr.cache)
	tr.mu.Unlock()
	if n != 0 {
		t.Fatal("a read started before ClearReader stored its result")
	}
}

func TestToolsHandlerReturnsNoToken(t *testing.T) {
	tr, _, _ := readerFixture(t)
	_ = tr.SetReader(t.Context(), "admin@acme.test", "jira-mcp")
	rec := httptest.NewRecorder()
	tr.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/v1/tools/jira-mcp", nil))
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if rec.Code != 200 || strings.Contains(rec.Body.String(), "at-admin") || body["reader"] != "admin@acme.test" {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}

// Disconnect during a read: the list read with the old grant is neither returned nor cached.
func TestRevokeDuringAReadDiscardsTheResult(t *testing.T) {
	tr, _, _ := readerFixture(t)
	_ = tr.SetReader(t.Context(), "admin@acme.test", "jira-mcp")
	tr.List = func(context.Context, *http.Client, string, string, string) ([]v1.Tool, error) {
		g, _ := tr.Conns.Store.Get(t.Context(), "admin@acme.test", "jira")
		_, _ = tr.Conns.Revoke(t.Context(), g) // not blocked: the read holds no grant lock while listing
		return []v1.Tool{{Name: "stale"}}, nil
	}
	if _, err := tr.Tools(t.Context(), "jira-mcp"); !errors.Is(err, ErrNoReader) {
		t.Fatalf("err %v", err)
	}
	if len(tr.cache) != 0 {
		t.Fatal("result of a revoked read cached")
	}
}

// Many reads, consent changes and renewals at once: run with -race; no read may return another reader's list.
func TestReaderConcurrentConsentChanges(t *testing.T) {
	tr, _, _ := readerFixture(t)
	tr.TTL = 0 // every call reads
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 10 {
				switch i % 4 {
				case 0:
					_ = tr.SetReader(t.Context(), "admin@acme.test", "jira-mcp")
				case 1:
					_ = tr.ClearReader(t.Context(), "jira-mcp")
				case 2:
					tr.Conns.CheckAll(t.Context(), true)
				default:
					if res, err := tr.Tools(t.Context(), "jira-mcp"); err == nil && res.Reader != "admin@acme.test" {
						t.Errorf("reader %q", res.Reader)
					}
				}
			}
		}()
	}
	wg.Wait()
	// After renewals interleaved with consent changes, the final consent is still in the grant.
	_ = tr.SetReader(t.Context(), "admin@acme.test", "jira-mcp")
	tr.Conns.CheckAll(t.Context(), true)
	if who, ok, err := tr.Reader(t.Context(), "jira-mcp"); !ok || who != "admin@acme.test" || err != nil {
		t.Fatalf("consent lost by renewal: %q %v %v", who, ok, err)
	}
}

func readerAPIFixture(t *testing.T) (http.Handler, *ToolReader, *Auth) {
	tr, c, _ := readerFixture(t)
	auth := &Auth{codec: Codec{Key: []byte("0123456789abcdef0123456789abcdef")}, ttl: time.Hour, Sessions: NewSessionStore()}
	return NewServer(auth, testSPA, LoginOptions{}, &Catalog{Client: c, Namespace: testNS, Conns: tr.Conns, Tools: tr}), tr, auth
}

func TestToolReaderAPI(t *testing.T) {
	h, tr, a := readerAPIFixture(t)
	if rec := do(t, h, "PUT", "/api/servers/jira-mcp/tool-reader", as(a, RoleViewer), map[string]string{}); rec.Code != http.StatusForbidden {
		t.Fatalf("viewer set: %d", rec.Code)
	}
	if rec := do(t, h, "PUT", "/api/servers/jira-mcp/tool-reader", as(a, RoleAdmin), map[string]string{}); rec.Code != http.StatusNoContent {
		t.Fatalf("admin set: %d %s", rec.Code, rec.Body)
	}
	if who, ok, _ := tr.Reader(t.Context(), "jira-mcp"); !ok || who != "admin@acme.test" {
		t.Fatalf("reader after PUT: %q", who)
	}
	if rec := do(t, h, "DELETE", "/api/servers/jira-mcp/tool-reader", as(a, RoleViewer), nil); rec.Code != http.StatusForbidden {
		t.Fatalf("viewer clear: %d", rec.Code)
	}
	if rec := do(t, h, "DELETE", "/api/servers/jira-mcp/tool-reader", as(a, RoleAdmin), nil); rec.Code != http.StatusNoContent {
		t.Fatalf("admin clear: %d", rec.Code)
	}
	if _, ok, _ := tr.Reader(t.Context(), "jira-mcp"); ok {
		t.Fatal("reader kept after DELETE")
	}
}
