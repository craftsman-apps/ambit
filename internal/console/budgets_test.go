package console

import (
	"bufio"
	"net"
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
				v, err := respRead(r)
				if err != nil {
					return
				}
				args := v.([]any)
				switch args[0] {
				case "GET":
					if val, ok := data[args[1].(string)]; ok {
						c.Write([]byte("$" + strconv.Itoa(len(val)) + "\r\n" + val + "\r\n"))
					} else {
						c.Write([]byte("$-1\r\n"))
					}
				case "SCAN":
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
			}()
		}
	}()
	return ln.Addr().String()
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
	b := &Budgets{Client: c, Namespace: testNS, Valkey: addr, Now: func() time.Time { return now }}
	got, err := b.views(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].GroupUsed != 7 || len(got[0].Users) != 2 || got[0].Users[0].Email != "alice@acme.test" || got[0].Users[0].Used != 5 ||
		got[0].Users[1].Email != "bob_smith@acme.test" || !got[0].ResetsAt.Equal(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("%+v", got)
	}
	// Valkey down: the view still answers, marked incomplete.
	b.Valkey = "127.0.0.1:1"
	got, err = b.views(t.Context())
	if err != nil || !got[0].CountsMissing {
		t.Fatalf("valkey down: %+v %v", got, err)
	}
}
