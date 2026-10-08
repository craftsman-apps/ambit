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
		{"provider Ambit cannot configure", `apiVersion: ambitproject.dev/v1alpha1
kind: Model
metadata: { name: gemini }
spec: { provider: Ollama, model: llama3, credentialsRef: { name: ambit-llm-x } }`, "Unsupported value"},
		{"azure without settings", `apiVersion: ambitproject.dev/v1alpha1
kind: Model
metadata: { name: gpt }
spec: { provider: Azure, model: gpt-4.1, credentialsRef: { name: ambit-llm-azure } }`, "azure settings are required with provider Azure"},
		{"azure settings on another provider", `apiVersion: ambitproject.dev/v1alpha1
kind: Model
metadata: { name: gpt }
spec: { provider: OpenAI, model: gpt-4.1, credentialsRef: { name: ambit-llm-x }, azure: { resourceName: acme-openai, resourceType: OpenAI } }`, "azure settings are required with provider Azure"},
		{"bedrock settings on another provider", `apiVersion: ambitproject.dev/v1alpha1
kind: Model
metadata: { name: claude }
spec: { provider: Anthropic, model: claude-sonnet-4-5, credentialsRef: { name: ambit-llm-x }, bedrock: { region: eu-west-1 } }`, "bedrock settings are only for provider Bedrock"},
		{"azure without credentials", `apiVersion: ambitproject.dev/v1alpha1
kind: Model
metadata: { name: gpt }
spec: { provider: Azure, model: gpt-4.1, azure: { resourceName: acme-openai, resourceType: OpenAI } }`, "credentialsRef is required"},
		{"foundry without project", `apiVersion: ambitproject.dev/v1alpha1
kind: Model
metadata: { name: gpt }
spec: { provider: Azure, model: gpt-4.1, credentialsRef: { name: ambit-llm-azure }, azure: { resourceName: acme-ai, resourceType: Foundry } }`, "projectName is required for Foundry and only there"},
		{"project on an OpenAI resource", `apiVersion: ambitproject.dev/v1alpha1
kind: Model
metadata: { name: gpt }
spec: { provider: Azure, model: gpt-4.1, credentialsRef: { name: ambit-llm-azure }, azure: { resourceName: acme-openai, resourceType: OpenAI, projectName: p } }`, "projectName is required for Foundry and only there"},
		{"baseURL with bedrock", `apiVersion: ambitproject.dev/v1alpha1
kind: Model
metadata: { name: claude }
spec: { provider: Bedrock, model: us.anthropic.claude-sonnet-4-5-20250929-v1:0, baseURL: "http://proxy:8000/v1" }`, "baseURL is not used with Azure, Bedrock or VertexAI"},
		{"formats with azure", `apiVersion: ambitproject.dev/v1alpha1
kind: Model
metadata: { name: gpt }
spec: { provider: Azure, model: gpt-4.1, credentialsRef: { name: ambit-llm-azure }, azure: { resourceName: acme-openai, resourceType: OpenAI }, formats: [Completions] }`, "formats describe a self-hosted server"},
		{"azure resource name with a dot", `apiVersion: ambitproject.dev/v1alpha1
kind: Model
metadata: { name: gpt }
spec: { provider: Azure, model: gpt-4.1, credentialsRef: { name: ambit-llm-azure }, azure: { resourceName: acme.openai, resourceType: OpenAI } }`, "should match"},
		{"azure resource name too long", `apiVersion: ambitproject.dev/v1alpha1
kind: Model
metadata: { name: gpt }
spec: { provider: Azure, model: gpt-4.1, credentialsRef: { name: ambit-llm-azure }, azure: { resourceName: a123456789012345678901234567890123456789012345678901234567890123b, resourceType: OpenAI } }`, "may not be more than 64"},
		{"bedrock region with capitals", `apiVersion: ambitproject.dev/v1alpha1
kind: Model
metadata: { name: claude }
spec: { provider: Bedrock, model: anthropic.claude-sonnet-4-5-20250929-v1:0, bedrock: { region: EU-WEST-1 } }`, "should match"},
		{"bedrock key secret outside the name rule", `apiVersion: ambitproject.dev/v1alpha1
kind: Model
metadata: { name: claude }
spec: { provider: Bedrock, model: anthropic.claude-sonnet-4-5-20250929-v1:0, credentialsRef: { name: aws-keys } }`, "ambit-llm-<something>"},
		{"vertexai without settings", `apiVersion: ambitproject.dev/v1alpha1
kind: Model
metadata: { name: gemini }
spec: { provider: VertexAI, model: gemini-2.5-pro }`, "vertexai settings are required with provider VertexAI"},
		{"vertexai settings on another provider", `apiVersion: ambitproject.dev/v1alpha1
kind: Model
metadata: { name: gemini }
spec: { provider: Gemini, model: gemini-2.5-pro, credentialsRef: { name: ambit-llm-x }, vertexai: { projectId: acme-ml } }`, "vertexai settings are required with provider VertexAI"},
		{"baseURL with vertexai", `apiVersion: ambitproject.dev/v1alpha1
kind: Model
metadata: { name: gemini }
spec: { provider: VertexAI, model: gemini-2.5-pro, vertexai: { projectId: acme-ml }, baseURL: "http://proxy:8000/v1" }`, "baseURL is not used with Azure, Bedrock or VertexAI"},
		{"formats with vertexai", `apiVersion: ambitproject.dev/v1alpha1
kind: Model
metadata: { name: gemini }
spec: { provider: VertexAI, model: gemini-2.5-pro, vertexai: { projectId: acme-ml }, formats: [Completions] }`, "formats describe a self-hosted server"},
		{"gemini without credentials", `apiVersion: ambitproject.dev/v1alpha1
kind: Model
metadata: { name: gemini }
spec: { provider: Gemini, model: gemini-2.5-pro }`, "credentialsRef is required (only Bedrock and VertexAI"},
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

// Providers that agentgateway serves with an API key alone (checked with a server-side dry run on v1.6.0).
func TestKeyOnlyProvidersAreValid(t *testing.T) {
	for _, p := range []string{"Anthropic", "Gemini", "OpenAI", "Openrouter", "Mistral", "Cohere", "XAI", "Deepseek", "Meta", "Groq",
		"Cerebras", "TogetherAI", "Fireworks", "Deepinfra", "Baseten", "Huggingface"} {
		doc := "apiVersion: ambitproject.dev/v1alpha1\nkind: Model\nmetadata: { name: m }\nspec: { provider: " + p +
			", model: some-model, credentialsRef: { name: ambit-llm-x } }"
		if probs := check(t, doc); len(probs) != 0 {
			t.Errorf("provider %s refused: %v", p, probs)
		}
	}
}

func TestCloudProviderModelsAreValid(t *testing.T) {
	for name, doc := range map[string]string{
		"azure openai":          `{ provider: Azure, model: gpt-4.1, credentialsRef: { name: ambit-llm-azure }, azure: { resourceName: ab, resourceType: OpenAI, apiVersion: 2025-04-01-preview } }`,
		"azure foundry":         `{ provider: Azure, model: gpt-4.1, credentialsRef: { name: ambit-llm-azure }, azure: { resourceName: a12345678901234567890123456789012345678901234567890123456789012b, resourceType: Foundry, projectName: research } }`,
		"bedrock with keys":     `{ provider: Bedrock, model: us.anthropic.claude-sonnet-4-5-20250929-v1:0, credentialsRef: { name: ambit-llm-aws }, bedrock: { region: eu-west-1 } }`,
		"bedrock with identity": `{ provider: Bedrock, model: anthropic.claude-sonnet-4-5-20250929-v1:0 }`,
		"longest settings":      `{ provider: Azure, model: gpt-4.1, credentialsRef: { name: ambit-llm-azure }, azure: { resourceName: acme-ai, resourceType: Foundry, apiVersion: ` + strings.Repeat("v", 64) + `, projectName: ` + strings.Repeat("p", 256) + ` } }`,
		"longest region":        `{ provider: Bedrock, model: anthropic.claude-sonnet-4-5-20250929-v1:0, bedrock: { region: ` + strings.Repeat("r", 63) + ` } }`,
	} {
		if p := check(t, "apiVersion: ambitproject.dev/v1alpha1\nkind: Model\nmetadata: { name: m }\nspec: "+doc+"\n"); len(p) != 0 {
			t.Errorf("%s: %v", name, p)
		}
	}
	// One character over each limit.
	for name, doc := range map[string]string{
		"apiVersion":                 `{ provider: Azure, model: gpt-4.1, credentialsRef: { name: ambit-llm-azure }, azure: { resourceName: acme-openai, resourceType: OpenAI, apiVersion: ` + strings.Repeat("v", 65) + ` } }`,
		"projectName":                `{ provider: Azure, model: gpt-4.1, credentialsRef: { name: ambit-llm-azure }, azure: { resourceName: acme-ai, resourceType: Foundry, projectName: ` + strings.Repeat("p", 257) + ` } }`,
		"region":                     `{ provider: Bedrock, model: anthropic.claude-sonnet-4-5-20250929-v1:0, bedrock: { region: ` + strings.Repeat("r", 64) + ` } }`,
		"resourceName one character": `{ provider: Azure, model: gpt-4.1, credentialsRef: { name: ambit-llm-azure }, azure: { resourceName: a, resourceType: OpenAI } }`,
	} {
		p := check(t, "apiVersion: ambitproject.dev/v1alpha1\nkind: Model\nmetadata: { name: m }\nspec: "+doc+"\n")
		if len(p) == 0 || !(strings.Contains(p[0].String(), "may not be more than") || strings.Contains(p[0].String(), "should be at least") || strings.Contains(p[0].String(), "should match")) {
			t.Errorf("%s over its limit: %v", name, p)
		}
	}
}

func TestVertexAIModels(t *testing.T) {
	doc := func(settings string, creds bool) string {
		c := ""
		if creds {
			c = "credentialsRef: { name: ambit-llm-gcp }, "
		}
		return "apiVersion: ambitproject.dev/v1alpha1\nkind: Model\nmetadata: { name: m }\nspec: { provider: VertexAI, model: gemini-2.5-pro, " + c + "vertexai: " + settings + " }\n"
	}
	for name, d := range map[string]string{
		"gemini with a key file": doc(`{ projectId: acme-ml }`, true),
		"claude on the identity": "apiVersion: ambitproject.dev/v1alpha1\nkind: Model\nmetadata: { name: m }\nspec: { provider: VertexAI, model: claude-sonnet-4-5@20250929, vertexai: { projectId: acme-ml, region: europe-west4 } }\n",
		"shortest project":       doc(`{ projectId: acme-x }`, false),
		"longest project":        doc(`{ projectId: a`+strings.Repeat("b", 28)+`c }`, false),
		"domain-scoped project":  doc(`{ projectId: "example.com:my-project" }`, false),
		"longest region":         doc(`{ projectId: acme-ml, region: `+strings.Repeat("r", 63)+` }`, false),
		"multi-region":           doc(`{ projectId: acme-ml, region: eu }`, false),
	} {
		if p := check(t, d); len(p) != 0 {
			t.Errorf("%s: %v", name, p)
		}
	}
	for name, d := range map[string]string{
		"project too short":       doc(`{ projectId: acme1 }`, false),
		"project too long":        doc(`{ projectId: a`+strings.Repeat("b", 29)+`c }`, false),
		"project uppercase":       doc(`{ projectId: Acme-ml }`, false),
		"project leading digit":   doc(`{ projectId: 1acme-ml }`, false),
		"project trailing hyphen": doc(`{ projectId: acme-ml- }`, false),
		"region too long":         doc(`{ projectId: acme-ml, region: `+strings.Repeat("r", 64)+` }`, false),
		"region uppercase":        doc(`{ projectId: acme-ml, region: EU }`, false),
		"region leading digit":    doc(`{ projectId: acme-ml, region: 4west }`, false),
		"region lone hyphen":      doc(`{ projectId: acme-ml, region: "-" }`, false),
		"no project":              doc(`{ region: eu }`, false),
	} {
		p := check(t, d)
		if len(p) == 0 {
			t.Errorf("%s: accepted", name)
		}
	}
}
