# Model providers

A `Model` names one model a client may request, the provider that serves it, and where the gateway finds the
provider's credentials. The credentials stay in a Secret named `ambit-llm-<something>` in Ambit's namespace;
the `Model` only names it, so the `Model` is safe to keep in Git.

| `provider` | Needs | Price suggestion |
|---|---|---|
| `Anthropic`, `Gemini`, `OpenAI`, `Openrouter`, `Mistral`, `Cohere`, `XAI`, `Deepseek`, `Meta`, `Groq`, `Cerebras`, `TogetherAI`, `Fireworks`, `Deepinfra`, `Baseten` | `credentialsRef` (API key) | yes, when the model is in LiteLLM's price list |
| `Huggingface` | `credentialsRef` (API key) | no (not in LiteLLM's price list) |
| `Azure` | `credentialsRef` (API key) and `azure` | only when the deployment name equals a model name in the list |
| `Bedrock` | `credentialsRef` (AWS keys) or the gateway's AWS identity; `bedrock` optional | yes, by model or inference-profile ID |
| `OpenAI` with `baseURL` | a self-hosted, OpenAI-compatible server (vLLM, Ollama, LiteLLM ...) | no |

A Model without a price shows tokens only in Usage, and a group with a dollar budget is refused it. Set the price
in the console (Models) or in `spec.price`.

## Secrets

An API key goes in the `Authorization` entry:

```sh
kubectl -n ambit-system create secret generic ambit-llm-openai --from-literal=Authorization=<api key>
```

AWS keys for Bedrock go in `accessKey`, `secretKey` and, for temporary credentials, `sessionToken`:

```sh
kubectl -n ambit-system create secret generic ambit-llm-aws \
  --from-literal=accessKey=<access key id> --from-literal=secretKey=<secret access key>
```

## Azure OpenAI and Azure AI Foundry

`model` is the **deployment name**. The gateway sends the key in the `api-key` header, as Azure documents for
API keys.

```yaml
apiVersion: ambitproject.dev/v1alpha1
kind: Model
metadata: { name: gpt-4-1 }
spec:
  provider: Azure
  model: gpt-4.1                 # the deployment name
  credentialsRef: { name: ambit-llm-azure }
  azure:
    resourceName: acme-openai    # acme-openai.openai.azure.com
    resourceType: OpenAI
    apiVersion: v1               # optional; v1 is the default
  price: { inputPerMillionTokens: "2", outputPerMillionTokens: "8" }
---
apiVersion: ambitproject.dev/v1alpha1
kind: Model
metadata: { name: gpt-4-1-foundry }
spec:
  provider: Azure
  model: gpt-4.1
  credentialsRef: { name: ambit-llm-azure }
  azure:
    resourceName: acme-ai        # acme-ai.services.ai.azure.com
    resourceType: Foundry
    projectName: research        # required for Foundry, refused for OpenAI
```

Deployment names rarely match a model name in the price list, so set the price yourself; until then, groups
with a dollar budget are refused the model.

## Amazon Bedrock

`model` is the model ID or inference-profile ID. Inference profiles depend on the region: `us.` profiles work from
US regions, `eu.` from EU regions. `region` defaults to `us-east-1`. A wrong ID or region shows as Bedrock's
error when a request is made, not when the Model is created.

```yaml
apiVersion: ambitproject.dev/v1alpha1
kind: Model
metadata: { name: claude-sonnet-bedrock }
spec:
  provider: Bedrock
  model: eu.anthropic.claude-sonnet-4-5-20250929-v1:0
  credentialsRef: { name: ambit-llm-aws }    # leave out to use the gateway's AWS identity
  bedrock: { region: eu-west-1 }
```

### Bedrock with the gateway's AWS identity

Without `credentialsRef`, the gateway uses the AWS SDK's default credentials: on EKS, IRSA or EKS Pod Identity.
The role needs `bedrock:InvokeModel` and `bedrock:InvokeModelWithResponseStream` on the models it may call.

- **IRSA**: set the role on the gateway's ServiceAccount through the chart, and name that ServiceAccount
  (namespace `ambit-system`) in the role's trust policy. Find its name with
  `kubectl -n ambit-system get pods -l gateway.networking.k8s.io/gateway-name=ambit -o jsonpath='{.items[0].spec.serviceAccountName}'`.

  ```yaml
  gateway:
    serviceAccount:
      annotations:
        eks.amazonaws.com/role-arn: arn:aws:iam::123456789012:role/ambit-bedrock
  ```

- **EKS Pod Identity**: install the EKS Pod Identity Agent (EKS Auto Mode has it), and create a Pod Identity
  association for the same ServiceAccount. The chart needs no change.

After changing the identity, restart the gateway pods so they pick it up:
`kubectl -n ambit-system rollout restart deployment -l gateway.networking.k8s.io/gateway-name=ambit`.
A missing or wrong identity shows only when a request is made, as an AWS error.

## Self-hosted servers

Use provider `OpenAI` with `baseURL` (include the API path, usually `/v1`). `formats` lists the APIs the server
implements; the gateway converts other request formats to one of them. See the field descriptions with
`kubectl explain models.spec.formats`.

## Not supported

Vertex AI; Azure Entra ID credentials (managed identity, workload identity); AWS assume-role; Bedrock guardrails.
Azure and Bedrock Models cannot be the classifier of a type-LLM `ModelRouter` (ambit-router calls
OpenAI-compatible endpoints only); they can be its route targets and default.
