// Package local holds the logic of `ambit local up`, `local down` and `local password` (spec 4.1, 4.2).
package local

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/craftsman-apps/ambit/internal/cli/kindx"
	"github.com/craftsman-apps/ambit/internal/cli/out"
)

// State is the local state file <user config dir>/ambit/local/<name>.json, proof (a) of ownership.
type State struct {
	Name        string    `json:"name"`
	Status      string    `json:"status"`                // "creating" until step 8 finished, then "ready"
	ContainerID string    `json:"containerID,omitempty"` // empty until `kind create` returned (ruling S3)
	Created     time.Time `json:"created"`
}

// Store reads and writes state files in Dir.
type Store struct{ Dir string }

// DefaultStore is the store in os.UserConfigDir(): ~/Library/Application Support on macOS, ~/.config on Linux,
// %AppData% on Windows.
func DefaultStore() (Store, error) {
	d, err := os.UserConfigDir()
	if err != nil {
		return Store{}, err
	}
	return Store{Dir: filepath.Join(d, "ambit", "local")}, nil
}

// validName is stricter than kind's own rule (^[a-z0-9.-]+$, kind v0.33.0): it also requires a letter or digit
// first and at most 63 characters, so a name can never leave the store directory or become a hidden file.
var validName = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{0,62}$`)

func checkName(name string) error {
	if validName.MatchString(name) {
		return nil
	}
	return out.Usage(fmt.Sprintf("%q is not a valid cluster name", name),
		"a cluster name has lower-case letters, digits, '-' and '.', starts with a letter or digit, and has at most 63 characters",
		"pass a valid name, for example --name ambit-try")
}

func (s Store) Path(name string) string { return filepath.Join(s.Dir, name+".json") }

// Read returns nil, nil when there is no state file.
func (s Store) Read(name string) (*State, error) {
	if err := checkName(name); err != nil {
		return nil, err
	}
	b, err := os.ReadFile(s.Path(name))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var st State
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, fmt.Errorf("%s: %w", s.Path(name), err)
	}
	return &st, nil
}

// Write replaces the state file atomically (write to a temporary file, then rename).
func (s Store) Write(st State) error {
	if err := checkName(st.Name); err != nil {
		return err
	}
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.Dir, st.Name+".*.tmp")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), s.Path(st.Name))
}

func (s Store) Remove(name string) error {
	if err := checkName(name); err != nil {
		return err
	}
	err := os.Remove(s.Path(name))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// Cluster is what the ownership check reads from Docker and from the cluster's Kubernetes API.
type Cluster interface {
	Exists(name string) (bool, error)
	ControlPlane(name string) (string, error) // control-plane node name = container name
	ContainerID(container string) (string, error)
	// NodeLabels reads node labels through the context kind-<name>; an error means the API did not answer.
	NodeLabels(ctx context.Context, name string) (map[string]map[string]string, error)
	// Containers lists the containers with kind's label io.x-k8s.kind.cluster=<name> (ruling S3).
	Containers(name string) ([]kindx.Container, error)
}

type Verdict int

const (
	Absent   Verdict = iota // no kind cluster with the name
	Owned                   // both proofs hold
	NotOwned                // a cluster exists, but at least one proof is missing
)

type Ownership struct {
	Verdict Verdict
	Reason  string // for NotOwned: what the CLI found
	State   *State
}

// CheckOwnership is the one function that decides whether the CLI may repair, delete or read a local cluster
// (spec 4.1 step 2). Both proofs are required: the state file with the control-plane container's ID, and the
// node label on the node that runs in that container.
//
// A state file with status "creating" and no container ID means `local up` stopped while kind was creating the
// cluster (ruling S3). The container is then found by kind's own container label; it is accepted only if it is
// the only one, it was created after the state file, and its node carries the ownership label. The ID is then
// written to the state file, so repair continues with both proofs.
func CheckOwnership(ctx context.Context, store Store, c Cluster, name string) (Ownership, error) {
	if err := checkName(name); err != nil {
		return Ownership{}, err
	}
	exists, err := c.Exists(name)
	if err != nil {
		return Ownership{}, err
	}
	st, err := store.Read(name)
	if err != nil {
		return Ownership{}, err
	}
	if !exists {
		return Ownership{Verdict: Absent, State: st}, nil
	}
	notOwned := func(format string, a ...any) (Ownership, error) {
		return Ownership{Verdict: NotOwned, Reason: fmt.Sprintf(format, a...), State: st}, nil
	}
	if st == nil {
		return notOwned("there is no state file %s", store.Path(name))
	}
	recovered := false
	if st.ContainerID == "" {
		if st.Status != "creating" {
			return notOwned("the state file %s has no container ID", store.Path(name))
		}
		cs, err := c.Containers(name)
		if err != nil {
			return notOwned("its node containers cannot be read: %v", err)
		}
		if len(cs) != 1 {
			return notOwned("the state file %s has no container ID, and %d containers carry the label %s=%s (ambit local up creates one)",
				store.Path(name), len(cs), kindx.NodeLabelKey, name)
		}
		if !cs[0].Created.After(st.Created) {
			return notOwned("container %s was created at %s, before the state file %s (%s); the clocks may differ", short(cs[0].ID),
				cs[0].Created.UTC().Format(time.RFC3339), store.Path(name), st.Created.UTC().Format(time.RFC3339))
		}
		st.ContainerID, recovered = cs[0].ID, true
	}
	cp, err := c.ControlPlane(name)
	if err != nil {
		return notOwned("its control-plane node cannot be read: %v", err)
	}
	id, err := c.ContainerID(cp)
	if err != nil {
		return notOwned("its control-plane container cannot be read: %v", err)
	}
	if id != st.ContainerID {
		return notOwned("its control-plane container is %s, but the state file %s names %s", short(id), store.Path(name), short(st.ContainerID))
	}
	labels, err := c.NodeLabels(ctx, name)
	if err != nil {
		return notOwned("its Kubernetes API does not answer, so the node label cannot be read (state file %s, container %s)", store.Path(name), short(id))
	}
	if labels[cp][kindx.Label] != kindx.LabelValue {
		return notOwned("node %s has no label %s=%s", cp, kindx.Label, kindx.LabelValue)
	}
	if recovered {
		if err := store.Write(*st); err != nil {
			return Ownership{}, err
		}
	}
	return Ownership{Verdict: Owned, State: st}, nil
}

// Refusal is the error every local command prints for a cluster it does not own. It never suggests a delete
// command: the docs page explains how a person checks and removes such a cluster.
func (o Ownership) Refusal(name string) *out.Error {
	other := "ambit-try"
	if name == other {
		other = "ambit-try-2"
	}
	return &out.Error{
		What: fmt.Sprintf("a kind cluster named %q exists and was not created by ambit local up", name),
		Why:  o.Reason,
		Next: "ambit local up --name " + other + " (or see https://ambitproject.dev/docs/local-up#a-cluster-that-ambit-did-not-create)",
		Code: out.ExitFailed,
	}
}

func short(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}
