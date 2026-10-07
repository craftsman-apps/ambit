package validate

import (
	"os"
	"strings"
	"testing"
)

func check(t *testing.T, doc string) []Problem {
	t.Helper()
	v, err := New()
	if err != nil {
		t.Fatal(err)
	}
	p, _, err := v.File(t.Context(), strings.NewReader(doc))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestTheTestCatalogsAreValid(t *testing.T) {
	v, err := New()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"../../test/llm/objects.yaml", "../../test/mcp/objects.yaml"} {
		r, err := os.Open(f)
		if err != nil {
			t.Fatal(err)
		}
		p, n, err := v.File(t.Context(), r)
		r.Close()
		if err != nil || len(p) != 0 || n == 0 {
			t.Fatalf("%s: %d objects, problems %v, err %v", f, n, p, err)
		}
	}
}

func TestProblemsTheAPIServerWouldReport(t *testing.T) {
	for _, tc := range []struct{ name, doc, want string }{
		{"tag instead of digest", `apiVersion: ambitproject.dev/v1alpha1
kind: Server
metadata: { name: time }
spec: { image: "ghcr.io/stacklok/dockyard/uvx/mcp-server-time:2026.8.18" }`, "sha256"},
		{"typo in a field", `apiVersion: ambitproject.dev/v1alpha1
kind: AccessRule
metadata: { name: eng }
spec: { groups: [eng], servers: [{ name: time, tool: [get_current_time] }] }`, "unknown field spec.servers[0].tool"},
		{"server name too long (CEL on metadata)", `apiVersion: ambitproject.dev/v1alpha1
kind: Server
metadata: { name: this-name-is-far-too-long-for-a-tool-prefix-x }
spec: { image: "ghcr.io/a/b@sha256:ab8255cf7bf2bdad14086b356790d2d61d06cab221b85950805427be9d06d949" }`, "at most 40 characters"},
		{"rule that grants nothing (CEL)", `apiVersion: ambitproject.dev/v1alpha1
kind: AccessRule
metadata: { name: empty }
spec: { groups: [eng] }`, "at least one model or server"},
		{"empty tool list", `apiVersion: ambitproject.dev/v1alpha1
kind: AccessRule
metadata: { name: none }
spec: { groups: [eng], servers: [{ name: time, tools: [] }] }`, "should have at least 1 items"},
		{"dollar cap above the largest", `apiVersion: ambitproject.dev/v1alpha1
kind: Budget
metadata: { name: eng }
spec: { group: eng, groupDollarsPerDay: "42949.68" }`, "at most 42949.67"},
		{"dollar cap of zero", `apiVersion: ambitproject.dev/v1alpha1
kind: Budget
metadata: { name: eng }
spec: { group: eng, userDollarsPerDay: "0" }`, "greater than 0"},
		{"dollar cap with too many decimals", `apiVersion: ambitproject.dev/v1alpha1
kind: Budget
metadata: { name: eng }
spec: { group: eng, groupDollarsPerDay: "1.000001" }`, "should match"},
		{"budget with no limit", `apiVersion: ambitproject.dev/v1alpha1
kind: Budget
metadata: { name: eng }
spec: { group: eng }`, "set at least one limit"},
		{"formats without a baseURL", `apiVersion: ambitproject.dev/v1alpha1
kind: Model
metadata: { name: gemma }
spec: { provider: OpenAI, model: gemma, credentialsRef: { name: ambit-llm-x }, formats: [Completions] }`, "formats describe a self-hosted server"},
		{"formats with another provider", `apiVersion: ambitproject.dev/v1alpha1
kind: Model
metadata: { name: router }
spec: { provider: Openrouter, model: x, credentialsRef: { name: ambit-llm-x }, baseURL: "http://proxy:8000/v1", formats: [Messages] }`, "formats describe a self-hosted server"},
		{"duplicate format", `apiVersion: ambitproject.dev/v1alpha1
kind: Model
metadata: { name: llama }
spec: { provider: OpenAI, model: llama, credentialsRef: { name: ambit-llm-x }, baseURL: "http://vllm:8000/v1", formats: [Completions, Completions] }`, "Duplicate value"},
		{"unknown format", `apiVersion: ambitproject.dev/v1alpha1
kind: Model
metadata: { name: llama }
spec: { provider: OpenAI, model: llama, credentialsRef: { name: ambit-llm-x }, baseURL: "http://vllm:8000/v1", formats: [Realtime] }`, "Unsupported value"},
		{"namespace in Git", `apiVersion: ambitproject.dev/v1alpha1
kind: AccessRule
metadata: { name: eng, namespace: ambit-system }
spec: { groups: [eng], models: [gemma] }`, "metadata.namespace should be left out"},
	} {
		p := check(t, tc.doc)
		if len(p) == 0 || !strings.Contains(p[0].String(), tc.want) {
			t.Errorf("%s: want a problem containing %q, got %v", tc.name, tc.want, p)
		}
	}
}

func TestOtherObjectsAreSkipped(t *testing.T) {
	if p := check(t, "apiVersion: v1\nkind: ConfigMap\nmetadata: { name: x }\n"); len(p) != 0 {
		t.Fatalf("%v", p)
	}
}

func TestDollarOnlyBudgetIsValid(t *testing.T) {
	if p := check(t, "apiVersion: ambitproject.dev/v1alpha1\nkind: Budget\nmetadata: { name: ml }\nspec: { group: ml-research, groupDollarsPerDay: \"0.00005\", userDollarsPerDay: \"42949.67\" }\n"); len(p) != 0 {
		t.Fatalf("%v", p)
	}
}
