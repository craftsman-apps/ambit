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
| `VertexAI` | `credentialsRef` (key file) or the gateway's Google identity; `vertexai` | yes, for the canonical model names |
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

A Google service-account key file for Vertex AI goes in `credentials.json`:

```sh
kubectl -n ambit-system create secret generic ambit-llm-gcp --from-file=credentials.json=<service-account key file>
```

## Fallback and several servers

A Model can use more than one server, and name other Models to use when its servers fail. Clients still ask for the
one model name; nothing changes for Models that set neither field.

```yaml
apiVersion: ambitproject.dev/v1alpha1
kind: Model
metadata: { name: gpt-5 }
spec:
  provider: Azure
  model: gpt-5
  credentialsRef: { name: ambit-llm-azure-eu }
  azure: { resourceName: eu-west, resourceType: OpenAI }
  # More servers for this same model. The settings above are the first one. Requests are shared by health and speed.
  additionalEndpoints:
    - credentialsRef: { name: ambit-llm-azure-us }
      azure: { resourceName: us-east, resourceType: OpenAI }
  # Other Models, in order, for when every server above fails.
  fallback: [claude-sonnet]
```

- **When a server fails.** A response with status 500 or higher (529 included) or 429 counts as a failure. The gateway
  tries the request again and sends it to another server, and skips the failed server for 10 seconds (longer after
  repeated failures). A person sees the answer of the next server, about 10 to 15 ms later. Other errors, such as 400
  or 401, are returned as they are.
- **`additionalEndpoints`** (at most 7) set the whole endpoint again: `baseURL`, `credentialsRef`, `azure`, `bedrock`
  or `vertexai`, as for the Model itself. Provider, model name, price and access rules are the Model's.
- **`fallback`** (at most 4) names Models in the same namespace. All servers of a fallback Model are used. A fallback
  Model's own `fallback` is not followed.
- **Access.** The gateway checks the AccessRules of the Model that answers. A person who may use the Model but not its
  fallback gets 403 when the Model fails; they are never answered by a model they may not use. The Model's `Ready`
  message lists who may use it but not a fallback.
- **Budgets and audit.** The Model that answers is charged at its own price and named in the audit record. The record
  also shows the name the client asked for. A fallback with no price is refused to groups with a dollar budget, as
  any Model without a price is.
- **Routers.** agentgateway v1.6.0 cannot nest these lists. Through a [ModelRouter](routing.md), one server of a Model
  with these fields answers (measured: always the same one, not chosen by health), without fallback, and the
  router's condition says `FallbackIgnored`. Do not put such a Model behind a router yet.

What the gateway (agentgateway v1.6.0) cannot do yet:

- **A timeout or a refused connection** is not handled inside the same request. The request ends in 504 or 503; the
  following requests go to the next server. Most client libraries try again, which hides it.
- **A stream that breaks after the first byte** is not retried, and the server is not marked as failed. A stream that
  fails before the first byte is retried like any request.
- **A person who is told 403 for a fallback** sees the text `authorization failed`, not the provider's error.
- Retries apply to Models that have these fields only. Ambit writes one retry policy for the whole LLM route
  (`ambit-llm-retry`) with `2 x (servers - 1)` attempts for the longest list, at most 8.

## Add a model in the console

An admin adds a model on the Models page with **Add model**. The form asks for the name, the provider, the model ID
and the settings the provider needs. For the API key, choose one of three ways:

- **Enter the key here.** Not for Bedrock access keys or Vertex AI key files. The console stores the key in the Secret
  `ambit-llm-<name>` and the Model names it.
- **Use an existing Secret.** Choose one of the Secrets named `ambit-llm-*` in the namespace; the console lists names
  only and never reads their contents. Create one as shown in [Secrets](#secrets).
- **Use the gateway's cloud identity.** Bedrock and Vertex AI only.

If you enter the key, read this first:

> The key is stored in the Kubernetes Secret `ambit-llm-<name>` in the namespace `<namespace>`. Anyone who can read
> Secrets in this namespace can read the key. The console never shows the key again. For production, create the
> Secret with your secret manager (for example External Secrets) and choose **Use an existing Secret**.

In GitOps mode the console also says:

> The key is not put in Git. The console writes the Secret to the cluster now; the pull request adds only the Model.
> If you rebuild the cluster from Git, create the Secret again.

The console never overwrites a Secret: if `ambit-llm-<name>` exists, choose **Use an existing Secret** or another
name. If the pull request is closed without merging, or a step fails after the Secret was written, the Models page
lists the key under **Unfinished additions**, with **Remove key**. To change or rotate a stored key, update the
Secret with `kubectl`.

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

## Google Vertex AI

Gemini and Claude models served by your Google Cloud project. `model` is the Vertex AI model name: Gemini
`gemini-2.5-pro`; Claude `claude-sonnet-4-5@20250929`. agentgateway also accepts `claude-sonnet-4-5-20250929`,
but only the `@` form has a price suggestion. Claude must be enabled in Model Garden and is offered only in some
regions. `region` defaults to `global`; `us`, `eu` or a region such as `europe-west4` also work. Legacy
domain-scoped project IDs (`example.com:my-project`) are accepted.

```yaml
apiVersion: ambitproject.dev/v1alpha1
kind: Model
metadata: { name: gemini-2-5-pro-vertex }
spec:
  provider: VertexAI
  model: gemini-2.5-pro
  credentialsRef: { name: ambit-llm-gcp }    # leave out to use the gateway's Google identity
  vertexai: { projectId: acme-ml, region: europe-west4 }
---
apiVersion: ambitproject.dev/v1alpha1
kind: Model
metadata: { name: claude-sonnet-vertex }
spec:
  provider: VertexAI
  model: claude-sonnet-4-5@20250929
  vertexai: { projectId: acme-ml }           # region global
```

### Vertex AI with the gateway's Google identity (GKE Workload Identity)

Without `credentialsRef`, the gateway uses Google's default credentials. On GKE, turn on Workload Identity for the
cluster and the node pool, then choose one of:

- **Direct**: grant `roles/aiplatform.user` to the gateway's Kubernetes principal,
  `principal://iam.googleapis.com/projects/<project number>/locations/global/workloadIdentityPools/<project>.svc.id.goog/subject/ns/ambit-system/sa/<gateway ServiceAccount>`.
  No annotation is needed.
- **Through a Google service account**: grant it `roles/aiplatform.user`; grant `roles/iam.workloadIdentityUser`
  on it to `serviceAccount:<project>.svc.id.goog[ambit-system/<gateway ServiceAccount>]`; and annotate the
  gateway's ServiceAccount through the chart:

  ```yaml
  gateway:
    serviceAccount:
      annotations:
        iam.gke.io/gcp-service-account: ambit-vertex@acme-ml.iam.gserviceaccount.com
  ```

Find the gateway ServiceAccount's name with the command in the Bedrock section. Restart the gateway pods after
changing the identity. A missing or wrong identity shows only when a request is made, as a Google auth error.

## Self-hosted servers

Use provider `OpenAI` with `baseURL` (include the API path, usually `/v1`). `formats` lists the APIs the server
implements; the gateway converts other request formats to one of them. See the field descriptions with
`kubectl explain models.spec.formats`.

## Not supported

Azure Entra ID credentials (managed identity, workload identity); AWS assume-role; Bedrock guardrails; Vertex AI
Model Garden partner models other than Claude; Vertex AI embeddings.
Azure, Bedrock and Vertex AI Models cannot be the classifier of a type-LLM `ModelRouter` (ambit-router calls
OpenAI-compatible endpoints only); they can be its route targets and default.
