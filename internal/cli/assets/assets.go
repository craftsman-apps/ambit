// Package assets gives the CLI the embedded files of its own version: the kind cluster file, the quick-start
// values, the third-party CRD bundle and the local test model.
package assets

import (
	"bytes"
	"io/fs"

	"sigs.k8s.io/yaml"

	"github.com/craftsman-apps/ambit/deploy"
	"github.com/craftsman-apps/ambit/kind"
)

// KindClusterYAML is kind/cluster.yaml.
func KindClusterYAML() []byte { return kind.ClusterYAML }

// TestModel is deploy/local/test-model.yaml.
func TestModel() []byte { return must(fs.ReadFile(deploy.Files, "local/test-model.yaml")) }

// QuickstartValues is deploy/quickstart.yaml with console.url set to consoleURL.
func QuickstartValues(consoleURL string) (map[string]any, error) {
	v := map[string]any{}
	if err := yaml.Unmarshal(must(fs.ReadFile(deploy.Files, "quickstart.yaml")), &v); err != nil {
		return nil, err
	}
	console, _ := v["console"].(map[string]any)
	if console == nil {
		console = map[string]any{}
		v["console"] = console
	}
	console["url"] = consoleURL
	return v, nil
}

// CRDBundle is deploy/crds/*.yaml concatenated in file-name order, each followed by "\n---\n": byte for byte the
// crds.yaml that .github/workflows/release.yml publishes.
func CRDBundle() []byte {
	entries := must(fs.ReadDir(deploy.Files, "crds"))
	var b bytes.Buffer
	for _, e := range entries {
		b.Write(must(fs.ReadFile(deploy.Files, "crds/"+e.Name())))
		b.WriteString("\n---\n")
	}
	return b.Bytes()
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err) // embedded files are fixed at build time; a missing one is a build bug
	}
	return v
}
