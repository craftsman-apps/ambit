package console

import (
	"bufio"
	"crypto/tls"
	"crypto/x509"
	"net"
	"net/http/httptest"
	"path"
	"strconv"
	"strings"
	"testing"
	"time"

	v1 "github.com/craftsman-apps/ambit/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// fakeValkey answers GET and SCAN (one page) from a map, over real RESP on a TCP port.
func fakeValkey(t *testing.T, data map[string]string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	serveValkey(t, ln, data, "")
	return ln.Addr().String()
}

// serveValkey answers AUTH, GET and SCAN on ln, several commands per connection. With a password,
// every other command before a correct AUTH gets NOAUTH, as Valkey does.
func serveValkey(t *testing.T, ln net.Listener, data map[string]string, password string) {
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				r := bufio.NewReader(c)
				authed := password == ""
				for {
					v, err := respRead(r)
					if err != nil {
						return
					}
					args := v.([]any)
					switch {
					case args[0] == "AUTH":
						if args[len(args)-1] == password {
							authed = true
							c.Write([]byte("+OK\r\n"))
						} else {
							c.Write([]byte("-WRONGPASS invalid username-password pair\r\n"))
						}
					case !authed:
						c.Write([]byte("-NOAUTH Authentication required.\r\n"))
					case args[0] == "GET":
						if val, ok := data[args[1].(string)]; ok {
							c.Write([]byte("$" + strconv.Itoa(len(val)) + "\r\n" + val + "\r\n"))
						} else {
							c.Write([]byte("$-1\r\n"))
						}
					case args[0] == "SCAN":
						var keys []string
						for k := range data {
							if ok, _ := path.Match(args[3].(string), k); ok {
								keys = append(keys, k)
							}
						}
						var sb strings.Builder
						sb.WriteString("*2\r\n$1\r\n0\r\n*" + strconv.Itoa(len(keys)) + "\r\n")
						for _, k := range keys {
							sb.WriteString("$" + strconv.Itoa(len(k)) + "\r\n" + k + "\r\n")
						}
						c.Write([]byte(sb.String()))
					}
				}
			}()
		}
	}()
}

func TestBudgetViewReadsTodaysCounters(t *testing.T) {
	now := time.Date(2026, 9, 30, 13, 0, 0, 0, time.UTC)
	day := strconv.FormatInt(time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC).Unix(), 10)
	yesterday := strconv.FormatInt(time.Date(2026, 9, 29, 0, 0, 0, 0, time.UTC).Unix(), 10)
	addr := fakeValkey(t, map[string]string{
		"ambit-llm_group_eng_" + day:                             "7",
		"ambit-llm_group_eng_user_alice@acme.test_" + day:        "5",
		"ambit-llm_group_eng_user_bob_smith@acme.test_" + day:    "2",
		"ambit-llm_group_eng_user_alice@acme.test_" + yesterday:  "40",
		"ambit-llm_group_ml-research_user_mira@acme.test_" + day: "3",
	})
	scheme := runtime.NewScheme()
	_ = v1.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&v1.Budget{ObjectMeta: metav1.ObjectMeta{Name: "eng", Namespace: testNS},
		Spec: v1.BudgetSpec{Group: "eng", GroupRequestsPerDay: ptr.To[int64](100), UserRequestsPerDay: ptr.To[int64](10)}}).Build()
	b := &Budgets{Client: c, Namespace: testNS, Valkey: Valkey{Addr: addr}, Now: func() time.Time { return now }}
	got, err := b.views(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].GroupUsed != 7 || len(got[0].Users) != 2 || got[0].Users[0].Email != "alice@acme.test" || got[0].Users[0].Used != 5 ||
		got[0].Users[1].Email != "bob_smith@acme.test" || !got[0].ResetsAt.Equal(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("%+v", got)
	}
	// Valkey down: the view still answers, marked incomplete.
	b.Valkey = Valkey{Addr: "127.0.0.1:1"}
	got, err = b.views(t.Context())
	if err != nil || !got[0].CountsMissing {
		t.Fatalf("valkey down: %+v %v", got, err)
	}
}

// An external Valkey (e.g. ElastiCache) needs TLS and a password; a wrong password leaves the counts
// marked missing instead of failing the page.
func TestBudgetViewOverTLSWithPassword(t *testing.T) {
	srv := httptest.NewUnstartedServer(nil)
	srv.StartTLS() // only for its self-signed certificate
	defer srv.Close()
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: srv.TLS.Certificates})
	if err != nil {
		t.Fatal(err)
	}
	day := strconv.FormatInt(time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC).Unix(), 10)
	serveValkey(t, ln, map[string]string{"ambit-llm_group_eng_user_alice@acme.test_" + day: "4"}, "s3cret")
	roots := x509.NewCertPool()
	roots.AddCert(srv.Certificate())
	scheme := runtime.NewScheme()
	_ = v1.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&v1.Budget{ObjectMeta: metav1.ObjectMeta{Name: "eng", Namespace: testNS},
		Spec: v1.BudgetSpec{Group: "eng", UserRequestsPerDay: ptr.To[int64](10)}}).Build()
	now := func() time.Time { return time.Date(2026, 9, 30, 13, 0, 0, 0, time.UTC) }
	b := &Budgets{Client: c, Namespace: testNS, Now: now, Valkey: Valkey{Addr: ln.Addr().String(), Password: "s3cret", TLS: &tls.Config{RootCAs: roots, ServerName: "example.com"}}}
	got, err := b.views(t.Context())
	if err != nil || got[0].CountsMissing || len(got[0].Users) != 1 || got[0].Users[0].Used != 4 {
		t.Fatalf("%+v %v", got, err)
	}
	b.Valkey.Password = "wrong"
	if got, err := b.views(t.Context()); err != nil || !got[0].CountsMissing {
		t.Fatalf("wrong password: %+v %v", got, err)
	}
	b.Valkey.Password, b.Valkey.TLS = "s3cret", &tls.Config{ServerName: "example.com"} // system roots: the test CA is not trusted
	if got, err := b.views(t.Context()); err != nil || !got[0].CountsMissing {
		t.Fatalf("untrusted certificate: %+v %v", got, err)
	}
}

func TestBudgetViewShowsSpend(t *testing.T) {
	now := time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)
	day := strconv.FormatInt(time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC).Unix(), 10)
	addr := fakeValkey(t, map[string]string{
		"ambit-llm_spend_group_ml-research_" + day:                           "3120000",
		"ambit-llm_spend_group_ml-research_spend_user_mira@acme.test_" + day: "300000",
		"ambit-llm_spend_group_ml-research_spend_user_kim@acme.test_" + day:  "3",
		"ambit-llm_group_ml-research_user_mira@acme.test_" + day:             "4",
	})
	scheme := runtime.NewScheme()
	_ = v1.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&v1.Budget{ObjectMeta: metav1.ObjectMeta{Name: "ml", Namespace: testNS},
		Spec: v1.BudgetSpec{Group: "ml-research", GroupDollarsPerDay: "50", UserDollarsPerDay: "5", UserRequestsPerDay: ptr.To[int64](10)}}).Build()
	b := &Budgets{Client: c, Namespace: testNS, Valkey: Valkey{Addr: addr}, Now: func() time.Time { return now }}
	got, err := b.views(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	v := got[0]
	if v.GroupDollarLimit != "50" || v.UserDollarLimit != "5" || v.GroupSpent != 31.2 {
		t.Fatalf("%+v", v)
	}
	spent := map[string]float64{}
	used := map[string]int64{}
	for _, u := range v.Users {
		spent[u.Email], used[u.Email] = u.Spent, u.Used
	}
	if spent["mira@acme.test"] != 3 || spent["kim@acme.test"] != 0.00003 || used["mira@acme.test"] != 4 {
		t.Fatalf("users %+v", v.Users)
	}
}
