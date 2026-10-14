// Package kindx is the only file that imports the kind library (sigs.k8s.io/kind, pinned v0.33.0). kind's Go API
// has no stability promise, so a kind upgrade touches only this file.
package kindx

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"sigs.k8s.io/kind/pkg/apis/config/v1alpha4"
	"sigs.k8s.io/kind/pkg/cluster"
	"sigs.k8s.io/kind/pkg/cluster/nodeutils"
	"sigs.k8s.io/kind/pkg/log"
	"sigs.k8s.io/yaml"
)

// The node label that marks a cluster created by `ambit local up` (spec 4.1 step 2, ruling S2).
const (
	Label      = "ambitproject.dev/created-by"
	LabelValue = "ambit-cli"
)

// Config returns kind/cluster.yaml with the cluster name, the host side of the two port mappings, and the
// ownership label on the control-plane node. The container side stays NodePort 30080 and 30443.
func Config(raw []byte, name string, httpPort, httpsPort int) ([]byte, error) {
	var c v1alpha4.Cluster
	if err := yaml.Unmarshal(raw, &c); err != nil {
		return nil, err
	}
	c.Name = name
	for i := range c.Nodes {
		n := &c.Nodes[i]
		if n.Role != v1alpha4.ControlPlaneRole {
			continue
		}
		if n.Labels == nil {
			n.Labels = map[string]string{}
		}
		n.Labels[Label] = LabelValue
		for j := range n.ExtraPortMappings {
			switch n.ExtraPortMappings[j].ContainerPort {
			case 30080:
				n.ExtraPortMappings[j].HostPort = int32(httpPort)
			case 30443:
				n.ExtraPortMappings[j].HostPort = int32(httpsPort)
			}
		}
	}
	return yaml.Marshal(c)
}

// Kind creates, deletes and inspects kind clusters on Docker.
type Kind struct{ p *cluster.Provider }

// New returns a Kind whose library logs go to w (nil: off).
func New(w io.Writer) *Kind {
	var l log.Logger = log.NoopLogger{}
	if w != nil {
		l = writerLogger{w}
	}
	return &Kind{p: cluster.NewProvider(cluster.ProviderWithDocker(), cluster.ProviderWithLogger(l))}
}

func (k *Kind) Exists(name string) (bool, error) {
	names, err := k.p.List()
	if err != nil {
		return false, err
	}
	for _, n := range names {
		if n == name {
			return true, nil
		}
	}
	return false, nil
}

// Create creates the cluster from cfg and waits up to 2 minutes for the control plane. kind adds the context
// kind-<name> to kubeconfig (empty: the default file) and makes it current; kind deletes a cluster whose create failed.
func (k *Kind) Create(name string, cfg []byte, kubeconfig string) error {
	return k.p.Create(name, cluster.CreateWithRawConfig(cfg), cluster.CreateWithKubeconfigPath(kubeconfig),
		cluster.CreateWithWaitForReady(2*time.Minute), cluster.CreateWithDisplayUsage(false), cluster.CreateWithDisplaySalutation(false))
}

// Delete deletes the cluster and its kubeconfig context.
func (k *Kind) Delete(name, kubeconfig string) error { return k.p.Delete(name, kubeconfig) }

// ControlPlane returns the name of the control-plane node, which is also its container name.
func (k *Kind) ControlPlane(name string) (string, error) {
	nodes, err := k.p.ListInternalNodes(name)
	if err != nil {
		return "", err
	}
	for _, n := range nodes {
		if r, _ := n.Role(); r == "control-plane" {
			return n.String(), nil
		}
	}
	return "", fmt.Errorf("kind cluster %q has no control-plane node", name)
}

// LoadArchive loads an image archive (docker save output) into every node.
func (k *Kind) LoadArchive(name, path string) error {
	nodes, err := k.p.ListInternalNodes(name)
	if err != nil {
		return err
	}
	for _, n := range nodes {
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		err = nodeutils.LoadImageArchive(n, f)
		f.Close()
		if err != nil {
			return fmt.Errorf("load %s into %s: %w", path, n.String(), err)
		}
	}
	return nil
}

// ContainerID returns the full Docker ID of a container.
func ContainerID(container string) (string, error) {
	out, err := exec.Command("docker", "inspect", "--format", "{{.Id}}", container).Output()
	if err != nil {
		return "", fmt.Errorf("docker inspect %s: %w", container, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// NodeLabelKey is the Docker label kind puts on every node container of a cluster; its value is the cluster name.
const NodeLabelKey = "io.x-k8s.kind.cluster"

// Container is one node container of a kind cluster, as Docker reports it.
type Container struct {
	ID      string    // full Docker ID
	Name    string    // container name = Kubernetes node name
	Created time.Time // Docker's creation time
}

// Containers returns every container that carries kind's label for the cluster, running or not. The ownership
// check uses it when the state file has no container ID yet (ruling S3).
func Containers(cluster string) ([]Container, error) {
	ids, err := exec.Command("docker", "ps", "-a", "--no-trunc", "--quiet", "--filter", "label="+NodeLabelKey+"="+cluster).Output()
	if err != nil {
		return nil, fmt.Errorf("docker ps: %w", err)
	}
	var cs []Container
	for _, id := range strings.Fields(string(ids)) {
		out, err := exec.Command("docker", "inspect", "--format", "{{.Id}}\t{{.Name}}\t{{.Created}}", id).Output()
		if err != nil {
			return nil, fmt.Errorf("docker inspect %s: %w", id, err)
		}
		c, err := parseInspect(strings.TrimSpace(string(out)))
		if err != nil {
			return nil, err
		}
		cs = append(cs, c)
	}
	return cs, nil
}

// parseInspect reads one line of `docker inspect --format '{{.Id}}\t{{.Name}}\t{{.Created}}'`. Docker prints the
// name with a leading slash and the time in RFC 3339 with nanoseconds.
func parseInspect(line string) (Container, error) {
	f := strings.Split(line, "\t")
	if len(f) != 3 {
		return Container{}, fmt.Errorf("docker inspect: unexpected output %q", line)
	}
	t, err := time.Parse(time.RFC3339Nano, f[2])
	if err != nil {
		return Container{}, fmt.Errorf("docker inspect: creation time %q: %w", f[2], err)
	}
	return Container{ID: f[0], Name: strings.TrimPrefix(f[1], "/"), Created: t}, nil
}

type writerLogger struct{ w io.Writer }

func (l writerLogger) Warn(m string)                     { fmt.Fprintln(l.w, m) }
func (l writerLogger) Warnf(f string, a ...interface{})  { fmt.Fprintf(l.w, f+"\n", a...) }
func (l writerLogger) Error(m string)                    { fmt.Fprintln(l.w, m) }
func (l writerLogger) Errorf(f string, a ...interface{}) { fmt.Fprintf(l.w, f+"\n", a...) }
func (l writerLogger) V(log.Level) log.InfoLogger        { return l }
func (l writerLogger) Info(m string)                     { fmt.Fprintln(l.w, m) }
func (l writerLogger) Infof(f string, a ...interface{})  { fmt.Fprintf(l.w, f+"\n", a...) }
func (l writerLogger) Enabled() bool                     { return true }
