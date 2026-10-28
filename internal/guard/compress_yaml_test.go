package guard

import (
	"reflect"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

func parseYAML(t *testing.T, s string) any {
	t.Helper()
	var v any
	if err := yaml.Unmarshal([]byte(s), &v); err != nil {
		t.Fatalf("invalid YAML: %v\n%s", err, s)
	}
	return v
}

const helmValues = `# Default values
replicas: 2   
image:
  repository: nginx

  tag: "1.27"   # pinned
script: |
  echo one

  echo two   
env:
  - name: A
    value: "1"
  - name: B
    value: "2"
`

func TestCompressYAMLLosslessKeepsEveryValue(t *testing.T) {
	for name, in := range map[string]string{"pods": podYAML(50), "helm values": helmValues} {
		out, _, ok := compressYAML(in, false)
		if !ok {
			t.Fatalf("%s: not detected", name)
		}
		if !reflect.DeepEqual(parseYAML(t, out), parseYAML(t, in)) {
			t.Errorf("%s: values changed:\n%s", name, out)
		}
		if len(out) > len(in) || (name == "helm values" && len(out) == len(in)) { // kubectl output has no blank lines to remove
			t.Errorf("%s: not smaller", name)
		}
	}
	out, _, _ := compressYAML(helmValues, false)
	if !strings.Contains(out, "# Default values") || !strings.Contains(out, "echo one\n\n  echo two   \n") {
		t.Errorf("comments or block scalar changed:\n%s", out)
	}
}

func TestCompressYAMLShortenDropsCommentsAndManagedFields(t *testing.T) {
	in := podYAML(50)
	out, omitted, ok := compressYAML(in, true)
	if !ok || omitted != 50 || strings.Count(out, "[ambit: managedFields omitted here]") != 50 {
		t.Fatalf("omitted %d ok %v", omitted, ok)
	}
	if strings.Contains(out, "fieldsV1") || !strings.Contains(out, "name: web-49") || !strings.Contains(out, "namespace: default") {
		t.Errorf("wrong content kept:\n%.600s", out)
	}
	if r := float64(len(out)) / float64(len(in)); r > 0.7 {
		t.Errorf("ratio %.2f", r)
	}
	parseYAML(t, out) // still valid
	c, _, _ := compressYAML(helmValues, true)
	if strings.Contains(c, "# Default values") {
		t.Error("full-line comment kept")
	}
}

func TestCompressYAMLNotYAMLIsUnknown(t *testing.T) {
	for _, in := range []string{htmlPage, "just some prose.\nmore prose.\n\nThree.\nFour.\nFive.\n"} {
		if _, _, ok := compressYAML(in, true); ok {
			t.Errorf("compressed %.40q", in)
		}
	}
}

func FuzzCompressYAML(f *testing.F) {
	f.Add(helmValues)
	f.Fuzz(func(t *testing.T, in string) {
		if out, _, ok := compressYAML(in, true); ok && len(out) > len(in)+64*len(strings.Split(in, "\n")) {
			t.Fatalf("grew a lot")
		}
	})
}
