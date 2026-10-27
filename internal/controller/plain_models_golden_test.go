package controller

import (
	"encoding/json"
	"os"
	"testing"

	v1 "github.com/craftsman-apps/ambit/api/v1alpha1"
)

// A Model without fallback and additionalEndpoints must compile to the same objects as before those fields existed.
// The golden file was written by Compile at the commit before them (f6710b2). Rewrite it only for a change that is
// meant to alter plain Models: AMBIT_WRITE_GOLDEN=1 go test ./internal/controller -run PlainModels
func TestPlainModelsCompileByteIdentical(t *testing.T) {
	openai := model("gpt-5", "gpt-5")
	openai.Spec.Provider = "OpenAI"
	selfHosted := model("gemma", "google/gemma-4-31b-it")
	selfHosted.Spec.Provider, selfHosted.Spec.BaseURL = "OpenAI", "http://vllm.models.svc:8000/v1"
	selfHosted.Spec.Formats = []v1.ModelFormat{"Completions", "Responses"}
	azure := model("azure-gpt", "gpt-5-deploy")
	azure.Spec.Provider, azure.Spec.Azure = "Azure", &v1.AzureSettings{ResourceName: "eu-west", ResourceType: "OpenAI", APIVersion: "2025-01-01"}
	bedrock := model("bedrock-sonnet", "us.anthropic.claude-sonnet-4-5-20250929-v1:0")
	bedrock.Spec.Provider, bedrock.Spec.Bedrock, bedrock.Spec.CredentialsRef = "Bedrock", &v1.BedrockSettings{Region: "eu-central-1"}, nil
	vertex := model("vertex-gemini", "gemini-2.5-pro")
	vertex.Spec.Provider, vertex.Spec.VertexAI = "VertexAI", &v1.VertexAISettings{ProjectID: "acme-ml-prod", Region: "europe-west4"}
	vertex.Spec.CredentialsRef = &v1.SecretRef{Name: "ambit-llm-vertex"}
	wild := model("openrouter-free", "*:free")
	models := []v1.Model{openai, selfHosted, azure, bedrock, vertex, wild, model("claude", "anthropic/claude-sonnet-4.5")}
	rules := []v1.AccessRule{
		rule("eng", []string{"eng"}, false, "gpt-5", "gemma", "azure-gpt", "claude"),
		rule("ml", []string{"ml-research"}, false, "bedrock-sonnet", "vertex-gemini"),
		rule("all", nil, true, "openrouter-free"),
	}
	got, err := json.MarshalIndent(Compile(models, nil, rules, route).Objects, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	const golden = "testdata/plain-models.golden.json"
	if os.Getenv("AMBIT_WRITE_GOLDEN") != "" {
		if err := os.WriteFile(golden, append(got, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if string(append(got, '\n')) != string(want) {
		t.Fatalf("plain Models no longer compile as before; compare with %s (git diff after AMBIT_WRITE_GOLDEN=1)", golden)
	}
}
