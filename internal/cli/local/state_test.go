package local

import (
	"context"
	"errors"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/craftsman-apps/ambit/internal/cli/kindx"
)

type fakeCluster struct {
	exists     bool
	cp         string
	id         string
	labels     map[string]map[string]string
	apiErr     error
	containers []kindx.Container
}

func (f *fakeCluster) Exists(string) (bool, error)         { return f.exists, nil }
func (f *fakeCluster) ControlPlane(string) (string, error) { return f.cp, nil }
func (f *fakeCluster) ContainerID(string) (string, error)  { return f.id, nil }
func (f *fakeCluster) NodeLabels(context.Context, string) (map[string]map[string]string, error) {
	return f.labels, f.apiErr
}
func (f *fakeCluster) Containers(string) ([]kindx.Container, error) { return f.containers, nil }

const cid = "4f1c2e7d9a0b5566778899aabbccddeeff00112233445566778899aabbccddee"

func owned() *fakeCluster {
	return &fakeCluster{exists: true, cp: "ambit-control-plane", id: cid,
		labels: map[string]map[string]string{"ambit-control-plane": {kindx.Label: kindx.LabelValue}}}
}

func TestStoreRoundTrip(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	if st, err := s.Read("ambit"); st != nil || err != nil {
		t.Fatalf("Read missing = %v, %v", st, err)
	}
	want := State{Name: "ambit", Status: "creating", Created: time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)}
	if err := s.Write(want); err != nil {
		t.Fatal(err)
	}
	got, err := s.Read("ambit")
	if err != nil || *got != want {
		t.Fatalf("Read = %+v, %v", got, err)
	}
	if fi, _ := os.Stat(s.Path("ambit")); runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", fi.Mode().Perm())
	}
	if err := s.Remove("ambit"); err != nil {
		t.Fatal(err)
	}
	if err := s.Remove("ambit"); err != nil {
		t.Fatalf("second Remove: %v", err)
	}
}

func TestCheckOwnership(t *testing.T) {
	ready := &State{Name: "ambit", Status: "ready", ContainerID: cid}
	noID := &State{Name: "ambit", Status: "ready"}
	cases := []struct {
		name    string
		state   *State
		cluster *fakeCluster
		want    Verdict
		reason  string
	}{
		{"missing cluster, no state", nil, &fakeCluster{}, Absent, ""},
		{"missing cluster, stale state", ready, &fakeCluster{}, Absent, ""},
		{"both proofs", ready, owned(), Owned, ""},
		{"interrupted after create (status creating, ID written)", &State{Name: "ambit", Status: "creating", ContainerID: cid}, owned(), Owned, ""},
		{"label but no state file", nil, owned(), NotOwned, "there is no state file"},
		{"ready state without container ID", noID, owned(), NotOwned, "has no container ID"},
		{"state names another container", &State{Name: "ambit", ContainerID: "0000000000001111"}, owned(), NotOwned, "but the state file"},
		{"API does not answer", ready, func() *fakeCluster { c := owned(); c.apiErr = errors.New("connection refused"); return c }(), NotOwned, "does not answer"},
		{"no label", ready, func() *fakeCluster {
			c := owned()
			c.labels = map[string]map[string]string{"ambit-control-plane": {}}
			return c
		}(), NotOwned, "has no label"},
		{"label on another node only", ready, func() *fakeCluster {
			c := owned()
			c.labels = map[string]map[string]string{"ambit-worker": {kindx.Label: kindx.LabelValue}, "ambit-control-plane": {}}
			return c
		}(), NotOwned, "has no label"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := Store{Dir: t.TempDir()}
			if c.state != nil {
				if err := s.Write(*c.state); err != nil {
					t.Fatal(err)
				}
			}
			o, err := CheckOwnership(context.Background(), s, c.cluster, "ambit")
			if err != nil {
				t.Fatal(err)
			}
			if o.Verdict != c.want || !strings.Contains(o.Reason, c.reason) {
				t.Fatalf("verdict %v reason %q; want %v containing %q", o.Verdict, o.Reason, c.want, c.reason)
			}
		})
	}
}

func TestRefusalNeverSuggestsDelete(t *testing.T) {
	e := Ownership{Verdict: NotOwned, Reason: "node ambit-control-plane has no label"}.Refusal("ambit")
	if e.What != `a kind cluster named "ambit" exists and was not created by ambit local up` {
		t.Fatalf("what = %q", e.What)
	}
	if strings.Contains(strings.ToLower(e.Next+e.Why), "delete") || !strings.HasPrefix(e.Next, "ambit local up --name ambit-try") {
		t.Fatalf("next = %q", e.Next)
	}
}

// Ruling S3: `local up` stopped between writing the state file (status creating, no container ID) and writing
// the container ID. The container is found by kind's label and accepted only on all three conditions.
func TestCheckOwnershipRecoversContainerID(t *testing.T) {
	stateTime := time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)
	node := kindx.Container{ID: cid, Name: "ambit-control-plane", Created: stateTime.Add(3 * time.Second)}
	cases := []struct {
		name    string
		cluster func() *fakeCluster
		want    Verdict
		reason  string
	}{
		{"accepted", func() *fakeCluster { c := owned(); c.containers = []kindx.Container{node}; return c }, Owned, ""},
		{"label missing", func() *fakeCluster {
			c := owned()
			c.containers = []kindx.Container{node}
			c.labels = map[string]map[string]string{"ambit-control-plane": {}}
			return c
		}, NotOwned, "has no label"},
		{"container older than the state file", func() *fakeCluster {
			c := owned()
			old := node
			old.Created = stateTime.Add(-time.Hour)
			c.containers = []kindx.Container{old}
			return c
		}, NotOwned, "before the state file"},
		{"two containers found", func() *fakeCluster {
			c := owned()
			other := kindx.Container{ID: "99" + cid[2:], Name: "ambit-worker", Created: node.Created}
			c.containers = []kindx.Container{node, other}
			return c
		}, NotOwned, "2 containers carry the label io.x-k8s.kind.cluster=ambit"},
		{"no container found", func() *fakeCluster { return owned() }, NotOwned, "0 containers"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := Store{Dir: t.TempDir()}
			if err := s.Write(State{Name: "ambit", Status: "creating", Created: stateTime}); err != nil {
				t.Fatal(err)
			}
			o, err := CheckOwnership(context.Background(), s, c.cluster(), "ambit")
			if err != nil {
				t.Fatal(err)
			}
			if o.Verdict != c.want || !strings.Contains(o.Reason, c.reason) {
				t.Fatalf("verdict %v reason %q; want %v containing %q", o.Verdict, o.Reason, c.want, c.reason)
			}
			st, err := s.Read("ambit")
			if err != nil {
				t.Fatal(err)
			}
			wantID := ""
			if c.want == Owned {
				wantID = cid // written, so the next command finds both proofs without this path
			}
			if st.ContainerID != wantID || st.Status != "creating" || !st.Created.Equal(stateTime) {
				t.Fatalf("state after check = %+v, want container ID %q, status creating, time unchanged", st, wantID)
			}
		})
	}
}

// Ruling S3: the refusal for a container older than the state file names the state file and both times, and
// its next step is another cluster name, not a delete.
func TestOlderContainerRefusalNamesBothTimes(t *testing.T) {
	stateTime := time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)
	c := owned()
	c.containers = []kindx.Container{{ID: cid, Name: "ambit-control-plane", Created: stateTime.Add(-time.Hour)}}
	s := Store{Dir: t.TempDir()}
	if err := s.Write(State{Name: "ambit", Status: "creating", Created: stateTime}); err != nil {
		t.Fatal(err)
	}
	o, err := CheckOwnership(context.Background(), s, c, "ambit")
	if err != nil {
		t.Fatal(err)
	}
	e := o.Refusal("ambit")
	for _, want := range []string{s.Path("ambit"), "2026-10-14T08:00:00Z", "2026-10-14T09:00:00Z"} {
		if !strings.Contains(e.Why, want) {
			t.Errorf("why %q does not contain %q", e.Why, want)
		}
	}
	if !strings.HasPrefix(e.Next, "ambit local up --name ") || strings.Contains(strings.ToLower(e.Next+e.Why), "delete") {
		t.Errorf("next = %q", e.Next)
	}
}
